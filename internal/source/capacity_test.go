package source

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/hashicorp/go-hclog"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"

	"github.com/nesty92/fleetci/internal/fleet"
	"github.com/nesty92/fleetci/internal/proxy"
)

// fakeBackend has free[class] VMs of room per class.
type fakeBackend struct {
	mu      sync.Mutex
	free    map[string]int
	n       int
	deleted []string
	made    map[string]fleet.VM
}

type fakeHeld struct {
	b     *fakeBackend
	class string
	used  bool
}

func (h *fakeHeld) Class() string { return h.class }

func (h *fakeHeld) Acquire(context.Context) (fleet.VM, error) {
	if h.used {
		return fleet.VM{}, errors.New("reservation already used")
	}
	h.used = true
	return h.b.vm(h.class), nil
}

func (h *fakeHeld) Release() {
	if h.used {
		return
	}
	h.used = true
	h.b.mu.Lock()
	h.b.free[h.class]++
	h.b.mu.Unlock()
}

func (b *fakeBackend) vm(class string) fleet.VM {
	b.mu.Lock()
	b.n++
	id := class + "-" + strconv.Itoa(b.n)
	vm := fleet.VM{ID: id, Class: class, Instance: "i-" + id,
		Conn: proxy.NewConn(func(context.Context) (proxy.Target, error) {
			return proxy.Target{Info: provider.ConnectInfo{ID: id}}, nil
		}, hclog.NewNullLogger())}
	if b.made == nil {
		b.made = map[string]fleet.VM{}
	}
	b.made[id] = vm
	b.mu.Unlock()
	return vm
}

func (b *fakeBackend) Reserve(class string) (held, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.free[class] == 0 {
		return nil, fleet.ErrNoRoom
	}
	b.free[class]--
	return &fakeHeld{b: b, class: class}, nil
}

func (b *fakeBackend) Create(_ context.Context, class string) (fleet.VM, error) {
	return b.vm(class), nil
}

func (b *fakeBackend) Delete(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deleted = append(b.deleted, id)
	return nil
}

func testCapacity() (*capacity, *fakeBackend) {
	b := &fakeBackend{free: map[string]int{"small": 1}}
	classes := map[string]fleet.Class{
		"small": {MaxInstances: 2, Overflow: "cloud"},
		"cloud": {MaxInstances: 3},
	}
	return newCapacity(b, classes, "small"), b
}

func TestClassesAreSortedWithTheirSlots(t *testing.T) {
	c, _ := testCapacity()
	got, def, err := c.Classes(t.Context())
	want := []Class{{Name: "cloud", Slots: 3}, {Name: "small", Slots: 2, Overflow: "cloud"}}
	if err != nil || def != "small" || !slices.Equal(got, want) {
		t.Errorf("Classes: %v %q %v; want %v small", got, def, err, want)
	}
}

func TestAReservationIsUsedOnce(t *testing.T) {
	ctx := t.Context()
	c, b := testCapacity()
	r, err := c.Reserve(ctx, "small")
	if err != nil || r.Class != "small" || r.ID == "" {
		t.Fatalf("Reserve: %+v %v", r, err)
	}
	if _, err := c.Reserve(ctx, "small"); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("second Reserve: %v, want ErrNoRoom", err)
	}
	vm, err := c.Acquire(ctx, r)
	if err != nil || vm.Class != "small" || vm.Instance != "i-"+vm.ID {
		t.Fatalf("Acquire: %+v %v", vm, err)
	}
	if _, err := c.Acquire(ctx, r); err == nil {
		t.Error("a reservation acquired twice")
	}
	if err := c.Release(ctx, r); err == nil {
		t.Error("an acquired reservation released")
	}
	if b.free["small"] != 0 {
		t.Errorf("room back after Acquire: %d", b.free["small"])
	}
}

func TestReleaseGivesTheRoomBack(t *testing.T) {
	ctx := t.Context()
	c, b := testCapacity()
	r, err := c.Reserve(ctx, "small")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Release(ctx, r); err != nil {
		t.Fatal(err)
	}
	if b.free["small"] != 1 {
		t.Errorf("free %d after Release, want 1", b.free["small"])
	}
	if _, err := c.Acquire(ctx, r); err == nil {
		t.Error("a released reservation acquired")
	}
}

func TestConnectKnowsAVMUntilItIsDeleted(t *testing.T) {
	ctx := t.Context()
	c, b := testCapacity()
	vm, err := c.Create(ctx, "cloud")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := c.Conn(vm.ID)
	if err != nil || conn == nil || conn != b.made[vm.ID].Conn {
		t.Fatalf("Conn: %v, not the vm's own connection", err)
	}
	if err := c.Delete(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Conn(vm.ID); err == nil {
		t.Error("Conn of a deleted VM")
	}
	if !slices.Equal(b.deleted, []string{vm.ID}) {
		t.Errorf("deleted %v", b.deleted)
	}
}

package fleet

import (
	"context"
	"errors"
	"testing"

	"github.com/hashicorp/go-hclog"
)

func TestReserveHoldsRoomUntilAcquiredOrReleased(t *testing.T) {
	onlyWakes(t)
	b, _ := hostPools(t, Resources{CPUs: 2, MemoryMB: 4096}, map[string]Class{"small": onHostClass(1, 2048, 8)})
	ctx := context.Background()

	r1, err := b.Reserve("small")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := b.Reserve("small")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reserve("small"); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("third reservation on a 2-cpu host: %v, want ErrNoRoom", err)
	}
	if _, ok := startsNow(t, b, "small"); ok {
		t.Fatal("a job started in reserved room")
	}

	// An unused reservation gives its room back, once.
	r1.Release()
	r1.Release()
	r3, err := b.Reserve("small")
	if err != nil {
		t.Fatalf("after a release: %v", err)
	}
	if _, err := b.Reserve("small"); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("a double release freed room twice: %v", err)
	}

	// Acquire takes the VM in the reserved room; Release after it is a
	// no-op, and the VM's Delete gives the room back.
	vm, err := r2.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if vm.Class != "small" {
		t.Fatalf("class %q", vm.Class)
	}
	if _, err := r2.Acquire(ctx); err == nil {
		t.Fatal("a reservation was acquired twice")
	}
	r2.Release()
	if _, err := b.Reserve("small"); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("Release after Acquire freed room: %v", err)
	}
	if err := b.Delete(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	r4, err := b.Reserve("small")
	if err != nil {
		t.Fatalf("after the vm's delete: %v", err)
	}
	r3.Release()
	r4.Release()
}

func TestReserveStandsBehindANominatedJob(t *testing.T) {
	b, clk, smalls, big := fourSmallsRunning(t, smallAndBig())
	ctx := context.Background()

	// Before the nomination, small jobs fill the room the big one waits for,
	// reserved ones as the others.
	if err := b.Delete(ctx, smalls[0].ID); err != nil {
		t.Fatal(err)
	}
	r, err := b.Reserve("small")
	if err != nil {
		t.Fatalf("before the nomination: %v", err)
	}
	r.Release()

	// Once nominated, the room is held for it: a reservation doesn't take
	// it.
	clk.Step(defaultNominateAfter)
	if _, ok := startsNow(t, b, "small"); ok {
		t.Fatal("a job took room held for the nominated one")
	}
	if err := b.Delete(ctx, smalls[0].ID); err != nil {
		t.Fatal(err)
	}
	if r, err := b.Reserve("small"); err == nil {
		r.Release()
		t.Fatal("a reservation jumped ahead of the waiting big job")
	}
	for _, vm := range smalls[1:] {
		if err := b.Delete(ctx, vm.ID); err != nil {
			t.Fatal(err)
		}
	}
	got(t, big, "big")
}

func TestReserveFallsBackToTheOverflow(t *testing.T) {
	own, cloud := fakePool("4vcpu-8gb", 1, "4vcpu-8gb-aws"), fakePool("4vcpu-8gb-aws", 1, "")
	b := &Backend{log: hclog.NewNullLogger(), vms: map[string]*pool{},
		pools: map[string][]*pool{own.name: {own}, cloud.name: {cloud}}}

	r1, err := b.Reserve("4vcpu-8gb")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := b.Reserve("4vcpu-8gb")
	if err != nil {
		t.Fatal(err)
	}
	vm, err := r2.Acquire(context.Background())
	if err != nil || vm.Class != "4vcpu-8gb-aws" {
		t.Fatalf("second reservation: %v %v, want the overflow", vm.Class, err)
	}
	if _, err := b.Reserve("4vcpu-8gb"); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("both full: %v, want ErrNoRoom", err)
	}
	if _, err := b.Reserve("nope"); err == nil || errors.Is(err, ErrNoRoom) {
		t.Fatalf("unknown class: %v", err)
	}
	r1.Release()
}

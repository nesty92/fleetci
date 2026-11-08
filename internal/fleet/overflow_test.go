package fleet

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"

	"gitlab.com/gitlab-org/fleeting/taskscaler"
)

// fakeScaler hands out a VM at once, or makes the caller wait until its
// context ends when full.
type fakeScaler struct {
	taskscaler.Taskscaler
	mu   sync.Mutex
	free int
}

type fakeAcq struct{ taskscaler.Acquisition }

func (fakeAcq) InstanceID() string { return "i-1" }

func (f *fakeScaler) Acquire(ctx context.Context, _ string, _ ...taskscaler.AcquireOption) (taskscaler.Acquisition, error) {
	f.mu.Lock()
	if f.free == 0 {
		f.mu.Unlock()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.free--
	f.mu.Unlock()
	return fakeAcq{}, nil
}

func (f *fakeScaler) Release(string) {
	f.mu.Lock()
	f.free++
	f.mu.Unlock()
}

func fakePool(name string, max int, overflow string) *pool {
	return &pool{name: name, class: Class{MaxInstances: max, Overflow: overflow}, conns: noConns(),
		ts: &fakeScaler{free: max}, timeout: 100 * time.Millisecond}
}

func TestOverflowTakesJobsWhileTheClassIsFull(t *testing.T) {
	own, cloud := fakePool("4vcpu-8gb", 2, "4vcpu-8gb-aws"), fakePool("4vcpu-8gb-aws", 1, "")
	b := &Backend{log: hclog.NewNullLogger(), vms: map[string]*pool{},
		pools: map[string][]*pool{own.name: {own}, cloud.name: {cloud}}}
	ctx := context.Background()

	var got []string
	var first VM
	for i := range 3 {
		vm, err := b.Create(ctx, "4vcpu-8gb")
		if err != nil {
			t.Fatalf("job %d: %v", i+1, err)
		}
		if i == 0 {
			first = vm
		}
		got = append(got, vm.Class)
	}
	if strings.Join(got, " ") != "4vcpu-8gb 4vcpu-8gb 4vcpu-8gb-aws" {
		t.Fatalf("classes %v: want two on the class, then the overflow", got)
	}

	// Both full: the job waits for its own class, not the overflow.
	_, err := b.Create(ctx, "4vcpu-8gb")
	if err == nil || !strings.Contains(err.Error(), "acquiring a 4vcpu-8gb vm") {
		t.Fatalf("both full: %v, want a wait on 4vcpu-8gb that times out", err)
	}
	if own.jobs != 2 || cloud.jobs != 1 {
		t.Fatalf("jobs %d/%d after a failed wait, want 2/1", own.jobs, cloud.jobs)
	}

	// A job ends on the class: the next goes there again.
	if err := b.Delete(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if vm, err := b.Create(ctx, "4vcpu-8gb"); err != nil || vm.Class != "4vcpu-8gb" {
		t.Fatalf("after a release: %v %v, want the class itself", vm.Class, err)
	}

	// Both full again; a waiting job takes the first room, here the
	// overflow's.
	overflowPoll = 10 * time.Millisecond
	defer func() { overflowPoll = time.Second }()
	own.timeout = 2 * time.Second
	var cloudVM string
	for id, p := range b.vms {
		if p == cloud {
			cloudVM = id
		}
	}
	done := make(chan VM)
	go func() {
		vm, err := b.Create(ctx, "4vcpu-8gb")
		if err != nil {
			t.Error(err)
		}
		done <- vm
	}()
	time.Sleep(50 * time.Millisecond)
	if err := b.Delete(ctx, cloudVM); err != nil {
		t.Fatal(err)
	}
	if vm := <-done; vm.Class != "4vcpu-8gb-aws" {
		t.Fatalf("waiting job got %q, want the overflow's freed room", vm.Class)
	}

	// Jobs asking for the overflow class itself use it as any class.
	if _, err := b.Create(ctx, "4vcpu-8gb-aws"); err == nil {
		t.Fatal("the full overflow class took another job")
	}
}

func TestCheckOverflow(t *testing.T) {
	classes := map[string]Class{
		"own":   {Overflow: "cloud"},
		"cloud": {},
		"self":  {Overflow: "self"},
		"lost":  {Overflow: "nowhere"},
		"chain": {Overflow: "own"},
		"x86":   {Overflow: "arm", Connector: Connector{Arch: "amd64"}},
		"arm":   {Connector: Connector{Arch: "arm64"}},
		"maybe": {Overflow: "arm"},
	}
	for name, want := range map[string]string{
		"own": "", "cloud": "", "self": "the class itself", "lost": "doesn't exist", "chain": "one step only",
		"x86": `is amd64, its overflow "arm" arm64`, "maybe": "",
	} {
		err := checkOverflow(name, classes)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v, want %q", name, err, want)
		}
	}
}

func TestAcquireTimeoutDefaults(t *testing.T) {
	for _, c := range []struct {
		class Class
		want  time.Duration
	}{
		{Class{}, 10 * time.Minute},
		{Class{Overflow: "cloud"}, 50 * time.Minute},
		{Class{Overflow: "cloud", AcquireTimeout: Duration(5 * time.Minute)}, 5 * time.Minute},
		{Class{AcquireTimeout: Duration(time.Minute)}, time.Minute},
	} {
		if got := c.class.acquireTimeout(); got != c.want {
			t.Errorf("%+v: %v, want %v", c.class, got, c.want)
		}
	}
}

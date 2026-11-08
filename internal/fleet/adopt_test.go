package fleet

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"

	"gitlab.com/gitlab-org/fleeting/fleeting"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"gitlab.com/gitlab-org/fleeting/taskscaler"
	"gitlab.com/gitlab-org/fleeting/taskscaler/storage"
)

// storedScaler is scaler with its state kept in st and up wrapped by
// adoptUp, as startPool does for a class with a state directory.
func storedScaler(t *testing.T, group provider.InstanceGroup, c Class, st storage.Storage, adopt func(key, instance string)) (taskscaler.Taskscaler, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	up := adoptUp(func(taskscaler.Taskscaler, taskscaler.UpFuncInstance) error { return nil }, adopt)
	ts, err := taskscaler.New(ctx, initialised{group, provider.ProviderInfo{ID: "cloud", MaxSize: 10}},
		append(scalerOptions(c, provider.Settings{}, up, st, hclog.NewNullLogger()),
			taskscaler.WithAcquireDelay(50*time.Millisecond),
			taskscaler.WithUpdateInterval(100*time.Millisecond),
			taskscaler.WithUpdateIntervalWhenExpecting(100*time.Millisecond),
			taskscaler.WithShutdownDeletionInterval(10*time.Millisecond),
			// A VM left to the next taskscaler keeps its heartbeat loop in
			// the one that left it, until the process ends; off here, so
			// the leak check sees only what the test is about.
			taskscaler.WithHeartbeatFunc(nil))...)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			// Held jobs are left to the next taskscaler: don't wait for them.
			ctx, done := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer done()
			ts.Shutdown(ctx)
			cancel()
		})
	}
	t.Cleanup(stop)
	return ts, stop
}

type adoptions struct {
	mu   sync.Mutex
	keys []string
}

func (a *adoptions) add(key, instance string) {
	a.mu.Lock()
	a.keys = append(a.keys, key+"@"+instance)
	a.mu.Unlock()
}

func (a *adoptions) wait(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		a.mu.Lock()
		ok := slices.Contains(a.keys, want)
		a.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never adopted (got %q)", want, a.keys)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestARestartAdoptsTheVMAndJobItWasLeft(t *testing.T) {
	group := newCloud()
	st, err := storage.NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := Class{MaxInstances: 2, MaxUseCount: 1}

	first, stop := storedScaler(t, group, c, st, func(string, string) { t.Error("nothing to adopt yet") })
	instance := acquire(t, first, "fleetci-gh-1").InstanceID()
	stop() // the job still holds its VM
	if group.wasRemoved(instance) {
		t.Fatal("stopping removed the VM a job holds")
	}

	var got adoptions
	next, _ := storedScaler(t, group, c, st, got.add)
	got.wait(t, "fleetci-gh-1@"+instance)

	// The job ends: the key is released, and the VM, used once, goes.
	next.Release("fleetci-gh-1")
	deadline := time.Now().Add(10 * time.Second)
	for !group.wasRemoved(instance) {
		if time.Now().After(deadline) {
			t.Fatal("the adopted VM was never removed after its job")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestAnInstanceWithNoSavedStateIsRemoved(t *testing.T) {
	up := adoptUp(func(taskscaler.Taskscaler, taskscaler.UpFuncInstance) error { return nil },
		func(string, string) { t.Error("adopted") })
	err := up(nil, taskscaler.UpFuncInstance{ID: "i-1", Cause: fleeting.CausePreexisted})
	if err == nil {
		t.Fatal("a VM nothing says anything about was kept")
	}
}

func TestAWarmVMWithSavedStateIsKept(t *testing.T) {
	up := adoptUp(func(taskscaler.Taskscaler, taskscaler.UpFuncInstance) error { return nil },
		func(string, string) { t.Error("adopted a VM with no job") })
	if err := up(nil, taskscaler.UpFuncInstance{ID: "i-1", Cause: fleeting.CausePreexisted, Restored: true}); err != nil {
		t.Fatal(err)
	}
}

func TestAVMThatIsntReadyIsntAdopted(t *testing.T) {
	up := adoptUp(func(taskscaler.Taskscaler, taskscaler.UpFuncInstance) error { return errors.New("no docker") },
		func(string, string) { t.Error("adopted") })
	err := up(nil, taskscaler.UpFuncInstance{ID: "i-1", Cause: fleeting.CausePreexisted, Restored: true, Acquisitions: []string{"k"}})
	if err == nil {
		t.Fatal("no error")
	}
}

func TestTheSweepLeavesTheVMsGivenToTheNextFleetci(t *testing.T) {
	group := newCloud("i-idle", "i-job")
	sweep(context.Background(), group, map[string]bool{"i-job": true}, hclog.NewNullLogger())
	if !group.wasRemoved("i-idle") || group.wasRemoved("i-job") {
		t.Fatalf("removed %q", group.removed)
	}
}

func TestOnlyAStoredClassHandsOver(t *testing.T) {
	stored := &pool{name: "aws", stored: true}
	plain := &pool{name: "local"}
	b := &Backend{log: hclog.NewNullLogger(), vms: map[string]*pool{"a": stored, "b": plain}}
	b.instance = map[string]string{"a": "i-a"}
	if !b.HandOver("a") || b.handed["a"] != "i-a" {
		t.Fatalf("stored class: handed %v", b.handed)
	}
	if b.HandOver("b") || b.HandOver("unknown") {
		t.Fatal("handed over a VM whose class keeps no state")
	}
	if _, ok := b.vms["a"]; ok {
		t.Fatal("a handed over VM is still this process's")
	}
}

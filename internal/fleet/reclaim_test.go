package fleet

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"gitlab.com/gitlab-org/fleeting/taskscaler"
)

// errReclaim stands in for the AWS plugin's spot termination notice.
var errReclaim = errors.New("spot instance termination notice")

// cloud is a plugin whose instances the cloud can mark for reclaim.
type cloud struct {
	provider.InstanceGroup
	mu        sync.Mutex
	instances []string
	marked    map[string]bool
	removed   []string
	next      int
}

func newCloud(ids ...string) *cloud {
	return &cloud{instances: ids, marked: map[string]bool{}}
}

func (c *cloud) Update(_ context.Context, fn func(string, provider.State)) error {
	c.mu.Lock()
	ids := slices.Clone(c.instances)
	c.mu.Unlock()
	for _, id := range ids {
		fn(id, provider.StateRunning)
	}
	return nil
}

func (c *cloud) Increase(_ context.Context, n int) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for range n {
		c.next++
		c.instances = append(c.instances, "i-new"+string(rune('0'+c.next)))
	}
	return n, nil
}

func (c *cloud) Decrease(_ context.Context, ids []string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		c.instances = slices.DeleteFunc(c.instances, func(s string) bool { return s == id })
		c.removed = append(c.removed, id)
	}
	return ids, nil
}

func (c *cloud) ConnectInfo(_ context.Context, id string) (provider.ConnectInfo, error) {
	return provider.ConnectInfo{ID: id}, nil
}

func (c *cloud) Heartbeat(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.marked[id] {
		return errReclaim
	}
	return nil
}

func (c *cloud) Shutdown(context.Context) error { return nil }

func (c *cloud) mark(id string) {
	c.mu.Lock()
	c.marked[id] = true
	c.mu.Unlock()
}

func (c *cloud) wasRemoved(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Contains(c.removed, id)
}

func scaler(t *testing.T, group provider.InstanceGroup, c Class) taskscaler.Taskscaler {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	up := func(taskscaler.Taskscaler, taskscaler.UpFuncInstance) error { return nil }
	ts, err := taskscaler.New(ctx, initialised{group, provider.ProviderInfo{ID: "cloud", MaxSize: 10}},
		append(scalerOptions(c, provider.Settings{}, up, nil, hclog.NewNullLogger()),
			// Faster than production, so the tests take seconds.
			taskscaler.WithAcquireDelay(50*time.Millisecond),
			taskscaler.WithUpdateInterval(100*time.Millisecond),
			taskscaler.WithUpdateIntervalWhenExpecting(100*time.Millisecond),
			taskscaler.WithShutdownDeletionInterval(10*time.Millisecond))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Shutdown waits for held jobs; a failed test may leave one.
		ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		ts.Shutdown(ctx)
		cancel()
	})
	return ts
}

func acquire(t *testing.T, ts taskscaler.Taskscaler, key string) taskscaler.Acquisition {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	acq, err := ts.Acquire(ctx, key)
	if err != nil {
		t.Fatalf("acquiring %s: %v", key, err)
	}
	return acq
}

func TestNoJobGoesToAnInstanceMarkedForReclaim(t *testing.T) {
	group := newCloud("i-marked", "i-ok")
	group.mark("i-marked")
	ts := scaler(t, group, Class{MaxInstances: 2, MaxUseCount: 100})

	for i := range 10 {
		key := "job" + string(rune('0'+i))
		acq := acquire(t, ts, key)
		if acq.InstanceID() != "i-ok" {
			t.Fatalf("job %d placed on %s, which the cloud marked for reclaim", i, acq.InstanceID())
		}
		ts.Release(key)
	}
}

func TestAMarkedInstanceKeepsItsJobAndGoesWhenEmpty(t *testing.T) {
	group := newCloud("i-busy")
	ts := scaler(t, group, Class{MaxInstances: 2, MaxUseCount: 100})

	if acq := acquire(t, ts, "running"); acq.InstanceID() != "i-busy" {
		t.Fatalf("first job on %s, want i-busy", acq.InstanceID())
	}
	group.mark("i-busy")

	// The next job gets a new instance; the running one stays where it is.
	if acq := acquire(t, ts, "next"); acq.InstanceID() == "i-busy" {
		t.Fatal("second job placed on the marked instance")
	}
	time.Sleep(time.Second)
	if group.wasRemoved("i-busy") {
		t.Fatal("marked instance removed while its job ran")
	}

	// Once empty, the failed heartbeats count: no job lands on it and it
	// is removed.
	ts.Release("running")
	deadline := time.Now().Add(30 * time.Second)
	for i := 0; !group.wasRemoved("i-busy"); i++ {
		if time.Now().After(deadline) {
			t.Fatal("empty marked instance never removed")
		}
		key := "probe" + string(rune('0'+i%10))
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		acq, err := ts.Acquire(ctx, key)
		cancel()
		if err == nil {
			if acq.InstanceID() == "i-busy" {
				t.Fatal("job placed on the marked instance after it emptied")
			}
			ts.Release(key)
		}
		time.Sleep(50 * time.Millisecond)
	}
	ts.Release("next")
}

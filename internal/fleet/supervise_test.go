package fleet

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

// fakeGroup is a plugin that can die.
type fakeGroup struct {
	provider.InstanceGroup
	name string
	dead atomic.Bool
}

var errGone = status.Error(codes.Unavailable, "connection error: desc = \"error reading from server: EOF\"")

func (f *fakeGroup) Update(_ context.Context, fn func(string, provider.State)) error {
	if f.dead.Load() {
		return errGone
	}
	fn(f.name, provider.StateRunning)
	return nil
}

func (f *fakeGroup) Increase(_ context.Context, n int) (int, error) {
	if f.dead.Load() {
		return 0, errGone
	}
	return n, nil
}

type fakePlugins struct {
	mu     sync.Mutex
	groups []*fakeGroup
	exited []func()
	killed int
	fail   int // start fails this many more times
	id     string
}

func (p *fakePlugins) start(_ context.Context, exited func()) (provider.InstanceGroup, provider.ProviderInfo, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail > 0 {
		p.fail--
		return nil, provider.ProviderInfo{}, nil, errors.New("no plugin binary")
	}
	g := &fakeGroup{name: "vm" + string(rune('0'+len(p.groups)))}
	p.groups = append(p.groups, g)
	p.exited = append(p.exited, exited)
	return g, provider.ProviderInfo{ID: p.id}, func() { p.mu.Lock(); p.killed++; p.mu.Unlock() }, nil
}

func (p *fakePlugins) started() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.groups)
}

func names(t *testing.T, g provider.InstanceGroup) ([]string, error) {
	t.Helper()
	var got []string
	err := g.Update(context.Background(), func(id string, _ provider.State) { got = append(got, id) })
	return got, err
}

func TestSupervisedRestartsAGonePlugin(t *testing.T) {
	p := &fakePlugins{id: "fd4"}
	s, info, err := supervise(context.Background(), p.start, hclog.NewNullLogger())
	if err != nil || info.ID != "fd4" {
		t.Fatalf("supervise: %v %v", info, err)
	}
	if got, _ := names(t, s); len(got) != 1 || got[0] != "vm0" {
		t.Fatalf("first plugin lists %v", got)
	}

	p.groups[0].dead.Store(true)
	if _, err := names(t, s); !pluginGone(err) {
		t.Fatalf("call on the dead plugin: %v, want its error", err)
	}
	got, err := names(t, s)
	if err != nil || len(got) != 1 || got[0] != "vm1" {
		t.Fatalf("after the restart: %v %v, want the new plugin's vm1", got, err)
	}
	defer s.Close()
	if p.killed != 1 || len(p.groups) != 2 {
		t.Fatalf("killed %d, started %d; want the old one killed and one new", p.killed, len(p.groups))
	}
}

func TestSupervisedRestartsOnceForConcurrentFailures(t *testing.T) {
	p := &fakePlugins{id: "fd4"}
	s, _, _ := supervise(context.Background(), p.start, hclog.NewNullLogger())
	p.groups[0].dead.Store(true)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = s.Increase(context.Background(), 1) }()
	}
	wg.Wait()
	if len(p.groups) != 2 {
		t.Fatalf("started %d plugins, want 2", len(p.groups))
	}
}

func TestSupervisedBacksOffAFailingStart(t *testing.T) {
	p := &fakePlugins{id: "fd4"}
	s, _, _ := supervise(context.Background(), p.start, hclog.NewNullLogger())
	defer s.Close()
	p.groups[0].dead.Store(true)
	p.fail = 1
	_, _ = names(t, s) // restart fails
	_, _ = names(t, s) // within the backoff: no attempt
	if len(p.groups) != 1 {
		t.Fatalf("started %d plugins during the backoff", len(p.groups))
	}
	// The retry comes on its own once the backoff (1s) is over.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if got, err := names(t, s); err == nil && got[0] == "vm1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no restart after the backoff")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestSupervisedRestartsWhenTheProcessExits(t *testing.T) {
	p := &fakePlugins{id: "fd4"}
	s, _, _ := supervise(context.Background(), p.start, hclog.NewNullLogger())
	first, exited := p.groups[0], p.exited[0]
	first.dead.Store(true)
	exited() // go-plugin saw the process end; no call made
	deadline := time.Now().Add(2 * time.Second)
	for {
		if g, _ := s.current(); g != first {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no restart after the process exited")
		}
		time.Sleep(10 * time.Millisecond)
	}
	exited() // a late second report changes nothing
	time.Sleep(50 * time.Millisecond)
	if n := p.started(); n != 2 {
		t.Fatalf("started %d plugins, want 2", n)
	}
}

func TestExitWatch(t *testing.T) {
	var n atomic.Int32
	w := watchExit(hclog.NewNullLogger(), func() { n.Add(1) })
	w.Info("plugin started")
	w.Error("plugin process exited", "error", "signal: killed")
	w.Info("plugin process exited")
	if n.Load() != 1 {
		t.Fatalf("exited called %d times, want once", n.Load())
	}
}

func TestSupervisedRefusesAnotherGroup(t *testing.T) {
	p := &fakePlugins{id: "fd4"}
	s, _, _ := supervise(context.Background(), p.start, hclog.NewNullLogger())
	defer s.Close() // stops the retry
	first := p.groups[0]
	first.dead.Store(true)
	p.mu.Lock()
	p.id = "other"
	p.mu.Unlock()
	_, _ = names(t, s)
	if g, _ := s.current(); g != first {
		t.Fatal("switched to a plugin of another group")
	}
	p.mu.Lock()
	killed := p.killed
	p.mu.Unlock()
	if killed != 2 {
		t.Fatalf("killed %d, want the old and the wrong one", killed)
	}
}

func TestSupervisedNoRestartAfterClose(t *testing.T) {
	p := &fakePlugins{id: "fd4"}
	s, _, _ := supervise(context.Background(), p.start, hclog.NewNullLogger())
	s.Close()
	p.groups[0].dead.Store(true)
	_, _ = names(t, s)
	if len(p.groups) != 1 {
		t.Fatal("restarted a closed class's plugin")
	}
}

func TestPluginGone(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errGone, true},
		{errors.New("rpc error: connection is shut down"), true},
		{status.Error(codes.Unknown, "instance not found"), false},
		{errors.New("available VM address space exhausted"), false},
	} {
		if got := pluginGone(c.err); got != c.want {
			t.Errorf("pluginGone(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// A plugin that exits while it starts: its restart waits for the start,
// and then replaces it, instead of finding nothing to replace.
func TestSupervisedPluginExitsWhileStarting(t *testing.T) {
	p := &fakePlugins{id: "fd4"}
	first := true
	start := func(ctx context.Context, exited func()) (provider.InstanceGroup, provider.ProviderInfo, func(), error) {
		g, info, kill, err := p.start(ctx, exited)
		if first {
			first = false
			g.(*fakeGroup).dead.Store(true)
			exited()
			time.Sleep(20 * time.Millisecond) // the restart runs meanwhile
		}
		return g, info, kill, err
	}
	s, _, err := supervise(context.Background(), start, hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for p.started() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("no restart")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for {
		if g, _ := s.current(); g != p.groups[0] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the dead plugin is still current")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

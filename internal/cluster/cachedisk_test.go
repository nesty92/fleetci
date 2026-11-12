package cluster

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/cachedisk"
	"github.com/nesty92/fleetci/internal/source"
)

// cacheFleet is a controller with workers w1 and w2, as their reports say.
func cacheFleet(t *testing.T, w1, w2 Report) *Controller {
	t.Helper()
	cfg := &ControllerConfig{
		Controller: ControllerTable{ID: "ctl-1", CacheWait: 1},
		Classes:    map[string]*ClassSpec{"small": {CPUs: 1, Memory: 1024, CacheDisk: true}, "plain": {CPUs: 1, Memory: 1024}},
	}
	c := &Controller{cfg: cfg, log: hclog.NewNullLogger(), workers: map[string]*workerConn{}}
	for id, r := range map[string]Report{"w1": w1, "w2": w2} {
		r.State = Ready
		c.workers[id] = &workerConn{id: id, report: r, seen: time.Now(), facts: Facts{CPUs: 4, MemoryMB: 8192}}
	}
	return c
}

func ids(ws []*workerConn) []string {
	var out []string
	for _, w := range ws {
		out = append(out, w.id)
	}
	return out
}

// TestPlaceByCache: a job goes first to the worker with its own cache,
// then to one with the cache it falls back on; when the worker with the
// best is full, Create says so, to wait for it.
func TestPlaceByCache(t *testing.T) {
	room := map[string]int{"small": 1, "plain": 1}
	full := map[string]int{"small": 0, "plain": 0}
	c := cacheFleet(t,
		Report{Room: room, Caches: []string{"main", "x"}},
		Report{Room: room, Caches: []string{"feature"}})
	dirs := []string{"feature", "main"}

	ws, better := c.byCache(c.candidates("small"), "small", dirs)
	if got := ids(ws); len(got) != 2 || got[0] != "w2" || better {
		t.Fatalf("own cache first: %v, better %v", got, better)
	}
	ws, _ = c.byCache(c.candidates("small"), "small", []string{"other", "main"})
	if got := ids(ws); got[0] != "w1" {
		t.Fatalf("the fallback's worker first: %v", got)
	}
	// A class without cache_disk keeps its order.
	if _, better := c.byCache(c.candidates("plain"), "plain", dirs); better {
		t.Fatal("plain class waits for a cache")
	}

	c.workers["w2"].report.Room = full
	ws, better = c.byCache(c.candidates("small"), "small", dirs)
	if got := ids(ws); len(got) != 1 || got[0] != "w1" || !better {
		t.Fatalf("holder full: %v, better %v", got, better)
	}
	// A holder that isn't Ready isn't waited for.
	c.workers["w2"].report.State = Draining
	if _, better := c.byCache(c.candidates("small"), "small", dirs); better {
		t.Fatal("waits for a draining worker")
	}
}

// TestCacheRelay: a GitHub job's hook goes through the worker's report to
// the leader, whose decision reaches the worker on the assignment, and
// what the job was told comes back.
func TestCacheRelay(t *testing.T) {
	f := newTestFleet(t)
	ready := Report{State: Ready, Room: map[string]int{"small": 1}}
	f.report(t, ready)
	l := f.watch(t, "")
	vmc := make(chan string, 1)
	go func() {
		vm, _ := f.c.Capacity().Create(context.Background(), "small")
		vmc <- vm.ID
	}()
	l = f.watch(t, l.Rev)
	st := AssignmentStatus{ID: l.Assignments[0].ID, Class: "small", Phase: PhaseReady, VM: "vm-1"}
	ready.Assignments = []AssignmentStatus{st}
	f.report(t, ready)
	id := <-vmc
	relay := f.c.Capacity().(source.CacheRelay)

	hint := make(chan string, 1)
	go func() {
		h, err := relay.JobHint(context.Background(), id)
		if err != nil {
			t.Error(err)
		}
		hint <- h
	}()
	st.Phase, st.JobHint = PhaseRunning, "o/r 70"
	ready.Assignments = []AssignmentStatus{st}
	f.report(t, ready)
	if h := <-hint; h != "o/r 70" {
		t.Fatalf("hint %q", h)
	}

	told := make(chan string, 1)
	go func() {
		n, err := relay.GiveCache(context.Background(), id, &cachedisk.Scope{Ref: "main"}, "")
		if err != nil {
			t.Error(err)
		}
		told <- n
	}()
	l = f.watch(t, l.Rev)
	if c := l.Assignments[0].Cache; c == nil || c.Scope == nil || c.Scope.Ref != "main" {
		t.Fatalf("assignment's cache %+v", c)
	}
	st.CacheNote = "Cache disk: main"
	ready.Assignments = []AssignmentStatus{st}
	f.report(t, ready)
	if n := <-told; n != "Cache disk: main" {
		t.Fatalf("told %q", n)
	}
}

// TestWorkerConfigCacheDisk: worker.toml keeps a [cache_disk] set, and
// fleetci join writes none when it isn't, which an operator then adds.
func TestWorkerConfigCacheDisk(t *testing.T) {
	path := t.TempDir() + "/worker.toml"
	c := &WorkerConfig{ID: "w1", Controllers: []string{"ctl"}, File: path}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "cache_disk") {
		t.Fatalf("an unset cache_disk is written:\n%s", b)
	}
	if err := os.WriteFile(path, append(b, "\n[cache_disk]\nbudget = \"10GB\"\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadWorker(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.CacheDisk.Budget != 10*1024 {
		t.Fatalf("budget %v", got.CacheDisk.Budget)
	}
	if err := got.Save(); err != nil {
		t.Fatal(err)
	}
	if again, err := LoadWorker(path); err != nil || again.CacheDisk.Budget != 10*1024 {
		t.Fatalf("after saving again: %v %v", again, err)
	}
}

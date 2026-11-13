package cluster

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/cachedisk"
	"github.com/nesty92/fleetci/internal/gitlab"
	"github.com/nesty92/fleetci/internal/source"
)

func TestFitReachesGitLab(t *testing.T) {
	for url, want := range map[string]string{"": "https://gitlab.com/", "https://gl.example/": "https://gl.example/"} {
		c := &Controller{cfg: &ControllerConfig{GitLab: &gitlab.Config{URL: url, ConfigFile: "config.toml"}}}
		if got := c.fit(Facts{}).Reach; len(got) != 1 || got[0] != want {
			t.Errorf("url %q: reach %q, want %q", url, got, want)
		}
	}
}

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
// then to one with the cache it falls back on; when only full workers
// have any of its caches, Create says so, to wait for one.
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
	if got := ids(ws); len(got) != 1 || got[0] != "w1" || better {
		t.Fatalf("own cache's worker full, main's free: %v, better %v", got, better)
	}
	// Waited for only when the workers with room have none of its caches.
	c.workers["w1"].report.Caches = []string{"x"}
	ws, better = c.byCache(c.candidates("small"), "small", dirs)
	if got := ids(ws); len(got) != 1 || got[0] != "w1" || !better {
		t.Fatalf("holder full, the rest cold: %v, better %v", got, better)
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

// TestPlaceByFreshness: of two workers with the job's cache, the newer
// copy first; with none of its caches on a worker with room, the job
// waits while one of them is copying one in.
func TestPlaceByFreshness(t *testing.T) {
	room := map[string]int{"small": 1}
	now := time.Now()
	c := cacheFleet(t,
		Report{Room: room, Caches: []string{"main"}, CacheCommitted: map[string]time.Time{"main": now.Add(-time.Hour)}},
		Report{Room: room, Caches: []string{"main"}, CacheCommitted: map[string]time.Time{"main": now}})
	dirs := []string{"feature", "main"}
	if ws, better := c.byCache(c.candidates("small"), "small", dirs); ids(ws)[0] != "w2" || better {
		t.Fatalf("newer copy first: %v, better %v", ids(ws), better)
	}
	// Own cache beats a newer fallback.
	c.workers["w1"].report.Caches = []string{"feature"}
	c.workers["w1"].report.CacheCommitted = map[string]time.Time{"feature": now.Add(-48 * time.Hour)}
	if ws, _ := c.byCache(c.candidates("small"), "small", dirs); ids(ws)[0] != "w1" {
		t.Fatalf("own cache first: %v", ids(ws))
	}
	// Neither has any, one is copying main's in: wait.
	for _, w := range c.workers {
		w.report.Caches, w.report.CacheCommitted = nil, nil
	}
	if _, better := c.byCache(c.candidates("small"), "small", dirs); better {
		t.Fatal("waits with no cache anywhere")
	}
	c.workers["w1"].report.CachesArriving = []string{"main"}
	if _, better := c.byCache(c.candidates("small"), "small", dirs); !better {
		t.Fatal("doesn't wait for main's cache arriving")
	}
	// Arrived: placed there, no wait.
	c.workers["w1"].report.CachesArriving = nil
	c.workers["w1"].report.Caches = []string{"main"}
	if ws, better := c.byCache(c.candidates("small"), "small", dirs); ids(ws)[0] != "w1" || better {
		t.Fatalf("after the copy: %v, better %v", ids(ws), better)
	}
}

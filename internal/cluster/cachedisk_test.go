package cluster

import (
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
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

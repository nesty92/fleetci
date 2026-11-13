package cluster

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/nesty92/fleetci/internal/cachedisk"
	"github.com/nesty92/fleetci/internal/fleet"
	"github.com/nesty92/fleetci/internal/github"
	"github.com/nesty92/fleetci/internal/proxy"
	"github.com/nesty92/fleetci/internal/source"
)

// Capacity is the fleet's room as the leader's job sources see it: each VM
// is created on the worker with room for it, picked by placement, and is
// that worker's from then on.
func (c *Controller) Capacity() source.Capacity { return fleetCapacity{c} }

type fleetCapacity struct{ c *Controller }

var _ source.Capacity = fleetCapacity{}

func (f fleetCapacity) Classes(context.Context) ([]source.Class, string, error) {
	c := f.c
	var out []source.Class
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, name := range slices.Sorted(maps.Keys(c.cfg.Classes)) {
		spec := c.cfg.Classes[name]
		// Slots: what the workers that run the class could hold of it,
		// at least one so a source never thinks the class empty.
		slots := 0
		for _, w := range c.workers {
			if slices.ContainsFunc(w.fit.Classes, func(x Class) bool { return x.Name == name }) {
				slots += min(w.facts.CPUs/spec.CPUs, int(w.facts.MemoryMB/int64(spec.Memory)))
			}
		}
		if spec.MaxInstances > 0 {
			slots = min(slots, spec.MaxInstances)
		}
		out = append(out, source.Class{Name: name, Slots: max(slots, 1), Overflow: spec.Overflow, Arch: spec.Arch})
	}
	return out, c.cfg.DefaultClass, nil
}

// candidates are the workers with room for class now, the best first by
// placement: pack the busiest, spread the idlest.
func (c *Controller) candidates(class string) []*workerConn {
	c.mu.Lock()
	defer c.mu.Unlock()
	spec := c.cfg.Classes[class]
	if spec == nil {
		return nil
	}
	if spec.MaxInstances > 0 {
		n := 0
		for _, w := range c.workers {
			for _, v := range w.report.VMs {
				if v.Class == class {
					n++
				}
			}
			n += w.pendingLocked(class)
		}
		if n >= spec.MaxInstances {
			return nil
		}
	}
	var ws []*workerConn
	for _, w := range c.workers {
		if w.state() == Ready && w.report.Room[class]-w.pendingLocked(class) > 0 {
			ws = append(ws, w)
		}
	}
	score := func(w *workerConn) float64 { // the share of the budget in use
		cpu := float64(w.report.UsedCPUs) / float64(max(w.facts.CPUs, 1))
		mem := float64(w.report.UsedMemory) / float64(max(w.facts.MemoryMB, 1))
		return (cpu + mem) / 2
	}
	slices.SortFunc(ws, func(a, b *workerConn) int {
		sa, sb := score(a), score(b)
		if c.cfg.Controller.Placement == fleet.Spread {
			sa, sb = sb, sa
		}
		switch {
		case sa > sb:
			return -1
		case sa < sb:
			return 1
		}
		return strings.Compare(a.id, b.id)
	})
	return ws
}

// Reserve says whether some worker has room for the class, or its
// overflow, now; it holds none: the VM is placed by Create, which waits
// for room if it went meanwhile.
func (f fleetCapacity) Reserve(_ context.Context, class string) (source.Reservation, error) {
	spec := f.c.cfg.Classes[class]
	if spec == nil {
		return source.Reservation{}, fmt.Errorf("no class %q", class)
	}
	if len(f.c.candidates(class)) == 0 && (spec.Overflow == "" || len(f.c.candidates(spec.Overflow)) == 0) {
		return source.Reservation{}, source.ErrNoRoom
	}
	return source.Reservation{ID: fmt.Sprintf("fleet-%d", time.Now().UnixNano()), Class: class}, nil
}

func (f fleetCapacity) Acquire(ctx context.Context, r source.Reservation) (source.VM, error) {
	return f.Create(ctx, r.Class)
}

func (f fleetCapacity) Release(context.Context, source.Reservation) error { return nil }

// Create waits for a worker with room for the class, or its overflow, and
// boots the VM there.
func (f fleetCapacity) Create(ctx context.Context, class string) (source.VM, error) {
	c := f.c
	spec := c.cfg.Classes[class]
	if spec == nil {
		return source.VM{}, fmt.Errorf("no class %q", class)
	}
	try := []string{class}
	if spec.Overflow != "" {
		try = append(try, spec.Overflow)
	}
	dirs := source.Caches(ctx)
	waitTill := time.Now().Add(time.Duration(c.cfg.Controller.CacheWait))
	waiting := false
	for {
		wake := c.changed()
		for i, cl := range try {
			ws, better := c.byCache(c.candidates(cl), cl, dirs)
			if i == 0 && better && time.Now().Before(waitTill) {
				// The worker with the job's cache is full: wait for it a
				// while, rather than start the job cold elsewhere.
				if !waiting {
					c.log.Info("waiting for room on the worker with the job's cache", "class", cl, "for", time.Until(waitTill).Round(time.Second).String())
					waiting = true
				}
				break
			}
			for _, w := range ws {
				vm, err := c.createOn(ctx, w, cl)
				if err == nil {
					return vm, nil
				}
				if ctx.Err() != nil {
					return source.VM{}, ctx.Err()
				}
				if errors.Is(err, ErrNoRoom) {
					// Its report had room it no longer has: say so, a
					// placement that keeps missing is worth seeing.
					c.log.Info("the worker had no room after all", "worker", w.id, "class", cl)
				} else {
					c.log.Warn("creating a vm on a worker", "worker", w.id, "class", cl, "err", err)
				}
			}
		}
		next := 5 * time.Second
		if w := time.Until(waitTill); waiting && w > 0 {
			next = min(next, w)
		}
		select {
		case <-ctx.Done():
			return source.VM{}, ctx.Err()
		case <-wake:
		case <-time.After(next):
		}
	}
}

// byCache puts first the workers that have the best of the caches a job
// starts from (dirs, the best first), keeping the order otherwise. better
// says a worker without room has a better one than any of ws.
func (c *Controller) byCache(ws []*workerConn, class string, dirs []string) (_ []*workerConn, better bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if spec := c.cfg.Classes[class]; spec == nil || !spec.CacheDisk || len(dirs) == 0 {
		return ws, false
	}
	rank := func(w *workerConn) int {
		for i, d := range dirs {
			if _, ok := slices.BinarySearch(w.report.Caches, d); ok {
				return i
			}
		}
		return len(dirs)
	}
	best := len(dirs) // over the workers that run the class
	for _, w := range c.workers {
		if _, runs := w.report.Room[class]; runs && w.state() == Ready {
			best = min(best, rank(w))
		}
	}
	ranks := map[*workerConn]int{}
	here := len(dirs)
	for _, w := range ws {
		ranks[w] = rank(w)
		here = min(here, ranks[w])
	}
	slices.SortStableFunc(ws, func(a, b *workerConn) int { return ranks[a] - ranks[b] })
	return ws, best < here
}

// bootWait is how long a worker may take to boot an assignment's VM.
const bootWait = 10 * time.Minute

// createOn writes an assignment for a VM of class on the worker and waits
// for it to be Ready. When ctx ends first, the assignment is cancelled, and
// the worker removes the VM it booted for nothing.
func (c *Controller) createOn(ctx context.Context, w *workerConn, class string) (source.VM, error) {
	a := c.assign(w, class)
	deadline := time.NewTimer(bootWait)
	defer deadline.Stop()
	for {
		wake := c.changed()
		c.mu.Lock()
		st, wst := a.status, w.state()
		c.mu.Unlock()
		switch {
		case st.Phase == PhaseReady:
			// Whoever runs the job on it follows it from here, or deletes it.
			c.unfollow(w, a)
			c.log.Info("vm placed", "worker", w.id, "class", st.Class, "vm", st.VM, "assignment", a.ID)
			return source.VM{ID: w.id + "/" + a.ID, Class: st.Class, Instance: st.Instance}, nil
		case st.Phase == PhaseFailed:
			c.unfollow(w, a)
			if st.Reason == ReasonNoRoom {
				return source.VM{}, ErrNoRoom
			}
			return source.VM{}, errors.New(st.Err)
		case st.terminal(), a.Cancel:
			c.unfollow(w, a)
			return source.VM{}, fmt.Errorf("assignment %s ended before its VM was ready", a.ID)
		case wst == Unreachable:
			c.cancel(w, a)
			return source.VM{}, fmt.Errorf("worker %s is unreachable", w.id)
		}
		select {
		case <-ctx.Done():
			c.cancel(w, a)
			return source.VM{}, ctx.Err()
		case <-deadline.C:
			c.cancel(w, a)
			return source.VM{}, fmt.Errorf("the VM was not ready in %s", bootWait)
		case <-wake:
		case <-time.After(5 * time.Second):
		}
	}
}

var _ source.CacheRelay = fleetCapacity{}

// HasCache says whether the VM's class has a cache disk.
func (f fleetCapacity) HasCache(id string) bool {
	_, a, err := f.c.assignmentOf(id)
	if err != nil || a == nil {
		return false
	}
	f.c.mu.Lock()
	class := cmp.Or(a.status.Class, a.Class)
	f.c.mu.Unlock()
	spec := f.c.cfg.Classes[class]
	return spec != nil && spec.CacheDisk
}

// ClassHasCache says whether the class's VMs have a cache disk.
func (f fleetCapacity) ClassHasCache(class string) bool {
	spec := f.c.cfg.Classes[class]
	return spec != nil && spec.CacheDisk
}

// AttachCache: a fleet's VM gets its disk from its worker (GiveCache).
func (f fleetCapacity) AttachCache(context.Context, string, cachedisk.Scope) (*cachedisk.Start, error) {
	return nil, errors.New("a fleet's worker attaches its VM's cache disk")
}

// JobHint waits for the worker to relay what the VM's job hook said.
func (f fleetCapacity) JobHint(ctx context.Context, id string) (string, error) {
	st, err := f.c.awaitStatus(ctx, id, func(s AssignmentStatus) bool { return s.JobHint != "" || s.CacheNote != "" })
	if err != nil {
		return "", err
	}
	if st.JobHint == "" {
		return "", errors.New(st.CacheNote) // the worker got no word from the hook
	}
	return st.JobHint, nil
}

// GiveCache writes the leader's decision on the VM's assignment: its
// worker attaches the disk and answers the job's hook.
func (f fleetCapacity) GiveCache(ctx context.Context, id string, scope *cachedisk.Scope, note string) (string, error) {
	c := f.c
	w, a, err := c.assignmentOf(id)
	if err != nil {
		return "", err
	}
	if a == nil {
		return "", fmt.Errorf("vm %s is gone", id)
	}
	c.mu.Lock()
	if a.Cache == nil {
		a.Cache = &CacheGrant{Scope: scope, Note: note}
		w.bumpLocked()
	}
	c.mu.Unlock()
	st, err := c.awaitStatus(ctx, id, func(s AssignmentStatus) bool { return s.CacheNote != "" })
	return st.CacheNote, err
}

// awaitStatus waits for the VM's assignment's status to be done, or over.
func (c *Controller) awaitStatus(ctx context.Context, id string, done func(AssignmentStatus) bool) (AssignmentStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	for {
		wake := c.changed()
		_, a, err := c.assignmentOf(id)
		if err != nil {
			return AssignmentStatus{}, err
		}
		if a == nil {
			return AssignmentStatus{}, fmt.Errorf("vm %s is gone", id)
		}
		c.mu.Lock()
		st := a.status
		c.mu.Unlock()
		switch {
		case done(st):
			return st, nil
		case st.terminal():
			return st, fmt.Errorf("vm %s's job ended", id)
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-wake:
		case <-time.After(5 * time.Second):
		}
	}
}

// Conn is nil: a fleet VM is reached by its worker, not the controller.
func (f fleetCapacity) Conn(string) (*proxy.Conn, error) { return nil, nil }

// Delete cancels the VM's assignment: its worker removes it.
func (f fleetCapacity) Delete(_ context.Context, id string) error {
	w, a, err := f.c.assignmentOf(id)
	if err != nil {
		return err
	}
	if a != nil {
		f.c.cancel(w, a)
	}
	return nil
}

// HandOver leaves the VM to its worker, which follows its job to the end
// and removes it then, as it does for every job.
func (f fleetCapacity) HandOver(context.Context, string) bool { return true }

// assignmentOf finds a VM's worker and assignment by the VM's id,
// worker/assignment; the assignment is nil when it is over and gone.
func (c *Controller) assignmentOf(id string) (*workerConn, *assignment, error) {
	wid, aid, ok := strings.Cut(id, "/")
	if !ok {
		return nil, nil, fmt.Errorf("vm %q names no worker", id)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.workers[wid]
	if w == nil {
		return nil, nil, fmt.Errorf("no worker %s", wid)
	}
	return w, w.assigns[aid], nil
}

// lostAfter is how long a job's worker may be unreachable before the
// leader stops waiting for its job.
const lostAfter = 10 * time.Minute

// GitHubStart puts a GitHub job's runner on its VM's assignment and follows
// it by the worker's reports.
func (c *Controller) GitHubStart(cfg github.Config) github.StartFunc {
	return func(ctx context.Context, vm github.VM, jit string, took func(job string)) error {
		run := RunSpec{GitHub: &GitHubRun{JITConfig: jit, RunnerVersion: cfg.RunnerVersion, CacheURL: cfg.CacheURL, CacheStore: cfg.CacheStore, CacheBucket: cfg.CacheBucket, CacheTokenIssuer: cfg.CacheTokenIssuer, IdleTimeout: time.Duration(cfg.IdleTimeout), Cache: vm.Cache}}
		return c.runOn(ctx, vm.ID, run, took)
	}
}

// GitLabRemote puts a GitLab job on its VM's assignment, to run with the
// job's own token, and follows it.
func (c *Controller) GitLabRemote() func(ctx context.Context, vm source.VM, job, runner []byte, cache *cachedisk.Scope) error {
	return func(ctx context.Context, vm source.VM, job, runner []byte, cache *cachedisk.Scope) error {
		return c.runOn(ctx, vm.ID, RunSpec{GitLab: &GitLabRun{Job: job, Runner: runner, Cache: cache}}, nil)
	}
}

// runOn sets the job of a VM's assignment and waits for its end. When ctx
// ends first, the job goes on: its worker sees it through.
func (c *Controller) runOn(ctx context.Context, id string, run RunSpec, took func(job string)) error {
	w, a, err := c.assignmentOf(id)
	if err != nil {
		return err
	}
	c.mu.Lock()
	switch {
	case a == nil, a.Cancel, a.status.terminal():
		c.mu.Unlock()
		return fmt.Errorf("vm %s is gone", id)
	case a.Run != nil:
		c.mu.Unlock()
		return fmt.Errorf("vm %s runs a job already", id)
	}
	a.Run, a.followed = &run, true
	w.bumpLocked()
	c.mu.Unlock()
	defer c.unfollow(w, a)
	lastTook, lostSince := "", time.Time{}
	for {
		wake := c.changed()
		c.mu.Lock()
		st, wst := a.status, w.state()
		c.mu.Unlock()
		if st.Took != "" && st.Took != lastTook {
			lastTook = st.Took
			if took != nil {
				took(st.Took)
			}
		}
		switch st.Phase {
		case PhaseSucceeded:
			return nil
		case PhaseFailed:
			return errors.New(st.Err)
		}
		if wst == Unreachable {
			if lostSince.IsZero() {
				lostSince = time.Now()
			}
			if time.Since(lostSince) > lostAfter {
				return fmt.Errorf("worker %s unreachable for %s", w.id, lostAfter)
			}
		} else {
			lostSince = time.Time{}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		case <-time.After(5 * time.Second):
		}
	}
}

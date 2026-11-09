package cluster

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

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
			n += w.asked[class]
		}
		if n >= spec.MaxInstances {
			return nil
		}
	}
	var ws []*workerConn
	for _, w := range c.workers {
		if w.state() == Ready && w.report.Room[class]-w.asked[class] > 0 {
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
	for {
		wake := c.roomChanged()
		for _, cl := range try {
			for _, w := range c.candidates(cl) {
				vm, err := c.createOn(ctx, w, cl)
				if err == nil {
					return vm, nil
				}
				if ctx.Err() != nil {
					return source.VM{}, ctx.Err()
				}
				if !errors.Is(err, ErrNoRoom) {
					c.log.Warn("creating a vm on a worker", "worker", w.id, "class", cl, "err", err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return source.VM{}, ctx.Err()
		case <-wake:
		case <-time.After(5 * time.Second):
		}
	}
}

// createOn asks a worker for a VM. The ask outlives ctx, so a VM the
// worker boots for a job that went meanwhile is removed, not left.
func (c *Controller) createOn(ctx context.Context, w *workerConn, class string) (source.VM, error) {
	c.mu.Lock()
	w.asked[class]++
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		w.asked[class]--
		c.mu.Unlock()
	}()
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer cancel()
	var resp createResp
	if err := w.s.ask(actx, "create", createReq{Class: class}, &resp); err != nil {
		if errors.Is(err, ErrNoRoom) {
			c.mu.Lock()
			w.report.Room[class] = 0
			c.mu.Unlock()
		}
		return source.VM{}, err
	}
	if ctx.Err() != nil {
		_ = w.s.ask(context.WithoutCancel(ctx), "delete", vmReq{VM: resp.VM}, nil)
		return source.VM{}, ctx.Err()
	}
	c.log.Info("vm placed", "worker", w.id, "class", resp.Class, "vm", resp.VM)
	return source.VM{ID: w.id + "/" + resp.VM, Class: resp.Class, Instance: resp.Instance}, nil
}

// Conn is nil: a fleet VM is reached by its worker, not the controller.
func (f fleetCapacity) Conn(string) (*proxy.Conn, error) { return nil, nil }

func (f fleetCapacity) Delete(ctx context.Context, id string) error {
	w, vm, err := f.c.workerOf(id)
	if err != nil {
		return err
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	return w.s.ask(dctx, "delete", vmReq{VM: vm}, nil)
}

// HandOver leaves the VM to its worker, which follows its job to the end
// and removes it then, as it does for every job.
func (f fleetCapacity) HandOver(context.Context, string) bool { return true }

func (c *Controller) workerOf(id string) (*workerConn, string, error) {
	wid, vm, ok := strings.Cut(id, "/")
	if !ok {
		return nil, "", fmt.Errorf("vm %q names no worker", id)
	}
	c.mu.Lock()
	w := c.workers[wid]
	c.mu.Unlock()
	if w == nil || !w.connected {
		return nil, "", fmt.Errorf("worker %s is not connected", wid)
	}
	return w, vm, nil
}

// lostAfter is how long a job's worker may be unreachable before the
// leader stops waiting for its runner.
const lostAfter = 10 * time.Minute

// GitHubStart starts a GitHub job's runner on its VM's worker and follows
// it there by the worker's events.
func (c *Controller) GitHubStart(cfg github.Config) github.StartFunc {
	return func(ctx context.Context, vm github.VM, jit string, took func(job string)) error {
		req := runGitHubReq{JITConfig: jit, RunnerVersion: cfg.RunnerVersion, CacheURL: cfg.CacheURL, IdleTimeout: time.Duration(cfg.IdleTimeout)}
		return c.runOn(ctx, vm.ID, "run-github", func(id string) any { req.VM = id; return req }, took)
	}
}

// GitLabRemote runs a GitLab job on its VM's worker, with the job's own
// token, and follows it there.
func (c *Controller) GitLabRemote() func(ctx context.Context, vm source.VM, job, runner []byte) error {
	return func(ctx context.Context, vm source.VM, job, runner []byte) error {
		return c.runOn(ctx, vm.ID, "run-gitlab", func(id string) any {
			return runGitLabReq{VM: id, Job: job, Runner: runner}
		}, nil)
	}
}

// runOn asks a VM's worker to run a job on it and waits for the job's end,
// by the worker's events or, when they are lost with a stream, its reports.
func (c *Controller) runOn(ctx context.Context, fullID, kind string, req func(id string) any, took func(job string)) error {
	events := make(chan Event, 8)
	c.mu.Lock()
	c.watch[fullID] = events
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.watch, fullID)
		c.mu.Unlock()
	}()
	w, id, err := c.workerOf(fullID)
	if err != nil {
		return err
	}
	if err := w.s.ask(ctx, kind, req(id), nil); err != nil {
		return err
	}
	wid := w.id
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	missing, lostSince := 0, time.Time{}
	for {
		select {
		case e := <-events:
			switch e.Kind {
			case "took":
				if took != nil {
					took(e.Job)
				}
			case "exit":
				if e.Err != "" {
					return errors.New(e.Err)
				}
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			c.mu.Lock()
			w := c.workers[wid]
			st, has := w.state(), slices.ContainsFunc(w.report.VMs, func(v VMInfo) bool { return v.ID == id })
			c.mu.Unlock()
			switch {
			case st == Unreachable:
				if lostSince.IsZero() {
					lostSince = time.Now()
				}
				if time.Since(lostSince) > lostAfter {
					return fmt.Errorf("worker %s unreachable for %s", wid, lostAfter)
				}
			case !has:
				lostSince = time.Time{}
				if missing++; missing >= 3 {
					return nil // the job ended and its VM is gone
				}
			default:
				lostSince, missing = time.Time{}, 0
			}
		}
	}
}

package cluster

import (
	"fmt"
	"time"
)

// forgetMissing is how long an assignment a worker had may be missing from
// its reports before it counts as lost: two watches.
const forgetMissing = 2 * watchTimeout

// assignment is one the controller wrote, with what its worker last
// reported of it.
type assignment struct {
	Assignment
	status AssignmentStatus
	// seen: the worker has reported it, so its room counts it.
	seen bool
	// followed: someone here waits on it; one nobody follows is dropped
	// once it is over.
	followed bool
	// missing is when the worker's reports stopped having it.
	missing time.Time
}

// workerLocked is the worker's record, made on first sight.
func (c *Controller) workerLocked(id string) *workerConn {
	w := c.workers[id]
	if w == nil {
		w = &workerConn{id: id, assigns: map[string]*assignment{}, revWake: make(chan struct{}), draining: c.drains[id].Drained}
		c.workers[id] = w
	}
	return w
}

// revLocked is the worker's assignments' revision. The epoch makes a
// restarted controller's differ from what the worker last saw.
func (c *Controller) revLocked(w *workerConn) string {
	return fmt.Sprintf("%s.%d", c.epoch, w.rev)
}

// bumpLocked wakes the worker's watches.
func (w *workerConn) bumpLocked() {
	w.rev++
	close(w.revWake)
	w.revWake = make(chan struct{})
}

// assign writes a new assignment for a VM of class on the worker.
func (c *Controller) assign(w *workerConn, class string) *assignment {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	a := &assignment{
		Assignment: Assignment{ID: fmt.Sprintf("%s-%s-%d", c.ID(), c.epoch, c.nextID), Class: class, Created: time.Now()},
		status:     AssignmentStatus{Phase: PhasePending},
		followed:   true,
	}
	w.assigns[a.ID] = a
	w.bumpLocked()
	return a
}

// cancel asks the worker to stop the assignment and remove its VM.
func (c *Controller) cancel(w *workerConn, a *assignment) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a.followed = false
	if !a.Cancel && !a.status.terminal() {
		a.Cancel = true
		w.bumpLocked()
	}
	c.gcLocked(w)
}

// unfollow leaves the assignment to its worker: it is dropped here once it
// is over, while the worker sees it through either way.
func (c *Controller) unfollow(w *workerConn, a *assignment) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a.followed = false
	c.gcLocked(w)
}

// observeLocked takes in the worker's report of its assignments.
func (c *Controller) observeLocked(w *workerConn) {
	reported := make(map[string]AssignmentStatus, len(w.report.Assignments))
	for _, s := range w.report.Assignments {
		reported[s.ID] = s
	}
	now := time.Now()
	for id, a := range w.assigns {
		if s, ok := reported[id]; ok {
			a.status, a.seen, a.missing = s, true, time.Time{}
			continue
		}
		if !a.seen || a.status.terminal() {
			continue
		}
		// It had it and no longer reports it. After a partition, a report
		// can come before its watch lists the assignment again, and the
		// worker reports it again then; one still missing after that, it
		// forgot: it restarted.
		if a.missing.IsZero() {
			a.missing = now
		} else if now.Sub(a.missing) > forgetMissing {
			a.status = AssignmentStatus{ID: id, Class: a.Class, Phase: PhaseFailed, Err: "the worker no longer has it: did it restart?", Since: now}
		}
	}
	c.gcLocked(w)
}

// gcLocked drops the assignments nobody follows that are over: done, or
// cancelled and never taken up after the time a VM may wait for its job.
func (c *Controller) gcLocked(w *workerConn) {
	changed := false
	for id, a := range w.assigns {
		if a.followed {
			continue
		}
		if a.status.terminal() || (a.Cancel && !a.seen && time.Since(a.Created) > idleVM) {
			delete(w.assigns, id)
			changed = true
		}
	}
	if changed {
		w.bumpLocked()
	}
}

// pendingLocked are the worker's assignments of class it has not reported
// yet: its room does not count them.
func (w *workerConn) pendingLocked(class string) int {
	n := 0
	for _, a := range w.assigns {
		if a.Class == class && !a.seen && !a.Cancel && !a.status.terminal() {
			n++
		}
	}
	return n
}

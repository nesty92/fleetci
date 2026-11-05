package fleet

import (
	"slices"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"k8s.io/utils/clock"
)

// The host queue follows the Kubernetes scheduler and Kueue, whose rules it
// ports (Apache License 2.0, https://github.com/kubernetes/kubernetes and
// https://github.com/kubernetes-sigs/kueue). A host is to fleetci what a
// node is to Kubernetes: a class's VMs run on one host, and a job waits
// until a host of its class has room.
//
//   - Jobs wait in the order they came, as PrioritySort orders pods of equal
//     priority by their queue timestamp (queuesort/priority_sort.go).
//   - A waiting job that doesn't fit doesn't hold up later ones that do, as
//     in Kueue's default BestEffortFIFO (cache/queue/cluster_queue.go). Of
//     the jobs room would fit, the earliest gets it: a job counts the room of
//     each earlier waiting job that fits the host as taken, the order the
//     scheduler's one-pod-at-a-time cycle gives.
//   - A job that has waited NominateAfter is nominated to a host, as a pod
//     is nominated to a node (backend/queue/nominator.go). Every other job
//     then counts the nominated job's room on that host as taken, as
//     RunFilterPluginsWithNominatedPods adds a node's nominated pods of
//     equal or higher priority before it filters the node
//     (framework/runtime/framework.go): the room that frees there goes to
//     the nominated job, and later jobs fill only what it doesn't need.
//     Other hosts aren't affected. As in Kubernetes the nomination is a
//     hint: a nominated job that fits another host first starts there.
//   - A host holds one nomination at a time, for the job waiting longest,
//     as Volcano elects one target job for its reservation and locks the
//     node with the most idle room for it
//     (docs/design/job-resource-reservation-design.md). Were every late
//     job nominated, a busy host would fall back to strict order and leave
//     room idle.
//
// A large job thus waits at most NominateAfter plus the time it takes for
// enough running jobs to end on the host it is nominated to.

// hostQueue is the hosts the classes' VMs run on and the jobs waiting for
// room on them.
type hostQueue struct {
	// NominateAfter is how long a job waits before it is nominated.
	NominateAfter time.Duration
	clock         clock.PassiveClock
	log           hclog.Logger

	mu      sync.Mutex
	hosts   map[string]*host
	waiting []*waiter // in the order they came
}

// host is one machine's budget and what of it is in use.
type host struct {
	name string
	Budget
	fixed     Resources // the classes' fixed shares, part of used
	used      Resources
	nominated *waiter // the job the room that frees is held for, or nil
}

// free is the host's room not in use.
func (h *host) free() Resources { return h.Resources.add(h.used.times(-1)) }

// waiter is a job waiting for a VM of a class.
type waiter struct {
	class   string
	need    Resources
	hosts   []*host // the hosts its class runs on
	since   time.Time
	blocked bool  // a try found the class free but its hosts short
	on      *host // the host it is nominated to, or nil
}

func emptyQueue(nominateAfter time.Duration) *hostQueue {
	return &hostQueue{NominateAfter: nominateAfter, clock: clock.RealClock{}, log: hclog.NewNullLogger(), hosts: map[string]*host{}}
}

func (q *hostQueue) enqueue(class string, need Resources, hosts ...*host) *waiter {
	q.mu.Lock()
	defer q.mu.Unlock()
	w := &waiter{class: class, need: need, hosts: hosts, since: q.clock.Now()}
	q.waiting = append(q.waiting, w)
	return w
}

// dequeue takes w out of the queue: it got a VM or stopped waiting.
func (q *hostQueue) dequeue(w *waiter) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.started(w)
}

// started is dequeue with q.mu held.
func (q *hostQueue) started(w *waiter) {
	q.waiting = slices.DeleteFunc(q.waiting, func(x *waiter) bool { return x == w })
	if w.on != nil {
		w.on.nominated = nil
		w.on = nil
	}
}

// fits reports whether a VM of size fits on h in the room the jobs ahead of
// w leave: the job nominated to h, and the earlier waiting jobs that fit
// h. With w nil, ahead of every waiting job. q.mu is held.
func (q *hostQueue) fits(h *host, w *waiter, size Resources) bool {
	q.nominate()
	taken := h.used
	if n := h.nominated; n != nil && n != w {
		taken = taken.add(n.need)
	}
	for _, x := range q.waiting {
		if x == w {
			break
		}
		if x.blocked && x.on == nil && slices.Contains(x.hosts, h) && taken.add(x.need).fits(h.Resources) {
			taken = taken.add(x.need)
		}
	}
	return taken.add(size).fits(h.Resources)
}

// nominate nominates each job that has waited NominateAfter, oldest first,
// to the host of its class with the most free room that holds no other
// nomination and could ever fit it. q.mu is held.
func (q *hostQueue) nominate() {
	now := q.clock.Now()
	for _, x := range q.waiting {
		if !x.blocked || x.on != nil {
			continue
		}
		waited := now.Sub(x.since)
		if waited < q.NominateAfter {
			return // the jobs behind it waited less
		}
		var best *host
		for _, h := range x.hosts {
			if h.nominated != nil || !h.fixed.add(x.need).fits(h.Resources) {
				continue
			}
			if best == nil || roomier(h.free(), best.free()) {
				best = h
			}
		}
		if best == nil {
			continue // its hosts hold others' nominations
		}
		x.on, best.nominated = best, x
		q.log.Info("job nominated: the host holds the room that frees for it", "class", x.class, "host", best.name, "size", x.need.String(), "waited", waited.Round(time.Second).String())
	}
}

// roomier reports whether a has more room than b: more CPUs, then more
// memory.
func roomier(a, b Resources) bool {
	if a.CPUs != b.CPUs {
		return a.CPUs > b.CPUs
	}
	return a.MemoryMB > b.MemoryMB
}

// heldFor names the class of a job nominated to one of w's hosts, other than
// w, or "".
func (q *hostQueue) heldFor(w *waiter) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, h := range w.hosts {
		if n := h.nominated; n != nil && n != w {
			return n.class
		}
	}
	return ""
}

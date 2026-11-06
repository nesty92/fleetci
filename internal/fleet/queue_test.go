package fleet

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	clocktesting "k8s.io/utils/clock/testing"
)

// hostPools is a backend whose classes share a host of the given budget,
// on a fake clock.
func hostPools(t *testing.T, budget Resources, classes map[string]Class) (*Backend, *clocktesting.FakeClock) {
	t.Helper()
	q, err := newHostQueue(&HostConfig{CPUs: budget.CPUs, Memory: Size(budget.MemoryMB)}, classes)
	if err != nil {
		t.Fatal(err)
	}
	clk := clocktesting.NewFakeClock(time.Now())
	if q != nil {
		q.clock = clk
	}
	b := &Backend{log: hclog.NewNullLogger(), vms: map[string]*pool{}, pools: map[string][]*pool{}}
	for name, c := range classes {
		p := &pool{name: name, class: c, ts: &fakeScaler{free: c.MaxInstances}, timeout: 5 * time.Second}
		if q != nil && c.OnHost() {
			p.queue, p.host, p.dynamic = q, q.hosts[localHost], !c.Reuses()
			p.size, _ = c.Size()
		}
		b.pools[name] = []*pool{p}
	}
	return b, clk
}

func smallAndBig() map[string]Class {
	return map[string]Class{
		"small": onHostClass(1, 2048, 8),
		"big":   onHostClass(4, 8192, 2),
	}
}

// onlyWakes makes a release the only thing that wakes a waiting job.
func onlyWakes(t *testing.T) {
	overflowPoll = time.Hour
	t.Cleanup(func() { overflowPoll = time.Second })
}

// fourSmallsRunning is a 4-cpu host running four 1-cpu jobs, with a 4-cpu
// job waiting behind them.
func fourSmallsRunning(t *testing.T, classes map[string]Class) (*Backend, *clocktesting.FakeClock, []VM, <-chan VM) {
	t.Helper()
	onlyWakes(t)
	b, clk := hostPools(t, Resources{CPUs: 4, MemoryMB: 8192}, classes)
	ctx := context.Background()
	var smalls []VM
	for range 4 {
		vm, err := b.Create(ctx, "small")
		if err != nil {
			t.Fatal(err)
		}
		smalls = append(smalls, vm)
	}
	big := waitingJob(t, b, "big")
	waitFor(t, func() bool { return blocked(b.pools["big"][0].queue) == 1 })
	return b, clk, smalls, big
}

// waitingJob asks for a VM of class in the background. The test's end
// stops it waiting.
func waitingJob(t *testing.T, b *Backend, class string) <-chan VM {
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan VM, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		vm, err := b.Create(ctx, class)
		if err == nil {
			got <- vm
		} else if ctx.Err() == nil {
			t.Error(err)
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	return got
}

// startsNow asks for a VM of class and reports whether it got one at once.
func startsNow(t *testing.T, b *Backend, class string) (VM, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	vm, err := b.Create(ctx, class)
	return vm, err == nil
}

func got(t *testing.T, ch <-chan VM, class string) VM {
	t.Helper()
	select {
	case vm := <-ch:
		if vm.Class != class {
			t.Fatalf("got a %s vm, want %s", vm.Class, class)
		}
		return vm
	case <-time.After(2 * time.Second):
		t.Fatalf("the waiting %s job never started", class)
	}
	return VM{}
}

func TestTheBudgetIsNeverExceeded(t *testing.T) {
	b, _ := hostPools(t, Resources{CPUs: 8, MemoryMB: 16384}, map[string]Class{
		"small": onHostClass(1, 2048, 8),
		"big":   onHostClass(4, 8192, 2),
	})
	q, h := b.pools["small"][0].queue, b.pools["small"][0].host
	ctx := context.Background()
	var wg sync.WaitGroup
	var peak Resources
	for i := range 40 {
		class := "small"
		if i%3 == 0 {
			class = "big"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			vm, err := b.Create(ctx, class)
			if err != nil {
				t.Error(err)
				return
			}
			q.mu.Lock()
			if !h.used.fits(h.Resources) {
				t.Errorf("over the budget: %v of %v", h.used, h.Resources)
			}
			peak = Resources{max(peak.CPUs, h.used.CPUs), max(peak.MemoryMB, h.used.MemoryMB)}
			q.mu.Unlock()
			time.Sleep(time.Millisecond)
			_ = b.Delete(ctx, vm.ID)
		}()
	}
	wg.Wait()
	if h.used != (Resources{}) || len(q.waiting) != 0 || h.nominated != nil {
		t.Errorf("after all jobs: used %v, %d waiting, nominated %v", h.used, len(q.waiting), h.nominated)
	}
	if peak.CPUs < 4 {
		t.Errorf("peak %v: the budget was never used", peak)
	}
}

func TestSmallJobsFillWhileABigOneWaits(t *testing.T) {
	b, clk, smalls, _ := fourSmallsRunning(t, smallAndBig())
	clk.Step(defaultNominateAfter - time.Second)
	_ = b.Delete(context.Background(), smalls[0].ID)
	if _, ok := startsNow(t, b, "small"); !ok {
		t.Fatal("a small job that fits waited behind a big one that isn't nominated yet")
	}
}

func TestALateBigJobIsNominatedAndStarts(t *testing.T) {
	b, clk, smalls, big := fourSmallsRunning(t, smallAndBig())
	ctx := context.Background()
	clk.Step(defaultNominateAfter)

	// The room that frees is held for the big job: no later job takes it.
	_ = b.Delete(ctx, smalls[0].ID)
	if vm, ok := startsNow(t, b, "small"); ok {
		t.Fatalf("a later small job took %s while the big one was nominated", vm.Class)
	}
	if n := b.pools["big"][0].host.nominated; n == nil || n.class != "big" {
		t.Fatalf("nominated %v, want the big job", n)
	}
	for _, vm := range smalls[1:] {
		_ = b.Delete(ctx, vm.ID)
	}
	vm := got(t, big, "big")
	if n := b.pools["big"][0].host.nominated; n != nil {
		t.Fatalf("still nominated after it started: %v", n.class)
	}
	_ = b.Delete(ctx, vm.ID)
	if _, ok := startsNow(t, b, "small"); !ok {
		t.Fatal("a small job didn't start once the big one ended")
	}
}

func TestNominateAfterZeroHoldsRoomAtOnce(t *testing.T) {
	b, _, smalls, _ := fourSmallsRunning(t, smallAndBig())
	b.pools["big"][0].queue.NominateAfter = 0
	_ = b.Delete(context.Background(), smalls[0].ID)
	if _, ok := startsNow(t, b, "small"); ok {
		t.Fatal("with nominate_after 0 a later small job went before the waiting big one")
	}
}

func TestRoomGoesToTheEarliestJobItFits(t *testing.T) {
	onlyWakes(t)
	for range 20 {
		b, _ := hostPools(t, Resources{CPUs: 4, MemoryMB: 8192}, map[string]Class{
			"big": onHostClass(4, 8192, 2),
			"mid": onHostClass(2, 4096, 2),
		})
		q := b.pools["big"][0].queue
		first, _ := b.Create(context.Background(), "big")
		big := waitingJob(t, b, "big")
		waitFor(t, func() bool { return blocked(q) == 1 })
		mid := waitingJob(t, b, "mid")
		waitFor(t, func() bool { return blocked(q) == 2 })

		_ = b.Delete(context.Background(), first.ID)
		got(t, big, "big")
		select {
		case <-mid:
			t.Fatal("the later mid job started beside the big one on a 4-cpu host")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestAJobBehindANominatedOneOverflows(t *testing.T) {
	classes := smallAndBig()
	small := classes["small"]
	small.Overflow = "small-aws"
	classes["small"] = small
	classes["small-aws"] = Class{Plugin: "fleeting-plugin-aws", MaxInstances: 4}
	b, clk, smalls, _ := fourSmallsRunning(t, classes)
	ctx := context.Background()

	_ = b.Delete(ctx, smalls[0].ID)
	if vm, err := b.Create(ctx, "small"); err != nil || vm.Class != "small" {
		t.Fatalf("before the big job is nominated: %q %v, want its own class", vm.Class, err)
	}
	_ = b.Delete(ctx, smalls[1].ID)
	clk.Step(defaultNominateAfter)
	if vm, err := b.Create(ctx, "small"); err != nil || vm.Class != "small-aws" {
		t.Fatalf("behind the nominated big job: %q %v, want its overflow", vm.Class, err)
	}
}

func TestAReleaseWakesAWaitingJob(t *testing.T) {
	onlyWakes(t)
	b, _ := hostPools(t, Resources{CPUs: 2, MemoryMB: 4096}, map[string]Class{"vm": onHostClass(2, 4096, 4)})
	first, _ := b.Create(context.Background(), "vm")
	next := waitingJob(t, b, "vm")
	waitFor(t, func() bool { return blocked(b.pools["vm"][0].queue) == 1 })
	released := time.Now()
	_ = b.Delete(context.Background(), first.ID)
	got(t, next, "vm")
	if d := time.Since(released); d > 500*time.Millisecond {
		t.Errorf("woke after %v", d)
	}
}

func TestNoRoomSaysWhy(t *testing.T) {
	b, clk := hostPools(t, Resources{CPUs: 2, MemoryMB: 4096}, map[string]Class{
		"vm":  onHostClass(2, 4096, 4),
		"one": onHostClass(1, 1024, 4),
	})
	b.pools["vm"][0].timeout = 50 * time.Millisecond
	b.pools["one"][0].timeout = 50 * time.Millisecond
	ctx := context.Background()
	if _, err := b.Create(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	_, err := b.Create(ctx, "vm")
	if err == nil || !strings.Contains(err.Error(), "or none of its hosts has room for one more of its VMs (cpus=2 memory=4GB; host local: cpus=1 memory=1GB in use of cpus=2 memory=4GB)") {
		t.Fatalf("err %v", err)
	}
	if n := len(b.pools["vm"][0].queue.waiting); n != 0 {
		t.Fatalf("%d still waiting after the timeout", n)
	}

	// A one-cpu job would fit, but the host holds its room for a vm job.
	b.pools["vm"][0].timeout = time.Minute
	_ = waitingJob(t, b, "vm")
	waitFor(t, func() bool { return blocked(b.pools["vm"][0].queue) == 1 })
	clk.Step(defaultNominateAfter)
	_, err = b.Create(ctx, "one")
	if err == nil || !strings.Contains(err.Error(), "as room is held for a vm job that has waited past nominate_after") {
		t.Fatalf("held: %v", err)
	}
}

// The rules for more than one host, on the queue itself.

func twoHosts(freeA, freeB int) (*hostQueue, *host, *host, *clocktesting.FakeClock) {
	clk := clocktesting.NewFakeClock(time.Now())
	q := emptyQueue(10 * time.Minute)
	q.clock = clk
	a := &host{name: "a", Budget: Budget{Resources: Resources{CPUs: 8, MemoryMB: 16384}}}
	b := &host{name: "b", Budget: Budget{Resources: Resources{CPUs: 8, MemoryMB: 16384}}}
	a.used = Resources{CPUs: 8 - freeA, MemoryMB: int64(8-freeA) * 2048}
	b.used = Resources{CPUs: 8 - freeB, MemoryMB: int64(8-freeB) * 2048}
	q.hosts["a"], q.hosts["b"] = a, b
	return q, a, b, clk
}

func cpus(n int) Resources { return Resources{CPUs: n, MemoryMB: int64(n) * 2048} }

// late queues a job that found no room on hosts.
func late(q *hostQueue, clk *clocktesting.FakeClock, class string, need Resources, hosts ...*host) *waiter {
	w := q.enqueue(class, need, hosts...)
	w.blocked = true
	return w
}

func TestNominationsGoToTheRoomiestFreeHost(t *testing.T) {
	q, a, b, clk := twoHosts(2, 3)
	x := late(q, clk, "x", cpus(4), a, b)
	y := late(q, clk, "y", cpus(4), a, b)
	z := late(q, clk, "z", cpus(4), a, b)
	clk.Step(10 * time.Minute)
	q.mu.Lock()
	defer q.mu.Unlock()
	q.nominate()
	if x.on != b || y.on != a || z.on != nil {
		t.Fatalf("x on %v, y on %v, z on %v: want the oldest on b (3 free), the next on a, the third on none", x.on, y.on, z.on)
	}
}

func TestANominationHoldsOnlyItsHost(t *testing.T) {
	q, a, b, clk := twoHosts(3, 3)
	late(q, clk, "x", cpus(4), b)
	clk.Step(10 * time.Minute)
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.fits(b, nil, cpus(1)) {
		t.Error("a small job fit on b, whose room is held for x")
	}
	if !q.fits(a, nil, cpus(1)) {
		t.Error("a small job didn't fit on a, which holds no nomination")
	}
}

func TestANominatedJobMayStartOnAnotherHost(t *testing.T) {
	q, a, b, clk := twoHosts(2, 3)
	x := late(q, clk, "x", cpus(4), a, b)
	clk.Step(10 * time.Minute)
	q.mu.Lock()
	defer q.mu.Unlock()
	q.nominate()
	if x.on != b {
		t.Fatalf("x on %v, want b", x.on)
	}
	a.used = a.used.add(cpus(-2)) // room frees on a first
	if !q.fits(a, x, x.need) {
		t.Fatal("x doesn't fit a, which now has room for it")
	}
	q.started(x)
	if b.nominated != nil || len(q.waiting) != 0 {
		t.Fatalf("b still holds %v after x started on a", b.nominated)
	}
}

func TestNoNominationToAHostThatNeverFits(t *testing.T) {
	q, a, b, clk := twoHosts(2, 3)
	b.fixed = cpus(6) // warm VMs: b never has 4 cpus for x
	x := late(q, clk, "x", cpus(4), a, b)
	clk.Step(10 * time.Minute)
	q.mu.Lock()
	defer q.mu.Unlock()
	q.nominate()
	if x.on != a {
		t.Fatalf("x on %v, want a: b's fixed shares leave no room for it", x.on)
	}
}

func TestAnEarlierJobThatFitsGoesFirst(t *testing.T) {
	q, a, _, clk := twoHosts(2, 0)
	late(q, clk, "x", cpus(4), a) // doesn't fit: later jobs may pass it
	late(q, clk, "y", cpus(2), a) // fits: later jobs wait for it
	w := late(q, clk, "w", cpus(2), a)
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.fits(a, w, w.need) {
		t.Fatal("w took the room of y, which came before it and fits")
	}
	if !q.fits(a, q.waiting[1], cpus(2)) {
		t.Fatal("y doesn't fit: x, which doesn't fit, held it up")
	}
}

func TestOnlyLateJobsAreNominated(t *testing.T) {
	q, a, _, clk := twoHosts(2, 2)
	x := late(q, clk, "x", cpus(4), a)
	clk.Step(10*time.Minute - time.Second)
	q.mu.Lock()
	defer q.mu.Unlock()
	q.nominate()
	if x.on != nil {
		t.Fatal("nominated before nominate_after")
	}
	clk.Step(time.Second)
	q.nominate()
	if x.on != a {
		t.Fatal("not nominated at nominate_after")
	}
}

// spanning is a backend whose class vm (1 cpu) runs on hosts a and b, with
// a 4 cpus in use and b idle.
func spanning(t *testing.T, placement string) (*Backend, *host, *host) {
	t.Helper()
	q, a, h2, _ := twoHosts(4, 8)
	q.Placement = placement
	c := onHostClass(1, 2048, 8)
	size, _ := c.Size()
	b := &Backend{log: hclog.NewNullLogger(), vms: map[string]*pool{}, pools: map[string][]*pool{}}
	for _, h := range []*host{a, h2} {
		b.pools["vm"] = append(b.pools["vm"], &pool{name: "vm", class: c, ts: &fakeScaler{free: c.MaxInstances},
			timeout: 100 * time.Millisecond, queue: q, host: h, size: size, dynamic: true})
	}
	return b, a, h2
}

func TestPlacement(t *testing.T) {
	for _, tc := range []struct {
		placement string
		want      string
	}{{Pack, "a"}, {Spread, "b"}} {
		b, a, h2 := spanning(t, tc.placement)
		vm, err := b.Create(context.Background(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		on := map[bool]string{true: "a", false: "b"}[a.used.CPUs == 5]
		if on != tc.want {
			t.Errorf("%s: the job went to host %s (a %v, b %v), want %s", tc.placement, on, a.used, h2.used, tc.want)
		}
		_ = b.Delete(context.Background(), vm.ID)
		if a.used.CPUs != 4 || h2.used.CPUs != 0 {
			t.Errorf("%s: after the release a %v, b %v", tc.placement, a.used, h2.used)
		}
	}
}

func TestAJobGoesToAnotherHostOfItsClass(t *testing.T) {
	b, a, h2 := spanning(t, Pack)
	ctx := context.Background()
	for range 12 { // a has room for 4, then b for 8
		if _, err := b.Create(ctx, "vm"); err != nil {
			t.Fatal(err)
		}
	}
	if a.used.CPUs != 8 || h2.used.CPUs != 8 {
		t.Fatalf("a %v, b %v: want both full", a.used, h2.used)
	}
	if _, err := b.Create(ctx, "vm"); err == nil || !strings.Contains(err.Error(), "host a: cpus=8 memory=16GB in use of cpus=8 memory=16GB; host b: cpus=8") {
		t.Fatalf("both hosts full: %v", err)
	}
}

func TestEarlierJobsArePlacedAsTheirsWouldBe(t *testing.T) {
	for _, tc := range []struct {
		placement string
		fits      bool
	}{{Pack, true}, {Spread, false}} {
		q, a, b, clk := twoHosts(2, 3)
		q.Placement = tc.placement
		late(q, clk, "x", cpus(2), a, b) // pack puts it on a, spread on b
		w := late(q, clk, "w", cpus(3), b)
		q.mu.Lock()
		got := q.fits(b, w, w.need)
		q.mu.Unlock()
		if got != tc.fits {
			t.Errorf("%s: w fits b: %v, want %v", tc.placement, got, tc.fits)
		}
	}
}

func blocked(q *hostQueue) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, w := range q.waiting {
		if w.blocked {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition never held")
}

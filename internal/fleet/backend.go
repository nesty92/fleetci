// Package fleet runs one fleeting plugin and one taskscaler per class. A job
// acquires a VM from its class; when the job ends the VM is released, and
// since each VM is used once, fleeting removes it.
package fleet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-hclog"

	"gitlab.com/gitlab-org/fleeting/fleeting"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"gitlab.com/gitlab-org/fleeting/taskscaler"
	"gitlab.com/gitlab-org/fleeting/taskscaler/storage"

	"github.com/nesty92/fleetci/internal/proxy"
	"github.com/nesty92/fleetci/internal/vmjob"
)

// VM is a VM acquired for one job.
type VM struct {
	ID    string
	Class string
	// Instance is the plugin's id for the machine behind the VM.
	Instance string
	// Conn is the machine's one SSH connection, shared with its other
	// jobs and its reclaim watch. Users open channels on it and never
	// close it.
	Conn *proxy.Conn
}

type pool struct {
	name    string
	class   Class
	group   *supervised
	reclaim *reclaimWatch // nil when the class isn't watched
	conns   *vmConns      // one shared connection per instance
	died    *deaths       // its instances that went without being removed
	ts      taskscaler.Taskscaler
	timeout time.Duration
	// stored: the class's taskscaler state is kept on disk, so its VMs
	// can be handed over to the next fleetci. ready closes once ts is set.
	stored bool
	ready  chan struct{}

	// host is the host this pool's VMs run on and count against, or nil: a
	// class has a pool on each host it runs on. queue is the hosts' queue. A dynamic class takes size from its host
	// for each job; the others hold a fixed share (see Class.Reserved).
	queue   *hostQueue
	host    *host
	size    Resources
	dynamic bool

	mu   sync.Mutex
	jobs int // jobs on the class's VMs or waiting for one
}

// take counts a job in on one of a class's pools, one per host it runs on:
// of those with a free slot whose host has room for one more of its VMs,
// the one the queue's placement prefers (see hostQueue.score). w is the job's place
// in the queue, or nil. It returns the pool, or nil.
//
// A pool's slots are its class's max_instances; what max_instances means
// for a class on more than one host is decided with multi-host.
func take(w *waiter, pools []*pool) *pool {
	if len(pools) == 0 {
		return nil
	}
	if !pools[0].dynamic {
		for _, p := range pools {
			if p.takeSlot() {
				return p
			}
		}
		return nil
	}
	q := pools[0].queue
	q.mu.Lock()
	defer q.mu.Unlock()
	taken := q.room(w)
	var best *pool
	var bestScore float64
	slot := false
	for _, p := range pools {
		p.mu.Lock()
		free := p.jobs < p.class.Slots()
		p.mu.Unlock()
		if !free {
			continue
		}
		slot = true
		after := taken[p.host].add(p.size)
		if !after.fits(p.host.Resources) {
			continue
		}
		if score := q.score(after, p.host.Resources); best == nil || score > bestScore {
			best, bestScore = p, score
		}
	}
	if best == nil {
		if slot && w != nil {
			w.blocked = true
		}
		return nil
	}
	// q.mu, held, keeps the slot free: done gives one back under it.
	best.mu.Lock()
	best.jobs++
	best.mu.Unlock()
	best.host.used = best.host.used.add(best.size)
	if w != nil {
		q.started(w)
	}
	return best
}

// takeSlot counts a job in if the pool has a free slot.
func (p *pool) takeSlot() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.jobs >= p.class.Slots() {
		return false
	}
	p.jobs++
	return true
}

func (p *pool) done() {
	if p.dynamic {
		p.queue.mu.Lock()
		p.host.used = p.host.used.add(p.size.times(-1))
		defer p.queue.mu.Unlock()
	}
	p.mu.Lock()
	p.jobs--
	p.mu.Unlock()
}

type Backend struct {
	log      hclog.Logger
	pools    map[string][]*pool // class → its pools, one per host it runs on
	shutdown sync.Once

	mu  sync.Mutex
	vms map[string]*pool // VM id (= acquisition key) → its pool
	// instance, by VM id: the machine each VM is on. handed and adopted
	// are VMs left to the next fleetci: handed over by a job source, and
	// taken from an earlier fleetci and still running their job.
	instance map[string]string
	handed   map[string]string
	adopted  map[string]string

	stateDir string
	// ctx ends when the backend stops, and with it adopted jobs' follows.
	ctx    context.Context
	cancel context.CancelFunc

	// wake is closed, and replaced, when a job ends, so jobs waiting for
	// room look again at once.
	wakeMu sync.Mutex
	wake   chan struct{}
}

func (b *Backend) woken() <-chan struct{} {
	b.wakeMu.Lock()
	defer b.wakeMu.Unlock()
	if b.wake == nil {
		b.wake = make(chan struct{})
	}
	return b.wake
}

func (b *Backend) notify() {
	b.wakeMu.Lock()
	defer b.wakeMu.Unlock()
	if b.wake != nil {
		close(b.wake)
	}
	b.wake = make(chan struct{})
}

// Option configures New.
type Option func(*Backend)

// WithStateDir keeps the taskscaler state of each class whose VMs aren't
// on this host in dir. A fleetci started on it later adopts the VMs this
// one left running jobs, and follows the jobs to their end (see Stop).
func WithStateDir(dir string) Option {
	return func(b *Backend) { b.stateDir = dir }
}

// New starts every class's plugin and taskscaler. host is fleetci.toml's
// [host], or nil for its defaults.
func New(ctx context.Context, classes map[string]Class, host *HostConfig, log hclog.Logger, opts ...Option) (*Backend, error) {
	b := &Backend{log: log, pools: map[string][]*pool{}, vms: map[string]*pool{}}
	for _, o := range opts {
		o(b)
	}
	b.ctx, b.cancel = context.WithCancel(context.WithoutCancel(ctx))
	hq, err := newHostQueue(host, classes)
	if err != nil {
		return nil, err
	}
	if hq != nil {
		hq.log = log
		local := hq.hosts[localHost]
		log.Info("host budget", "host", local.name, "budget", local.Budget.String(), "reserved", local.fixed.String(), "nominate_after", hq.NominateAfter.String(), "placement", hq.Placement)
	}
	groups := map[string]string{} // group id → class
	for _, name := range slices.Sorted(maps.Keys(classes)) {
		c := classes[name]
		if err := c.validate(name); err != nil {
			_ = b.Shutdown(ctx)
			return nil, err
		}
		st, err := b.storage(name, c)
		if err != nil {
			_ = b.Shutdown(ctx)
			return nil, fmt.Errorf("class %q: %w", name, err)
		}
		p, err := startPool(ctx, name, c, groups, st, b.adopt, log.Named("class."+name))
		if err != nil {
			_ = b.Shutdown(ctx)
			return nil, fmt.Errorf("class %q: %w", name, err)
		}
		if hq != nil && c.OnHost() {
			p.queue, p.host, p.dynamic = hq, hq.hosts[localHost], !c.Reuses()
			p.size, _ = c.Size() // checked by newHostQueue
		}
		b.pools[name] = []*pool{p}
	}
	for name := range b.pools {
		if err := checkOverflow(name, classes); err != nil {
			_ = b.Shutdown(ctx)
			return nil, err
		}
	}
	return b, nil
}

// storage is where a class's taskscaler state is kept, or nil. A class on
// this host has none: its VMs end with fleetci's plugin.
func (b *Backend) storage(name string, c Class) (storage.Storage, error) {
	if b.stateDir == "" {
		return nil, nil
	}
	if c.OnHost() {
		b.log.Info("class runs on this host: its VMs, and their jobs, end with fleetci", "class", name)
		return nil, nil
	}
	return storage.NewFileStorage(filepath.Join(b.stateDir, "classes", name))
}

// localHost is the name of the machine fleetci runs on, the one host there
// is until classes can run on others.
const localHost = "local"

// newHostQueue works out the host's budget and the fixed shares of its
// classes, or nil when there is none. Every class on the host must fit.
func newHostQueue(h *HostConfig, classes map[string]Class) (*hostQueue, error) {
	budget, on, err := HostBudget(h, classes)
	if err != nil || !on {
		return nil, err
	}
	local := &host{name: localHost, Budget: budget}
	var largest Resources
	var largestName string
	for _, name := range slices.Sorted(maps.Keys(classes)) {
		c := classes[name]
		if !c.OnHost() {
			continue
		}
		size, err := c.Size()
		if err != nil {
			return nil, fmt.Errorf("class %q: %w", name, err)
		}
		local.fixed = local.fixed.add(c.Reserved(size))
		if !c.Reuses() && !size.fits(largest) {
			largest, largestName = Resources{max(size.CPUs, largest.CPUs), max(size.MemoryMB, largest.MemoryMB)}, name
		}
	}
	if !local.fixed.fits(local.Resources) {
		return nil, fmt.Errorf("host: the classes' fixed shares (warm and reused VMs, %s) are more than the budget (%s)", local.fixed, budget)
	}
	if largestName != "" && !local.fixed.add(largest).fits(local.Resources) {
		return nil, fmt.Errorf("host: a VM of class %q (%s) never fits: the budget is %s and the fixed shares take %s", largestName, largest, budget, local.fixed)
	}
	local.used = local.fixed
	q := emptyQueue(budget.NominateAfter)
	q.Placement = budget.Placement
	q.hosts[localHost] = local
	return q, nil
}

// checkOverflow checks that the class's overflow is another class with no
// overflow of its own.
func checkOverflow(name string, classes map[string]Class) error {
	o := classes[name].Overflow
	if o == "" {
		return nil
	}
	oc, ok := classes[o]
	switch {
	case o == name:
		return fmt.Errorf("class %q: overflow can't be the class itself", name)
	case !ok:
		return fmt.Errorf("class %q: overflow class %q doesn't exist", name, o)
	case oc.Overflow != "":
		return fmt.Errorf("class %q: overflow class %q has an overflow of its own (%q); one step only", name, o, oc.Overflow)
	case classes[name].Arch() != "" && oc.Arch() != "" && classes[name].Arch() != oc.Arch():
		return fmt.Errorf("class %q is %s, its overflow %q %s", name, classes[name].Arch(), o, oc.Arch())
	}
	return nil
}

// startPool starts the class's plugin and then its taskscaler. The plugin is
// initialised first, so a class whose group another class already uses is
// refused before a second taskscaler can see the group: each would remove
// the other's VMs as unknown.
//
// With st, the class's state is kept there, and adopt is called for each
// job an earlier fleetci left running on one of its VMs.
func startPool(ctx context.Context, name string, c Class, groups map[string]string, st storage.Storage, adopt func(p *pool, key, instance string), log hclog.Logger) (*pool, error) {
	settings, err := c.Connector.settings()
	if err != nil {
		return nil, err
	}

	cfg := []byte(c.PluginConfig)
	if len(cfg) == 0 {
		cfg = []byte("{}")
	}
	start := func(ctx context.Context, exited func()) (provider.InstanceGroup, provider.ProviderInfo, func(), error) {
		runner, err := fleeting.RunPlugin(c.Plugin, cfg, fleeting.WithPluginLogger(watchExit(log.Named("plugin"), exited)))
		if err != nil {
			return nil, provider.ProviderInfo{}, nil, fmt.Errorf("starting plugin %s: %w", c.Plugin, err)
		}
		info, err := runner.InstanceGroup().Init(ctx, log.Named("plugin"), settings)
		if err != nil {
			runner.Kill()
			return nil, provider.ProviderInfo{}, nil, fmt.Errorf("initialising plugin %s: %w", c.Plugin, err)
		}
		return runner.InstanceGroup(), info, runner.Kill, nil
	}
	group, info, err := supervise(ctx, start, log.Named("plugin"))
	if err != nil {
		return nil, err
	}
	if other, ok := groups[info.ID]; ok {
		group.Close()
		return nil, fmt.Errorf("uses group %s, as class %q does; each class needs a group of its own", info.ID, other)
	}
	groups[info.ID] = name

	conns := newVMConns(group, c.Connector.UseExternalAddr, log.Named("conn"))
	p := &pool{name: name, class: c, group: group, conns: conns, timeout: c.acquireTimeout(), stored: st != nil, ready: make(chan struct{})}
	ready := dockerReady(group, c.Connector.UseExternalAddr, log)
	if st != nil {
		ready = adoptUp(dockerUp(group, c.Connector.UseExternalAddr, log), func(key, instance string) { adopt(p, key, instance) })
	}
	p.died = newDeaths(log)
	scaled, up, reclaim := withReclaim(c, group, conns, p.died, ready, sshTunnel(conns), log)
	p.reclaim = reclaim
	stop := func() {
		if reclaim != nil {
			reclaim.close()
		}
		conns.close()
		group.Close()
	}

	ts, err := taskscaler.New(ctx, initialised{scaled, info}, scalerOptions(c, settings, up, st, log)...)
	if err != nil {
		stop()
		return nil, fmt.Errorf("starting taskscaler: %w", err)
	}

	if err := ts.ConfigureSchedule(c.schedules()...); err != nil {
		ts.Shutdown(ctx)
		stop()
		return nil, err
	}

	p.ts = ts
	close(p.ready)
	log.Info("class ready", "group", info.ID, "plugin", c.Plugin, "max_instances", c.MaxInstances, "policies", len(c.schedules()), "state_kept", st != nil)
	return p, nil
}

// withReclaim wraps a class's group so a gone instance's connection is
// dropped and, for a class watched for reclaim, so the heartbeat of a VM
// with a notice fails and up starts watching a VM once it is admitted.
// reclaim is nil for a class that isn't watched.
func withReclaim(c Class, group provider.InstanceGroup, conns *vmConns, died *deaths, up taskscaler.UpFunc, open opener, log hclog.Logger) (provider.InstanceGroup, taskscaler.UpFunc, *reclaimWatch) {
	if !c.watchesReclaim() {
		return vmGroup{group, conns, nil, died}, up, nil
	}
	w := newReclaimWatch(open, c.Reclaim.Rebalance, log.Named("reclaim"))
	return vmGroup{group, conns, w, died}, watchOnceUp(up, w), w
}

// watchOnceUp starts watching an instance for reclaim once up admits it.
func watchOnceUp(up taskscaler.UpFunc, w *reclaimWatch) taskscaler.UpFunc {
	return func(ts taskscaler.Taskscaler, inst taskscaler.UpFuncInstance) error {
		if err := up(ts, inst); err != nil {
			return err
		}
		w.start(inst.ID)
		return nil
	}
}

// scalerOptions configures a class's taskscaler; up readies a new instance.
// With st, its state is kept there and its VMs outlive it; without, its
// shutdown removes them.
func scalerOptions(c Class, settings provider.Settings, up taskscaler.UpFunc, st storage.Storage, log hclog.Logger) []taskscaler.Option {
	keep := taskscaler.WithDeleteInstancesOnShutdown()
	if st != nil {
		keep = taskscaler.WithStorage(st)
	}
	return []taskscaler.Option{
		keep,
		taskscaler.WithLogger(log.Named("taskscaler")),
		taskscaler.WithInstanceGroupSettings(settings),
		taskscaler.WithCapacityPerInstance(c.capacity()),
		taskscaler.WithMaxUseCount(c.useCount()),
		taskscaler.WithMaxInstances(c.MaxInstances),
		taskscaler.WithInstanceUpFunc(up),
		// A job is waiting while instances are expected: look often. Each
		// look is a list call to the plugin: for a class on this host a look
		// at the plugin's own memory, every quarter second; for a cloud one
		// an API call, every second.
		taskscaler.WithUpdateIntervalWhenExpecting(expectingInterval(c)),
		// Otherwise it looks every minute, the default, for a cloud class
		// an API call. On this host a look is the plugin's own memory: look
		// every few seconds, so a VM that died is replaced, and a job
		// waiting on it goes on, within seconds.
		taskscaler.WithUpdateInterval(updateInterval(c)),
		// A job waiting for a VM looks for a ready one this often. The look
		// is in taskscaler's memory, and the default two seconds was most
		// of a restored VM's wait.
		taskscaler.WithAcquireDelay(100 * time.Millisecond),
		// Without a heartbeat function taskscaler never asks the plugin about
		// an instance before placing a job on it. With one, every placement
		// first calls the plugin's Heartbeat, and a failure skips the
		// instance: the AWS plugin fails it for a spot instance the cloud has
		// marked for reclaim, dockervm for a container that stopped. The
		// instance is removed only once it holds no job, so running jobs are
		// never cut short. The function itself has nothing to add: Prepare
		// connects to the VM anyway.
		taskscaler.WithHeartbeatFunc(func(context.Context, provider.ConnectInfo) error { return nil }),
	}
}

// updateInterval is how often taskscaler lists a class's instances when
// it expects none: 0 for taskscaler's default.
func updateInterval(c Class) time.Duration {
	if c.OnHost() {
		return 5 * time.Second
	}
	return 0
}

// expectingInterval is how often taskscaler lists a class's instances
// while it waits for new ones.
func expectingInterval(c Class) time.Duration {
	if c.OnHost() {
		return 250 * time.Millisecond
	}
	return time.Second
}

// initialised is an instance group whose Init already ran; taskscaler's own
// call gets that result instead of initialising the plugin twice.
type initialised struct {
	provider.InstanceGroup
	info provider.ProviderInfo
}

func (g initialised) Init(context.Context, hclog.Logger, provider.Settings) (provider.ProviderInfo, error) {
	return g.info, nil
}

// Has reports whether a class exists.
// Died is how many of a class's instances have gone without fleetci
// removing them: VMs that died, cloud instances taken away.
func (b *Backend) Died(class string) int64 {
	var n int64
	for p := range b.allPools() {
		if p.name == class {
			n += p.died.count()
		}
	}
	return n
}

// Idle is how many of a class's VMs are booted and hold no job: ones
// taskscaler started for a wait that ended before they were ready, and
// keeps until the class's idle_time. They take room on the host all the
// same.
func (b *Backend) Idle(class string) int {
	n := 0
	for p := range b.allPools() {
		if p.name != class {
			continue
		}
		select {
		case <-p.ready:
		default:
			continue
		}
		if p.ts == nil {
			continue
		}
		available, _ := p.ts.Capacity()
		n += available / p.class.capacity()
	}
	return n
}

func (b *Backend) Has(class string) bool {
	_, ok := b.pools[class]
	return ok
}

// Classes lists the class names, sorted.
func (b *Backend) Classes() []string {
	names := make([]string, 0, len(b.pools))
	for name := range b.pools {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// overflowPoll is how often a job whose class and overflow are both full
// looks for room in either.
var overflowPoll = time.Second

// Create acquires a VM of the class, waiting for one to boot if none is idle.
// While all of the class's VMs are in use, its overflow class serves the job;
// when both are full, the job takes the first room in either, its own class
// first.
func (b *Backend) Create(ctx context.Context, class string) (VM, error) {
	pools := b.pools[class]
	if len(pools) == 0 {
		return VM{}, fmt.Errorf("unknown class %q", class)
	}
	first := pools[0]
	start := time.Now()
	actx, cancel := context.WithTimeout(ctx, first.timeout)
	defer cancel()

	overflow := b.pools[first.class.Overflow]
	var p *pool
	if len(pools) == 1 && overflow == nil && !first.dynamic {
		// One pool, no overflow and no budget: taskscaler queues the job
		// itself.
		p = first
		p.mu.Lock()
		p.jobs++
		p.mu.Unlock()
	} else {
		// fleetci holds the job until its class, its overflow or a host
		// has room, its own class first.
		var w *waiter
		if first.dynamic {
			hosts := make([]*host, len(pools))
			for i, p := range pools {
				hosts[i] = p.host
			}
			w = first.queue.enqueue(class, first.size, hosts...)
		}
		for p == nil {
			wake := b.woken()
			if p = take(w, pools); p == nil {
				p = take(w, overflow)
			}
			if p != nil {
				break
			}
			select {
			case <-actx.Done():
				held := ""
				if w != nil {
					held = first.queue.heldFor(w)
					first.queue.dequeue(w)
				}
				return VM{}, fmt.Errorf("acquiring a %s vm: %s: %w", class, full(class, pools, overflow, held), actx.Err())
			case <-wake:
			case <-time.After(overflowPoll):
			}
		}
		if w != nil {
			first.queue.dequeue(w)
		}
	}
	return b.acquire(actx, p, class, start)
}

// ErrNoRoom is Reserve's error when a class, its overflow and its hosts
// have no room for one more of its VMs.
var ErrNoRoom = errors.New("no room")

// Reservation is room for one VM of a class, taken by Reserve. Acquire
// turns it into the VM; Release gives it back unused.
type Reservation struct {
	b     *Backend
	p     *pool
	class string
	start time.Time
	used  atomic.Bool
}

// Reserve takes room for one VM of the class without waiting, behind the
// jobs already waiting in Create: gitlab-runner's loop asks for capacity
// this way before it asks GitLab for a job, so a job only leaves GitLab's
// queue once fleetci has room for it.
func (b *Backend) Reserve(class string) (*Reservation, error) {
	pools := b.pools[class]
	if len(pools) == 0 {
		return nil, fmt.Errorf("unknown class %q", class)
	}
	p := take(nil, pools)
	if p == nil {
		p = take(nil, b.pools[pools[0].class.Overflow])
	}
	if p == nil {
		return nil, ErrNoRoom
	}
	return &Reservation{b: b, p: p, class: class, start: time.Now()}, nil
}

// Acquire takes the reserved VM. Once called, the VM is Deleted, not
// Released, whether or not Acquire succeeds.
func (r *Reservation) Acquire(ctx context.Context) (VM, error) {
	if !r.used.CompareAndSwap(false, true) {
		return VM{}, errors.New("reservation already used")
	}
	actx, cancel := context.WithTimeout(ctx, r.p.timeout)
	defer cancel()
	return r.b.acquire(actx, r.p, r.class, r.start)
}

// Class is the class Reserve was asked for.
func (r *Reservation) Class() string { return r.class }

// Release gives the room back if Acquire was never called.
func (r *Reservation) Release() {
	if r.used.CompareAndSwap(false, true) {
		r.p.done()
		r.b.notify()
	}
}

// acquire takes a VM from p, where room for it is already counted.
func (b *Backend) acquire(actx context.Context, p *pool, class string, start time.Time) (VM, error) {
	id := "fleetci-" + p.name + "-" + randomHex(4)
	acq, err := p.ts.Acquire(actx, id)
	if err != nil {
		p.done()
		b.notify()
		return VM{}, fmt.Errorf("acquiring a %s vm: %w", p.name, err)
	}

	b.mu.Lock()
	b.vms[id] = p
	if p.stored {
		set(&b.instance, id, acq.InstanceID())
	}
	b.mu.Unlock()

	log := []any{"vm", id, "class", p.name, "instance", acq.InstanceID(), "waited", time.Since(start).Round(time.Millisecond).String()}
	if p.name != class {
		log = append(log, "overflow_of", class)
	}
	b.log.Info("vm acquired", log...)

	// The instance's one connection: every job on it, and its reclaim
	// watch, share it.
	return VM{ID: id, Class: p.name, Instance: acq.InstanceID(), Conn: p.conns.get(acq.InstanceID())}, nil
}

// full says why a job of the class found no room. held is the class of the
// nominated job a host held room for, if any.
func full(class string, pools, overflow []*pool, held string) string {
	why := class + " is full"
	if len(overflow) > 0 {
		why = fmt.Sprintf("%s and its overflow %s are full", class, overflow[0].name)
	}
	if first := pools[0]; first.dynamic {
		var hosts []string
		first.queue.mu.Lock()
		for _, p := range pools {
			hosts = append(hosts, fmt.Sprintf("host %s: %s in use of %s", p.host.name, p.host.used, p.host.Resources))
		}
		first.queue.mu.Unlock()
		why += fmt.Sprintf(", or none of its hosts has room for one more of its VMs (%s; %s)", first.size, strings.Join(hosts, "; "))
		if held != "" {
			why += fmt.Sprintf(", as room is held for a %s job that has waited past nominate_after", held)
		}
	}
	return why
}

// Delete releases the VM. It was used once, so fleeting removes it.
func (b *Backend) Delete(_ context.Context, id string) error {
	b.mu.Lock()
	p, ok := b.vms[id]
	delete(b.vms, id)
	delete(b.instance, id)
	delete(b.adopted, id)
	b.mu.Unlock()
	if !ok {
		return nil
	}
	p.ts.Release(id)
	p.done()
	b.notify()
	b.log.Info("vm released", "vm", id, "class", p.name)
	return nil
}

// HandOver leaves the VM, and the job running on it, to the next fleetci:
// Stop removes neither, the VM's state stays held, and the next fleetci
// adopts the VM and follows its job to its end. It is false for a class
// whose state isn't kept; the caller then Deletes the VM as usual.
func (b *Backend) HandOver(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.vms[id]
	if !ok || !p.stored {
		return false
	}
	delete(b.vms, id)
	set(&b.handed, id, b.instance[id])
	delete(b.instance, id)
	b.log.Info("vm left to the next fleetci with its job", "vm", id, "class", p.name)
	return true
}

// adopt takes a VM an earlier fleetci left running a job, its key restored
// by taskscaler, and follows the job to its end.
func (b *Backend) adopt(p *pool, key, instance string) {
	b.mu.Lock()
	b.vms[key] = p
	set(&b.instance, key, instance)
	set(&b.adopted, key, instance)
	b.mu.Unlock()
	p.mu.Lock()
	p.jobs++
	p.mu.Unlock()
	b.log.Info("vm adopted from an earlier fleetci", "vm", key, "class", p.name, "instance", instance)
	go b.follow(p, key, instance)
}

// followRetry is how often an adopted job's VM is tried again once the
// connection to it is lost, and followGiveUp how long until it is taken
// for gone and removed.
var (
	followRetry  = 5 * time.Second
	followGiveUp = 10 * time.Minute
)

// follow logs an adopted job's new output until it ends, then removes its
// VM. A job that isn't on the VM (one that never started, or a GitLab job,
// whose executor ended with the fleetci that ran it) ends at once. When
// the backend stops first, the VM is left to the next fleetci in turn.
func (b *Backend) follow(p *pool, key, instance string) {
	select {
	case <-p.ready:
	case <-b.ctx.Done():
		return
	}
	log := b.log.With("vm", key, "class", p.name, "instance", instance)
	conn := p.conns.get(instance)
	next := 0 // the earlier fleetci logged what came before
	var lost time.Time
	for {
		client, _, err := conn.Client(b.ctx)
		if err == nil {
			lost = time.Time{}
			var status int
			status, next, err = vmjob.Follow(b.ctx, client, key, next, func(l string) { log.Info("job: " + l) })
			switch {
			case err == nil:
				log.Info("adopted job ended", "status", status)
				_ = b.Delete(context.Background(), key)
				return
			case errors.Is(err, vmjob.ErrNoJob):
				log.Info("adopted vm runs no job of fleetci's; removing it")
				_ = b.Delete(context.Background(), key)
				return
			case errors.Is(err, vmjob.ErrLost):
				log.Warn("adopted job ended without a status; removing its vm")
				_ = b.Delete(context.Background(), key)
				return
			}
		}
		if b.ctx.Err() != nil {
			return
		}
		if lost.IsZero() {
			lost = time.Now()
		}
		if time.Since(lost) > followGiveUp {
			log.Error("adopted vm unreachable; removing it", "for", followGiveUp.String(), "err", err)
			_ = b.Delete(context.Background(), key)
			return
		}
		log.Debug("following the adopted job again", "err", err)
		select {
		case <-b.ctx.Done():
			return
		case <-time.After(followRetry):
		}
	}
}

// set sets m[k], making m if it is nil.
func set(m *map[string]string, k, v string) {
	if *m == nil {
		*m = map[string]string{}
	}
	(*m)[k] = v
}

// allPools yields every class's pools.
func (b *Backend) allPools() iter.Seq[*pool] {
	return func(yield func(*pool) bool) {
		for _, pools := range b.pools {
			for _, p := range pools {
				if !yield(p) {
					return
				}
			}
		}
	}
}

// Shutdown removes every VM of every class and stops the plugins. Calls
// after the first, or after Stop, do nothing.
func (b *Backend) Shutdown(ctx context.Context) error {
	b.stop(ctx, false)
	return nil
}

// Stop stops the plugins and removes every VM but those left to the next
// fleetci: handed over by a job source, or adopted and still running their
// job. Without a state directory it is Shutdown.
func (b *Backend) Stop(ctx context.Context) error {
	b.stop(ctx, true)
	return nil
}

// storedShutdown bounds a stored class's taskscaler shutdown, which
// otherwise waits for every VM to be released, those left to the next
// fleetci too.
var storedShutdown = 10 * time.Second

func (b *Backend) stop(ctx context.Context, leave bool) {
	b.shutdown.Do(func() {
		if b.cancel != nil {
			b.cancel()
		}
		left := map[string]bool{} // instances
		if leave {
			b.mu.Lock()
			for _, m := range []map[string]string{b.handed, b.adopted} {
				for _, inst := range m {
					left[inst] = true
				}
			}
			b.mu.Unlock()
		}
		var wg sync.WaitGroup
		for p := range b.allPools() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				log := b.log.With("class", p.name)
				tctx := ctx
				if p.stored {
					var cancel context.CancelFunc
					tctx, cancel = context.WithTimeout(ctx, storedShutdown)
					defer cancel()
				}
				p.ts.Shutdown(tctx)
				if p.reclaim != nil {
					p.reclaim.close()
				}
				p.conns.close()
				keep := map[string]bool{}
				if p.stored {
					keep = left
				}
				if n := len(keep); n > 0 {
					log.Info("leaving vms that run jobs to the next fleetci", "vms", n)
				}
				sweep(ctx, p.group, keep, log)
				p.group.Close()
			}()
		}
		wg.Wait()
	})
}

// sweep removes whatever the plugin still lists once taskscaler has stopped,
// but the instances in keep. taskscaler removes only the instances it has
// seen running, so one that was still booting would be left behind. A
// class's group belongs to fleetci alone and each VM serves one job, so
// nothing else in it is worth keeping.
//
// It runs after taskscaler's shutdown has called the plugin's Shutdown; the
// fleeting plugins fleetci uses (aws, dockervm) keep working after it.
func sweep(ctx context.Context, group provider.InstanceGroup, keep map[string]bool, log hclog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	for {
		var ids []string
		err := group.Update(ctx, func(id string, state provider.State) {
			if state != provider.StateDeleting && state != provider.StateDeleted && !keep[id] {
				ids = append(ids, id)
			}
		})
		if err != nil {
			log.Warn("listing instances at shutdown", "err", err)
			return
		}
		if len(ids) == 0 {
			return
		}

		log.Info("removing instances left at shutdown", "instances", ids)
		if _, err := group.Decrease(ctx, ids); err != nil {
			log.Warn("removing instances at shutdown", "err", err)
		}

		select {
		case <-ctx.Done():
			log.Warn("instances may be left behind", "instances", ids)
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// dockerReady admits an instance once its setup has finished and Docker
// answers over SSH. Instances that existed before fleetci started may have
// run a job already, so they are removed instead.
func dockerReady(group provider.InstanceGroup, external bool, log hclog.Logger) taskscaler.UpFunc {
	up := dockerUp(group, external, log)
	return func(ts taskscaler.Taskscaler, inst taskscaler.UpFuncInstance) error {
		if inst.Cause == fleeting.CausePreexisted {
			return errors.New("instance existed before fleetci started; removing it")
		}
		return up(ts, inst)
	}
}

// adoptUp admits, for a class whose state is kept, the instances an earlier
// fleetci left: their saved state says what they ran, and adopt follows each
// job they still hold. One that existed with no saved state is removed, as
// dockerReady removes it.
func adoptUp(up taskscaler.UpFunc, adopt func(key, instance string)) taskscaler.UpFunc {
	return func(ts taskscaler.Taskscaler, inst taskscaler.UpFuncInstance) error {
		if inst.Cause == fleeting.CausePreexisted && !inst.Restored {
			return errors.New("instance existed before fleetci started, with no saved state; removing it")
		}
		if err := up(ts, inst); err != nil {
			return err
		}
		for _, key := range inst.Acquisitions {
			adopt(key, inst.ID)
		}
		return nil
	}
}

// dockerUp admits an instance once its setup has finished and Docker
// answers over SSH.
//
// Every attempt opens a new connection with fresh credentials: a login made
// while the VM's setup is still running keeps the groups it started with,
// so it can stay locked out of Docker after setup adds the user to the
// docker group. Short-lived keys (EC2 Instance Connect's last 60 seconds)
// need the fresh ConnectInfo anyway.
func dockerUp(group provider.InstanceGroup, external bool, log hclog.Logger) taskscaler.UpFunc {
	return func(_ taskscaler.Taskscaler, inst taskscaler.UpFuncInstance) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		wait := 500 * time.Millisecond
		for {
			version, err := dockerVersion(ctx, group, inst.ID, external)
			if err == nil {
				log.Info("instance ready", "instance", inst.ID, "docker", version)
				return nil
			}
			log.Debug("instance not ready yet", "instance", inst.ID, "err", err)
			select {
			case <-ctx.Done():
				return fmt.Errorf("docker not ready on %s: %w", inst.ID, err)
			case <-time.After(wait):
			}
			// SSH comes up seconds into boot: retry soon, then back off so a
			// slow VM costs few ConnectInfo calls (an API call on EC2).
			wait = min(wait*3/2, 3*time.Second)
		}
	}
}

// readyScript waits for cloud-init where the image has it, then asks the
// Docker server for its version. The client alone can print an empty
// version and exit 0 when no daemon answers, so the output is what counts.
// cloud-init writes result.json when it is done; while it is there,
// cloud-init status, a Python program slow to start, has nothing to add.
const readyScript = `if [ ! -e /run/cloud-init/result.json ] && command -v cloud-init >/dev/null 2>&1; then cloud-init status --wait >/dev/null 2>&1 || true; fi
docker version --format '{{.Server.Version}}'`

func dockerVersion(ctx context.Context, group provider.InstanceGroup, id string, external bool) (string, error) {
	info, err := group.ConnectInfo(ctx, id)
	if err != nil {
		return "", err
	}
	client, err := proxy.Dial(ctx, target(info, external))
	if err != nil {
		return "", err
	}
	defer client.Close()

	s, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer s.Close()
	out, err := s.CombinedOutput(readyScript)
	version := strings.TrimSpace(string(out))
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, version)
	}
	if version == "" || strings.ContainsAny(version, " \n") {
		return "", fmt.Errorf("docker server did not answer: %q", version)
	}
	return version, nil
}

// target picks the address to dial. As in fleeting's connector, a VM
// without an external address is reached at its internal one.
func target(info provider.ConnectInfo, external bool) proxy.Target {
	if external && info.ExternalAddr != "" {
		info.InternalAddr = info.ExternalAddr
	}
	return proxy.Target{Info: info}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

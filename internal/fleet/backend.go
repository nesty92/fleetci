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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-hclog"

	"gitlab.com/gitlab-org/fleeting/fleeting"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"gitlab.com/gitlab-org/fleeting/taskscaler"

	"github.com/nesty92/fleetci/internal/proxy"
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
	ts      taskscaler.Taskscaler
	timeout time.Duration

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

// New starts every class's plugin and taskscaler. host is fleetci.toml's
// [host], or nil for its defaults.
func New(ctx context.Context, classes map[string]Class, host *HostConfig, log hclog.Logger) (*Backend, error) {
	b := &Backend{log: log, pools: map[string][]*pool{}, vms: map[string]*pool{}}
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
		p, err := startPool(ctx, name, c, groups, log.Named("class."+name))
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
	}
	return nil
}

// startPool starts the class's plugin and then its taskscaler. The plugin is
// initialised first, so a class whose group another class already uses is
// refused before a second taskscaler can see the group: each would remove
// the other's VMs as unknown.
func startPool(ctx context.Context, name string, c Class, groups map[string]string, log hclog.Logger) (*pool, error) {
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
	scaled, up, reclaim := withReclaim(c, group, conns, dockerReady(group, c.Connector.UseExternalAddr, log),
		sshTunnel(conns), log)
	stop := func() {
		if reclaim != nil {
			reclaim.close()
		}
		conns.close()
		group.Close()
	}

	ts, err := taskscaler.New(ctx, initialised{scaled, info}, scalerOptions(c, settings, up, log)...)
	if err != nil {
		stop()
		return nil, fmt.Errorf("starting taskscaler: %w", err)
	}

	if err := ts.ConfigureSchedule(c.schedules()...); err != nil {
		ts.Shutdown(ctx)
		stop()
		return nil, err
	}

	timeout := c.acquireTimeout()
	log.Info("class ready", "group", info.ID, "plugin", c.Plugin, "max_instances", c.MaxInstances, "policies", len(c.schedules()))
	return &pool{name: name, class: c, group: group, reclaim: reclaim, conns: conns, ts: ts, timeout: timeout}, nil
}

// withReclaim wraps a class's group so a gone instance's connection is
// dropped and, for a class watched for reclaim, so the heartbeat of a VM
// with a notice fails and up starts watching a VM once it is admitted.
// reclaim is nil for a class that isn't watched.
func withReclaim(c Class, group provider.InstanceGroup, conns *vmConns, up taskscaler.UpFunc, open opener, log hclog.Logger) (provider.InstanceGroup, taskscaler.UpFunc, *reclaimWatch) {
	if !c.watchesReclaim() {
		return vmGroup{group, conns, nil}, up, nil
	}
	w := newReclaimWatch(open, c.Reclaim.Rebalance, log.Named("reclaim"))
	return vmGroup{group, conns, w}, watchOnceUp(up, w), w
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
func scalerOptions(c Class, settings provider.Settings, up taskscaler.UpFunc, log hclog.Logger) []taskscaler.Option {
	return []taskscaler.Option{
		taskscaler.WithLogger(log.Named("taskscaler")),
		taskscaler.WithInstanceGroupSettings(settings),
		taskscaler.WithCapacityPerInstance(c.capacity()),
		taskscaler.WithMaxUseCount(c.useCount()),
		taskscaler.WithMaxInstances(c.MaxInstances),
		taskscaler.WithInstanceUpFunc(up),
		taskscaler.WithDeleteInstancesOnShutdown(),
		// A job is waiting while instances are expected: look every second
		// instead of every two. One list call per second per scaling class.
		taskscaler.WithUpdateIntervalWhenExpecting(time.Second),
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
// after the first do nothing.
func (b *Backend) Shutdown(ctx context.Context) error {
	b.shutdown.Do(func() {
		var wg sync.WaitGroup
		for p := range b.allPools() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p.ts.Shutdown(ctx)
				if p.reclaim != nil {
					p.reclaim.close()
				}
				p.conns.close()
				sweep(ctx, p.group, b.log.With("class", p.name))
				p.group.Close()
			}()
		}
		wg.Wait()
	})
	return nil
}

// sweep removes whatever the plugin still lists once taskscaler has stopped.
// taskscaler removes only the instances it has seen running, so one that
// was still booting would be left behind. A class's group belongs to fleetci
// alone and each VM serves one job, so nothing in it is worth keeping.
//
// It runs after taskscaler's shutdown has called the plugin's Shutdown; the
// fleeting plugins fleetci uses (aws, dockervm) keep working after it.
func sweep(ctx context.Context, group provider.InstanceGroup, log hclog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	for {
		var ids []string
		err := group.Update(ctx, func(id string, state provider.State) {
			if state != provider.StateDeleting && state != provider.StateDeleted {
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
//
// Every attempt opens a new connection with fresh credentials: a login made
// while the VM's setup is still running keeps the groups it started with,
// so it can stay locked out of Docker after setup adds the user to the
// docker group. Short-lived keys (EC2 Instance Connect's last 60 seconds)
// need the fresh ConnectInfo anyway.
func dockerReady(group provider.InstanceGroup, external bool, log hclog.Logger) taskscaler.UpFunc {
	return func(_ taskscaler.Taskscaler, inst taskscaler.UpFuncInstance) error {
		if inst.Cause == fleeting.CausePreexisted {
			return errors.New("instance existed before fleetci started; removing it")
		}

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
const readyScript = `if command -v cloud-init >/dev/null 2>&1; then cloud-init status --wait >/dev/null 2>&1 || true; fi
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

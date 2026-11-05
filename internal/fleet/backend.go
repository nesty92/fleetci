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
	"maps"
	"slices"
	"strings"
	"sync"
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
	Connect  proxy.ConnectFunc
}

type pool struct {
	name     string
	class    Class
	group    *supervised
	ts       taskscaler.Taskscaler
	timeout  time.Duration
	overflow *pool

	// host is the budget the class's VMs count against, or nil. A dynamic
	// class takes size from it for each job; the others hold a fixed share
	// (see Class.Reserved).
	host    *hostBudget
	size    Resources
	dynamic bool

	mu   sync.Mutex
	jobs int // jobs on the class's VMs or waiting for one
}

// hostBudget is the host's budget and what of it is in use. Jobs waiting
// for room are served in arrival order: a waiting job that is short of room
// keeps what it needs from the jobs behind it, so a large job isn't starved
// by small ones.
type hostBudget struct {
	Budget

	mu      sync.Mutex
	used    Resources
	waiting []*waiter
}

// waiter is a job waiting for a VM of a class on the host.
type waiter struct {
	need    Resources
	blocked bool // its last try found the class free but the host short
}

func (h *hostBudget) enqueue(need Resources) *waiter {
	h.mu.Lock()
	defer h.mu.Unlock()
	w := &waiter{need: need}
	h.waiting = append(h.waiting, w)
	return w
}

func (h *hostBudget) dequeue(w *waiter) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.waiting = slices.DeleteFunc(h.waiting, func(x *waiter) bool { return x == w })
}

// ahead is what the blocked jobs before w need; with w nil, all of them.
func (h *hostBudget) ahead(w *waiter) Resources {
	var r Resources
	for _, x := range h.waiting {
		if x == w {
			break
		}
		if x.blocked {
			r = r.add(x.need)
		}
	}
	return r
}

// take counts a job in, if the class has a free slot for it and, for a
// dynamic class, the host has room for one more of its VMs. w is the job's
// place in the host's queue, or nil.
func (p *pool) take(w *waiter) bool {
	h := p.host
	if h != nil && p.dynamic {
		h.mu.Lock()
		defer h.mu.Unlock()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.jobs >= p.class.Slots() {
		return false
	}
	if h != nil && p.dynamic {
		if !h.used.add(h.ahead(w)).add(p.size).fits(h.Resources) {
			if w != nil {
				w.blocked = true
			}
			return false
		}
		h.used = h.used.add(p.size)
	}
	p.jobs++
	return true
}

func (p *pool) done() {
	if h := p.host; h != nil && p.dynamic {
		h.mu.Lock()
		h.used = h.used.add(p.size.times(-1))
		defer h.mu.Unlock()
	}
	p.mu.Lock()
	p.jobs--
	p.mu.Unlock()
}

type Backend struct {
	log      hclog.Logger
	pools    map[string]*pool
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
	b := &Backend{log: log, pools: map[string]*pool{}, vms: map[string]*pool{}}
	hb, err := newHostBudget(host, classes)
	if err != nil {
		return nil, err
	}
	if hb != nil {
		log.Info("host budget", "budget", hb.Budget.String(), "reserved", hb.used.String())
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
		if hb != nil && c.OnHost() {
			p.host, p.dynamic = hb, !c.Reuses()
			p.size, _ = c.Size() // checked by newHostBudget
		}
		b.pools[name] = p
	}
	for name, p := range b.pools {
		if err := checkOverflow(name, classes); err != nil {
			_ = b.Shutdown(ctx)
			return nil, err
		}
		if o := p.class.Overflow; o != "" {
			p.overflow = b.pools[o]
		}
	}
	return b, nil
}

// newHostBudget works out the host's budget and the fixed shares of its
// classes, or nil when there is none. Every class on the host must fit.
func newHostBudget(h *HostConfig, classes map[string]Class) (*hostBudget, error) {
	budget, on, err := HostBudget(h, classes)
	if err != nil || !on {
		return nil, err
	}
	hb := &hostBudget{Budget: budget}
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
		hb.used = hb.used.add(c.Reserved(size))
		if !c.Reuses() && !size.fits(largest) {
			largest, largestName = Resources{max(size.CPUs, largest.CPUs), max(size.MemoryMB, largest.MemoryMB)}, name
		}
	}
	if !hb.used.fits(hb.Resources) {
		return nil, fmt.Errorf("host: the classes' fixed shares (warm and reused VMs, %s) are more than the budget (%s)", hb.used, budget)
	}
	if largestName != "" && !hb.used.add(largest).fits(hb.Resources) {
		return nil, fmt.Errorf("host: a VM of class %q (%s) never fits: the budget is %s and the fixed shares take %s", largestName, largest, budget, hb.used)
	}
	return hb, nil
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

	ts, err := taskscaler.New(ctx, initialised{group, info},
		taskscaler.WithLogger(log.Named("taskscaler")),
		taskscaler.WithInstanceGroupSettings(settings),
		taskscaler.WithCapacityPerInstance(c.capacity()),
		taskscaler.WithMaxUseCount(c.useCount()),
		taskscaler.WithMaxInstances(c.MaxInstances),
		taskscaler.WithInstanceUpFunc(dockerReady(group, c.Connector.UseExternalAddr, log)),
		taskscaler.WithDeleteInstancesOnShutdown(),
		// A job is waiting while instances are expected: look every second
		// instead of every two. One list call per second per scaling class.
		taskscaler.WithUpdateIntervalWhenExpecting(time.Second),
	)
	if err != nil {
		group.Close()
		return nil, fmt.Errorf("starting taskscaler: %w", err)
	}

	if err := ts.ConfigureSchedule(c.schedules()...); err != nil {
		ts.Shutdown(ctx)
		group.Close()
		return nil, err
	}

	timeout := c.acquireTimeout()
	log.Info("class ready", "group", info.ID, "plugin", c.Plugin, "max_instances", c.MaxInstances, "policies", len(c.schedules()))
	return &pool{name: name, class: c, group: group, ts: ts, timeout: timeout}, nil
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
	asked, ok := b.pools[class]
	if !ok {
		return VM{}, fmt.Errorf("unknown class %q", class)
	}
	start := time.Now()
	actx, cancel := context.WithTimeout(ctx, asked.timeout)
	defer cancel()

	o := asked.overflow
	var p *pool
	if o == nil && !asked.dynamic {
		// No overflow and no budget: taskscaler queues the job itself.
		p = asked
		asked.mu.Lock()
		asked.jobs++
		asked.mu.Unlock()
	} else {
		// fleetci holds the job until its class, its overflow or the host
		// has room, its own class first.
		var w *waiter
		if asked.dynamic {
			w = asked.host.enqueue(asked.size)
		}
		for p == nil {
			wake := b.woken()
			switch {
			case asked.take(w):
				p = asked
			case o != nil && o.take(w):
				p = o
			default:
				select {
				case <-actx.Done():
					if w != nil {
						asked.host.dequeue(w)
					}
					return VM{}, fmt.Errorf("acquiring a %s vm: %s: %w", class, b.full(asked), actx.Err())
				case <-wake:
				case <-time.After(overflowPoll):
				}
			}
		}
		if w != nil {
			asked.host.dequeue(w)
		}
	}

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
	if p != asked {
		log = append(log, "overflow_of", asked.name)
	}
	b.log.Info("vm acquired", log...)

	// Each call asks the plugin again, so plugins that issue short-lived
	// keys hand out a fresh one for every connection.
	external := p.class.Connector.UseExternalAddr
	connect := func(ctx context.Context) (proxy.Target, error) {
		info, err := acq.InstanceConnectInfo(ctx)
		if err != nil {
			return proxy.Target{}, err
		}
		return target(info, external), nil
	}
	return VM{ID: id, Class: p.name, Instance: acq.InstanceID(), Connect: connect}, nil
}

// full says why a job of the class found no room.
func (b *Backend) full(p *pool) string {
	why := p.name + " is full"
	if o := p.overflow; o != nil {
		why = fmt.Sprintf("%s and its overflow %s are full", p.name, o.name)
	}
	if h := p.host; h != nil && p.dynamic {
		h.mu.Lock()
		why += fmt.Sprintf(", or the host has no room for one more of its VMs (%s; host %s in use of %s)", p.size, h.used, h.Resources)
		h.mu.Unlock()
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

// Shutdown removes every VM of every class and stops the plugins. Calls
// after the first do nothing.
func (b *Backend) Shutdown(ctx context.Context) error {
	b.shutdown.Do(func() {
		var wg sync.WaitGroup
		for _, p := range b.pools {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p.ts.Shutdown(ctx)
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

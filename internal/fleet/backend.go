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

	mu   sync.Mutex
	jobs int // VMs acquired or being acquired; each serves one job
}

// take counts a job in, if the class has room for it.
func (p *pool) take() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.jobs >= p.class.MaxInstances {
		return false
	}
	p.jobs++
	return true
}

func (p *pool) done() {
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
}

// New starts every class's plugin and taskscaler.
func New(ctx context.Context, classes map[string]Class, log hclog.Logger) (*Backend, error) {
	b := &Backend{log: log, pools: map[string]*pool{}, vms: map[string]*pool{}}
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
		taskscaler.WithCapacityPerInstance(1),
		taskscaler.WithMaxUseCount(1),
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

	p := asked
	if o := asked.overflow; o != nil {
		for p = nil; p == nil; {
			switch {
			case asked.take():
				p = asked
			case o.take():
				p = o
			default:
				select {
				case <-actx.Done():
					return VM{}, fmt.Errorf("acquiring a %s vm: %s and its overflow %s are full: %w", class, class, o.name, actx.Err())
				case <-time.After(overflowPoll):
				}
			}
		}
	} else {
		// No overflow: taskscaler queues the job itself.
		asked.mu.Lock()
		asked.jobs++
		asked.mu.Unlock()
	}

	id := "fleetci-" + p.name + "-" + randomHex(4)
	acq, err := p.ts.Acquire(actx, id)
	if err != nil {
		p.done()
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

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
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"

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
	name    string
	class   Class
	runner  *fleeting.Runner
	ts      taskscaler.Taskscaler
	timeout time.Duration
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
	for name, c := range classes {
		if err := c.validate(name); err != nil {
			_ = b.Shutdown(ctx)
			return nil, err
		}
		p, err := startPool(ctx, name, c, log.Named("class."+name))
		if err != nil {
			_ = b.Shutdown(ctx)
			return nil, fmt.Errorf("class %q: %w", name, err)
		}
		b.pools[name] = p
	}
	return b, nil
}

func startPool(ctx context.Context, name string, c Class, log hclog.Logger) (*pool, error) {
	settings, err := c.Connector.settings()
	if err != nil {
		return nil, err
	}

	cfg := []byte(c.PluginConfig)
	if len(cfg) == 0 {
		cfg = []byte("{}")
	}
	runner, err := fleeting.RunPlugin(c.Plugin, cfg, fleeting.WithPluginLogger(log.Named("plugin")))
	if err != nil {
		return nil, fmt.Errorf("starting plugin %s: %w", c.Plugin, err)
	}

	ts, err := taskscaler.New(ctx, runner.InstanceGroup(),
		taskscaler.WithLogger(log.Named("taskscaler")),
		taskscaler.WithInstanceGroupSettings(settings),
		taskscaler.WithCapacityPerInstance(1),
		taskscaler.WithMaxUseCount(1),
		taskscaler.WithMaxInstances(c.MaxInstances),
		taskscaler.WithInstanceUpFunc(dockerReady(c.Connector.UseExternalAddr, log)),
		taskscaler.WithDeleteInstancesOnShutdown(),
	)
	if err != nil {
		runner.Kill()
		return nil, fmt.Errorf("starting taskscaler: %w", err)
	}

	idleTime := time.Duration(c.IdleTime)
	if idleTime == 0 {
		idleTime = 10 * time.Minute
	}
	if err := ts.ConfigureSchedule(taskscaler.Schedule{IdleCount: c.IdleCount, IdleTime: idleTime}); err != nil {
		ts.Shutdown(ctx)
		runner.Kill()
		return nil, err
	}

	timeout := time.Duration(c.AcquireTimeout)
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	log.Info("class ready", "plugin", c.Plugin, "max_instances", c.MaxInstances, "idle_count", c.IdleCount)
	return &pool{name: name, class: c, runner: runner, ts: ts, timeout: timeout}, nil
}

// Has reports whether a class exists.
func (b *Backend) Has(class string) bool {
	_, ok := b.pools[class]
	return ok
}

// Create acquires a VM of the class, waiting for one to boot if none is idle.
func (b *Backend) Create(ctx context.Context, class string) (VM, error) {
	p, ok := b.pools[class]
	if !ok {
		return VM{}, fmt.Errorf("unknown class %q", class)
	}

	id := "fleetci-" + class + "-" + randomHex(4)
	start := time.Now()

	actx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	acq, err := p.ts.Acquire(actx, id)
	if err != nil {
		return VM{}, fmt.Errorf("acquiring a %s vm: %w", class, err)
	}

	b.mu.Lock()
	b.vms[id] = p
	b.mu.Unlock()

	b.log.Info("vm acquired", "vm", id, "class", class, "instance", acq.InstanceID(), "waited", time.Since(start).Round(time.Millisecond).String())

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
	return VM{ID: id, Class: class, Instance: acq.InstanceID(), Connect: connect}, nil
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
				sweep(ctx, p.runner.InstanceGroup(), b.log.With("class", p.name))
				p.runner.Kill()
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

// dockerReady admits an instance once Docker answers over SSH. Instances
// that existed before fleetci started may have run a job already, so they
// are removed instead.
func dockerReady(external bool, log hclog.Logger) taskscaler.UpFunc {
	return func(_ taskscaler.Taskscaler, inst taskscaler.UpFuncInstance) error {
		if inst.Cause == fleeting.CausePreexisted {
			return errors.New("instance existed before fleetci started; removing it")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		client, err := proxy.Dial(ctx, target(inst.Info, external))
		if err != nil {
			return err
		}
		defer client.Close()

		for {
			err := run(client, "docker info --format '{{.ServerVersion}}' >/dev/null")
			if err == nil {
				log.Info("instance ready", "instance", inst.ID)
				return nil
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("docker not ready on %s: %w", inst.ID, err)
			case <-time.After(time.Second):
			}
		}
	}
}

// target picks the address to dial. As in fleeting's connector, a VM
// without an external address is reached at its internal one.
func target(info provider.ConnectInfo, external bool) proxy.Target {
	if external && info.ExternalAddr != "" {
		info.InternalAddr = info.ExternalAddr
	}
	return proxy.Target{Info: info}
}

func run(c *ssh.Client, cmd string) error {
	s, err := c.NewSession()
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Run(cmd)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

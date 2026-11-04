// Package fleetci is the fleeting plugin gitlab-runner loads. To the runner
// it is a group of one instance: an SSH gateway on loopback inside this
// process. Behind it are the nesting API, which takes one VM per job from
// the job's class, and the proxy that carries the runner's VM connection to
// that VM. Each class is its own fleeting plugin and taskscaler.
package fleetci

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"gitlab.com/gitlab-org/fleeting/nesting/api"

	"github.com/nesty92/fleetci/internal/fleet"
	"github.com/nesty92/fleetci/internal/gateway"
	"github.com/nesty92/fleetci/internal/github"
)

const (
	instanceID = "fleetci"
	keyTTL     = 5 * time.Minute
)

// Config is the runner's [runners.autoscaler.plugin_config].
type Config struct {
	// Listen is the gateway's loopback address. Default 127.0.0.1:0.
	Listen string `json:"listen"`
	// NestingPath must match vm_isolation.nesting_host. Default
	// /fleetci/nesting.sock.
	NestingPath  string                 `json:"nesting_path"`
	DefaultClass string                 `json:"default_class"`
	Classes      map[string]fleet.Class `json:"classes"`
	// GitHub, when set, also runs GitHub Actions jobs on the classes.
	GitHub *github.Config `json:"github"`
}

type InstanceGroup struct {
	Config

	log      hclog.Logger
	settings provider.Settings
	gw       *gateway.Server
	stop     context.CancelFunc
	stopped  chan struct{}
	dir      string

	ghStop context.CancelFunc
	ghDone chan struct{}

	mu      sync.Mutex
	running bool
}

var _ provider.InstanceGroup = (*InstanceGroup)(nil)

func (g *InstanceGroup) Init(ctx context.Context, log hclog.Logger, settings provider.Settings) (provider.ProviderInfo, error) {
	if g.Listen == "" {
		g.Listen = "127.0.0.1:0"
	}
	if g.NestingPath == "" {
		g.NestingPath = "/fleetci/nesting.sock"
	}
	if _, ok := g.Classes[g.DefaultClass]; !ok {
		return provider.ProviderInfo{}, fmt.Errorf("default_class %q is not in classes", g.DefaultClass)
	}
	g.log = log
	g.settings = settings

	// Each class starts its own fleeting plugin and taskscaler.
	backend, err := fleet.New(ctx, g.Classes, log)
	if err != nil {
		return provider.ProviderInfo{}, err
	}
	started := false
	defer func() {
		if !started {
			_ = backend.Shutdown(context.WithoutCancel(ctx))
		}
	}()

	// The nesting API listens on a socket only this user can reach. The
	// runner gets to it through the gateway, never directly.
	g.dir, err = os.MkdirTemp("", "fleetci-")
	if err != nil {
		return provider.ProviderInfo{}, err
	}
	socket := filepath.Join(g.dir, "nesting.sock")
	if err := os.Setenv("NESTING_SOCKET", socket); err != nil {
		return provider.ProviderInfo{}, err
	}

	hv := NewHypervisor(g.DefaultClass, backend, log)

	var gh *github.Dispatcher
	if g.GitHub != nil {
		gh, err = github.New(*g.GitHub, g.DefaultClass, backend, nil, log.Named("github"))
		if err != nil {
			return provider.ProviderInfo{}, err
		}
	}
	g.gw, err = gateway.New(gateway.Config{
		Listen:        g.Listen,
		NestingPath:   g.NestingPath,
		NestingSocket: socket,
		VMs:           hv,
		Log:           log,
	})
	if err != nil {
		return provider.ProviderInfo{}, err
	}

	runCtx, stop := context.WithCancel(context.Background())
	g.stop = stop
	g.stopped = make(chan struct{})
	go func() {
		defer close(g.stopped)
		if err := api.Serve(runCtx, hv); err != nil {
			log.Error("nesting api stopped", "err", err)
		}
	}()
	go g.gw.Serve(runCtx)

	if err := waitForSocket(ctx, socket); err != nil {
		stop()
		<-g.stopped
		return provider.ProviderInfo{}, err
	}
	started = true

	if gh != nil {
		ghCtx, ghStop := context.WithCancel(context.Background())
		g.ghStop, g.ghDone = ghStop, make(chan struct{})
		go func() {
			defer close(g.ghDone)
			gh.Run(ghCtx)
		}()
	}

	log.Info("fleetci ready", "gateway", g.gw.Addr(), "classes", len(g.Classes), "default_class", g.DefaultClass)
	return provider.ProviderInfo{ID: "fleetci/gateway", MaxSize: 1}, nil
}

func waitForSocket(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("nesting api did not start")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (g *InstanceGroup) Update(_ context.Context, fn func(string, provider.State)) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running {
		fn(instanceID, provider.StateRunning)
	}
	return nil
}

func (g *InstanceGroup) Increase(_ context.Context, n int) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if n <= 0 || g.running {
		return 0, nil
	}
	g.running = true
	return 1, nil
}

// Decrease takes the gateway out of the group. The VMs behind it are removed
// when the runner deletes them or the plugin shuts down.
func (g *InstanceGroup) Decrease(_ context.Context, ids []string) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, id := range ids {
		if id == instanceID && g.running {
			g.running = false
			return []string{id}, nil
		}
	}
	return nil, nil
}

// ConnectInfo hands the runner a fresh key for the gateway. Keys open new
// connections for a few minutes; open connections are not affected.
func (g *InstanceGroup) ConnectInfo(_ context.Context, id string) (provider.ConnectInfo, error) {
	if id != instanceID {
		return provider.ConnectInfo{}, fmt.Errorf("unknown instance %q", id)
	}
	key, err := g.gw.IssueKey(keyTTL)
	if err != nil {
		return provider.ConnectInfo{}, err
	}

	cc := g.settings.ConnectorConfig
	cc.Protocol = provider.ProtocolSSH
	cc.ProtocolPort = 0
	cc.Username = gateway.User
	cc.Password = ""
	cc.Key = key
	if cc.OS == "" {
		cc.OS = "linux"
	}
	if cc.Arch == "" {
		cc.Arch = runtime.GOARCH
	}
	if cc.Timeout == 0 {
		cc.Timeout = 2 * time.Minute
	}

	expires := time.Now().Add(keyTTL - time.Minute)
	return provider.ConnectInfo{
		ConnectorConfig: cc,
		ID:              id,
		InternalAddr:    g.gw.Addr(),
		ExternalAddr:    g.gw.Addr(),
		Expires:         &expires,
	}, nil
}

func (g *InstanceGroup) Heartbeat(_ context.Context, id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if id != instanceID || !g.running {
		return provider.ErrInstanceUnhealthy
	}
	return nil
}

func (g *InstanceGroup) Suspend(context.Context, []string) ([]string, error) { return nil, nil }
func (g *InstanceGroup) Resume(context.Context, []string) ([]string, error)  { return nil, nil }

// Shutdown stops GitHub's runners, then the gateway and the nesting API,
// which removes every VM.
func (g *InstanceGroup) Shutdown(ctx context.Context) error {
	if g.ghStop != nil {
		g.ghStop()
		select {
		case <-g.ghDone:
		case <-ctx.Done():
		}
	}
	if g.stop == nil {
		return nil
	}
	g.stop()
	select {
	case <-g.stopped:
	case <-ctx.Done():
	}
	return os.RemoveAll(g.dir)
}

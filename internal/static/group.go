// Package static is a fleeting provider with exactly one instance: a host
// that already exists and is reachable at a fixed address. fleetci uses it
// to point gitlab-runner at the gateway host that runs fleetci-nesting.
package static

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/hashicorp/go-hclog"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

var _ provider.InstanceGroup = (*InstanceGroup)(nil)

// InstanceGroup is configured from the runner's plugin_config.
type InstanceGroup struct {
	// Address is host:port of the instance's SSH server.
	Address string `json:"address"`
	// Name identifies the instance in logs and metrics.
	Name string `json:"name"`

	mu       sync.Mutex
	state    provider.State // "" when not provisioned
	settings provider.Settings
	log      hclog.Logger
}

func (g *InstanceGroup) Init(_ context.Context, log hclog.Logger, settings provider.Settings) (provider.ProviderInfo, error) {
	if g.Address == "" {
		return provider.ProviderInfo{}, errors.New("plugin_config.address is required")
	}
	if g.Name == "" {
		g.Name = "static"
	}
	g.log = log
	g.settings = settings

	return provider.ProviderInfo{ID: "static/" + g.Name, MaxSize: 1}, nil
}

func (g *InstanceGroup) Update(_ context.Context, fn func(string, provider.State)) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	switch g.state {
	case "":
		return nil
	case provider.StateDeleting:
		// The host is not ours to remove: report it gone right away.
		fn(g.Name, provider.StateDeleted)
		g.state = ""
	default:
		fn(g.Name, g.state)
	}
	return nil
}

func (g *InstanceGroup) Increase(_ context.Context, n int) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if n <= 0 || g.state != "" {
		return 0, nil
	}
	g.state = provider.StateRunning
	return 1, nil
}

func (g *InstanceGroup) Decrease(_ context.Context, ids []string) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	var done []string
	for _, id := range ids {
		if id == g.Name && g.state == provider.StateRunning {
			g.state = provider.StateDeleting
			done = append(done, id)
		}
	}
	return done, nil
}

func (g *InstanceGroup) ConnectInfo(_ context.Context, id string) (provider.ConnectInfo, error) {
	if id != g.Name {
		return provider.ConnectInfo{}, fmt.Errorf("unknown instance %q", id)
	}
	return provider.ConnectInfo{
		ConnectorConfig: g.settings.ConnectorConfig,
		ID:              id,
		InternalAddr:    g.Address,
		ExternalAddr:    g.Address,
	}, nil
}

func (g *InstanceGroup) Heartbeat(_ context.Context, id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if id != g.Name || g.state != provider.StateRunning {
		return provider.ErrInstanceUnhealthy
	}
	return nil
}

func (g *InstanceGroup) Suspend(context.Context, []string) ([]string, error) { return nil, nil }
func (g *InstanceGroup) Resume(context.Context, []string) ([]string, error)  { return nil, nil }
func (g *InstanceGroup) Shutdown(context.Context) error                      { return nil }

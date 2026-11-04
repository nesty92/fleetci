package fleet

import (
	"context"
	"fmt"

	"github.com/hashicorp/go-hclog"

	"gitlab.com/gitlab-org/fleeting/fleeting"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

// Validate checks a class's settings without starting anything.
func (c Class) Validate(name string) error { return c.validate(name) }

// CheckResult is what CheckClass found.
type CheckResult struct {
	// Group is the plugin's id for the instance group, such as an Auto
	// Scaling group's name.
	Group string
	// Instances counts the group's instances by state.
	Instances map[provider.State]int
}

// CheckClass starts the class's plugin, initialises it with the class's
// settings and lists its instances, which proves the plugin is installed,
// its credentials work and the group exists. It changes nothing: no
// instance is started or removed, so it is safe while fleetci runs.
func CheckClass(ctx context.Context, name string, c Class, log hclog.Logger) (CheckResult, error) {
	if err := c.validate(name); err != nil {
		return CheckResult{}, err
	}
	settings, err := c.Connector.settings()
	if err != nil {
		return CheckResult{}, err
	}
	cfg := []byte(c.PluginConfig)
	if len(cfg) == 0 {
		cfg = []byte("{}")
	}
	runner, err := fleeting.RunPlugin(c.Plugin, cfg, fleeting.WithPluginLogger(log))
	if err != nil {
		return CheckResult{}, fmt.Errorf("starting plugin %s: %w", c.Plugin, err)
	}
	defer runner.Kill()

	group := runner.InstanceGroup()
	info, err := group.Init(ctx, log, settings)
	if err != nil {
		return CheckResult{}, fmt.Errorf("plugin %s: %w", c.Plugin, err)
	}
	res := CheckResult{Group: info.ID, Instances: map[provider.State]int{}}
	if err := group.Update(ctx, func(_ string, state provider.State) { res.Instances[state]++ }); err != nil {
		return res, fmt.Errorf("listing instances: %w", err)
	}
	return res, nil
}

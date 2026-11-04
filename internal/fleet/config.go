package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"gitlab.com/gitlab-org/fleeting/taskscaler"
	"gitlab.com/gitlab-org/fleeting/taskscaler/cron"
)

// Class is one machine size, backed by its own fleeting plugin. Every VM in
// a class is used for one job and then removed.
type Class struct {
	// Plugin is the fleeting plugin's path or name, as in the runner's
	// [runners.autoscaler] plugin.
	Plugin string `json:"plugin" toml:"plugin"`
	// PluginConfig is passed to the plugin as it is, as JSON.
	PluginConfig RawConfig `json:"plugin_config" toml:"plugin_config"`
	// Connector is how to log in to this class's VMs.
	Connector Connector `json:"connector_config" toml:"connector_config"`

	// MaxInstances caps the VMs of this class, running jobs and idle.
	MaxInstances int `json:"max_instances" toml:"max_instances"`
	// IdleCount is how many VMs to keep booted and waiting for a job. It
	// is shorthand for one policy that applies at all times.
	IdleCount int `json:"idle_count" toml:"idle_count"`
	// IdleTime is how long an unused idle VM is kept. Default 10m.
	IdleTime Duration `json:"idle_time" toml:"idle_time"`
	// Policy sets warm VMs by time of day, as gitlab-runner's
	// [[runners.autoscaler.policy]] does. Of the policies whose periods
	// match, the last one applies; when none matches, no VMs are kept warm.
	Policy []Policy `json:"policy" toml:"policy"`
	// AcquireTimeout is how long a job waits for a VM. Default 10m.
	AcquireTimeout Duration `json:"acquire_timeout" toml:"acquire_timeout"`
}

// Connector mirrors the runner's connector_config.
type Connector struct {
	OS                   string   `json:"os" toml:"os"`
	Arch                 string   `json:"arch" toml:"arch"`
	ProtocolPort         int      `json:"protocol_port" toml:"protocol_port"`
	Username             string   `json:"username" toml:"username"`
	KeyPath              string   `json:"key_path" toml:"key_path"`
	UseStaticCredentials bool     `json:"use_static_credentials" toml:"use_static_credentials"`
	Keepalive            Duration `json:"keepalive" toml:"keepalive"`
	Timeout              Duration `json:"timeout" toml:"timeout"`
	// UseExternalAddr connects to the VM's public address, for a manager
	// outside the VMs' network.
	UseExternalAddr bool `json:"use_external_addr" toml:"use_external_addr"`
}

// Policy is one entry of a class's policy list.
type Policy struct {
	// Periods are cron expressions ("* 8-18 * * mon-fri"); none means
	// always.
	Periods []string `json:"periods" toml:"periods"`
	// Timezone for Periods, such as "Europe/Warsaw". Default local time.
	Timezone string `json:"timezone" toml:"timezone"`
	// IdleCount is how many VMs to keep booted and waiting.
	IdleCount int `json:"idle_count" toml:"idle_count"`
	// IdleTime is how long an unused idle VM is kept. Default 10m.
	IdleTime Duration `json:"idle_time" toml:"idle_time"`
	// ScaleFactor keeps running jobs × ScaleFactor VMs warm when that is
	// more than IdleCount.
	ScaleFactor float64 `json:"scale_factor" toml:"scale_factor"`
	// ScaleFactorLimit caps running jobs × ScaleFactor.
	ScaleFactorLimit int `json:"scale_factor_limit" toml:"scale_factor_limit"`
	// PreemptiveMode is read only to reject it: it governs gitlab-runner's
	// job reservations, which fleetci's classes don't make.
	PreemptiveMode *bool `json:"preemptive_mode" toml:"preemptive_mode"`
}

const defaultIdleTime = 10 * time.Minute

// schedules turns the class's policies into taskscaler's.
func (c Class) schedules() []taskscaler.Schedule {
	policies := c.Policy
	if len(policies) == 0 {
		policies = []Policy{{IdleCount: c.IdleCount, IdleTime: c.IdleTime}}
	}
	out := make([]taskscaler.Schedule, len(policies))
	for i, p := range policies {
		idle := time.Duration(p.IdleTime)
		if idle == 0 {
			idle = defaultIdleTime
		}
		out[i] = taskscaler.Schedule{
			Periods:          p.Periods,
			Timezone:         p.Timezone,
			IdleCount:        p.IdleCount,
			IdleTime:         idle,
			ScaleFactor:      p.ScaleFactor,
			ScaleFactorLimit: p.ScaleFactorLimit,
		}
	}
	return out
}

func (c Class) validate(name string) error {
	if c.Plugin == "" {
		return fmt.Errorf("class %q: plugin is required", name)
	}
	if c.MaxInstances <= 0 {
		return fmt.Errorf("class %q: max_instances must be at least 1", name)
	}
	if c.IdleCount < 0 || c.IdleCount > c.MaxInstances {
		return fmt.Errorf("class %q: idle_count must be between 0 and max_instances", name)
	}
	if len(c.Policy) > 0 && (c.IdleCount != 0 || c.IdleTime != 0) {
		return fmt.Errorf("class %q: set idle_count and idle_time in its policies, not beside them", name)
	}
	for i, p := range c.Policy {
		where := fmt.Sprintf("class %q: policy %d", name, i+1)
		if p.IdleCount < 0 || p.IdleCount > c.MaxInstances {
			return fmt.Errorf("%s: idle_count must be between 0 and max_instances", where)
		}
		if p.PreemptiveMode != nil {
			return fmt.Errorf("%s: preemptive_mode doesn't apply to fleetci classes (it only affects gitlab-runner's own reservations); remove it", where)
		}
		if p.ScaleFactor < 0 || p.ScaleFactorLimit < 0 {
			return fmt.Errorf("%s: scale_factor and scale_factor_limit can't be negative", where)
		}
		for _, period := range p.Periods {
			if _, err := cron.Parse(period, p.Timezone); err != nil {
				return fmt.Errorf("%s: period %q: %w", where, period, err)
			}
		}
	}
	return nil
}

func (c Connector) settings() (provider.Settings, error) {
	cc := provider.ConnectorConfig{
		OS:                   c.OS,
		Arch:                 c.Arch,
		Protocol:             provider.ProtocolSSH,
		ProtocolPort:         c.ProtocolPort,
		Username:             c.Username,
		UseStaticCredentials: c.UseStaticCredentials,
		Keepalive:            time.Duration(c.Keepalive),
		Timeout:              time.Duration(c.Timeout),
	}
	if cc.Timeout == 0 {
		cc.Timeout = 2 * time.Minute
	}
	if c.KeyPath != "" {
		key, err := os.ReadFile(c.KeyPath)
		if err != nil {
			return provider.Settings{}, fmt.Errorf("reading key_path: %w", err)
		}
		cc.Key = key
	}
	return provider.Settings{ConnectorConfig: cc}, nil
}

// RawConfig is a fleeting plugin's config, kept as JSON for the plugin. From
// TOML it is any table, converted to JSON.
type RawConfig json.RawMessage

func (r RawConfig) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("null"), nil
	}
	return r, nil
}

func (r *RawConfig) UnmarshalJSON(b []byte) error {
	*r = append((*r)[:0], b...)
	return nil
}

func (r *RawConfig) UnmarshalTOML(v any) error {
	if _, ok := v.(map[string]any); !ok {
		return fmt.Errorf("plugin_config must be a table, not %T", v)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	*r = b
	return nil
}

// UnmarshalText reads "90s" or "10m" from TOML.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Duration reads "90s" or "10m" from config. The runner hands
// plugin_config over as JSON, where a TOML duration arrives as a string.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		*d = Duration(v)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return errors.New("duration must be a string such as \"10m\"")
	}
	*d = Duration(n)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

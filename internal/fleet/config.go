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
	// CapacityPerInstance is how many jobs a VM runs at once, and
	// MaxUseCount how many it runs before it is removed. Both default to 1:
	// a fresh VM per job. More reuses VMs, for GitLab only; see Reuses.
	CapacityPerInstance int `json:"capacity_per_instance" toml:"capacity_per_instance"`
	MaxUseCount         int `json:"max_use_count" toml:"max_use_count"`
	// IdleCount is how many VMs to keep booted and waiting for a job. It
	// is shorthand for one policy that applies at all times.
	IdleCount int `json:"idle_count" toml:"idle_count"`
	// IdleTime is how long an unused idle VM is kept. Default 10m.
	IdleTime Duration `json:"idle_time" toml:"idle_time"`
	// Policy sets warm VMs by time of day, as gitlab-runner's
	// [[runners.autoscaler.policy]] does. Of the policies whose periods
	// match, the last one applies; when none matches, no VMs are kept warm.
	Policy []Policy `json:"policy" toml:"policy"`
	// AcquireTimeout is how long a job waits for a VM. Default 10m, or 50m
	// for a class with an overflow, whose jobs wait while both are busy
	// rather than because something is broken.
	AcquireTimeout Duration `json:"acquire_timeout" toml:"acquire_timeout"`
	// Overflow names another class that takes this class's jobs while all
	// of this class's max_instances are in use, such as cloud VMs for the
	// spikes of a class on your own machines. When both are full, the job
	// waits for this class. The overflow class can't have one of its own.
	Overflow string `json:"overflow" toml:"overflow"`

	// Host says whether the class's VMs count against the [host] budget.
	// Unset, they do for the plugins known to boot VMs on this machine
	// (fleetingd, dockervm).
	Host *bool `json:"host" toml:"host"`
	// CPUs and Memory are the size of one of the class's VMs, for the
	// budget. Unset, they are read from fleetingd's or dockervm's
	// plugin_config.
	CPUs   int  `json:"cpus" toml:"cpus"`
	Memory Size `json:"memory" toml:"memory"`
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

const (
	defaultAcquireTimeout = 10 * time.Minute
	// Under GitLab's default job timeout (1h), so a job that never got a VM
	// still fails as fleetci's, with its reason, rather than as a timeout.
	defaultOverflowAcquireTimeout = 50 * time.Minute
)

// capacity is how many jobs a VM runs at once.
func (c Class) capacity() int { return max(c.CapacityPerInstance, 1) }

// useCount is how many jobs a VM runs before it is removed.
func (c Class) useCount() int { return max(c.MaxUseCount, 1) }

// Slots is how many of the class's jobs run at once.
func (c Class) Slots() int { return c.MaxInstances * c.capacity() }

// Reuses reports whether a VM of the class runs more than one job, at once
// or one after another.
func (c Class) Reuses() bool { return c.capacity() > 1 || c.useCount() > 1 }

func (c Class) acquireTimeout() time.Duration {
	switch {
	case c.AcquireTimeout != 0:
		return time.Duration(c.AcquireTimeout)
	case c.Overflow != "":
		return defaultOverflowAcquireTimeout
	}
	return defaultAcquireTimeout
}

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
	if c.CapacityPerInstance < 0 || c.MaxUseCount < 0 {
		return fmt.Errorf("class %q: capacity_per_instance and max_use_count can't be negative", name)
	}
	if c.CPUs < 0 || c.Memory < 0 {
		return fmt.Errorf("class %q: cpus and memory can't be negative", name)
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

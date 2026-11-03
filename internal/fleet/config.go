package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

// Class is one machine size, backed by its own fleeting plugin. Every VM in
// a class is used for one job and then removed.
type Class struct {
	// Plugin is the fleeting plugin's path or name, as in the runner's
	// [runners.autoscaler] plugin.
	Plugin string `json:"plugin"`
	// PluginConfig is passed to the plugin as it is.
	PluginConfig json.RawMessage `json:"plugin_config"`
	// Connector is how to log in to this class's VMs.
	Connector Connector `json:"connector_config"`

	// MaxInstances caps the VMs of this class, running jobs and idle.
	MaxInstances int `json:"max_instances"`
	// IdleCount is how many VMs to keep booted and waiting for a job.
	IdleCount int `json:"idle_count"`
	// IdleTime is how long an unused idle VM is kept.
	IdleTime Duration `json:"idle_time"`
	// AcquireTimeout is how long a job waits for a VM. Default 10m.
	AcquireTimeout Duration `json:"acquire_timeout"`
}

// Connector mirrors the runner's connector_config.
type Connector struct {
	OS                   string   `json:"os"`
	Arch                 string   `json:"arch"`
	ProtocolPort         int      `json:"protocol_port"`
	Username             string   `json:"username"`
	KeyPath              string   `json:"key_path"`
	UseStaticCredentials bool     `json:"use_static_credentials"`
	Keepalive            Duration `json:"keepalive"`
	Timeout              Duration `json:"timeout"`
	// UseExternalAddr connects to the VM's public address, for a manager
	// outside the VMs' network.
	UseExternalAddr bool `json:"use_external_addr"`
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

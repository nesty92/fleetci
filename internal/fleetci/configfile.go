package fleetci

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/nesty92/fleetci/internal/fleet"
)

// loadFile reads ConfigFile, a TOML file with fleetci's own settings, so
// config.toml only has to point at it:
//
//	[runners.autoscaler.plugin_config]
//	  config_file = "/etc/fleetci/fleetci.toml"
//
// The file holds default_class, classes, github and host with short table
// names ([classes.4vcpu-8gb], [[classes.4vcpu-8gb.policy]], [github], [host]). Unknown
// keys are errors, so a misspelled setting is not silently ignored.
// Relative paths in it are relative to the file.
func (c *Config) loadFile() error {
	if c.ConfigFile == "" {
		return nil
	}
	if c.DefaultClass != "" || len(c.Classes) > 0 || c.GitHub != nil || c.GitLab != nil || c.Host != nil || c.StateDir != "" {
		return errors.New("with config_file, put default_class, classes, github, gitlab, host and state_dir in that file, not in plugin_config")
	}

	var f Config
	md, err := toml.DecodeFile(c.ConfigFile, &f)
	if err != nil {
		var perr toml.ParseError
		if errors.As(err, &perr) {
			return fmt.Errorf("config_file %s:\n%s", c.ConfigFile, perr.ErrorWithPosition())
		}
		return fmt.Errorf("config_file %s: %w", c.ConfigFile, err)
	}
	// A plugin_config table is the plugin's to check; anything else unknown
	// is a misspelling.
	var unknown []string
	for _, key := range md.Undecoded() {
		if !slices.Contains(key, "plugin_config") {
			unknown = append(unknown, key.String())
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("config_file %s: unknown keys: %s", c.ConfigFile, strings.Join(unknown, ", "))
	}
	if f.ConfigFile != "" {
		return errors.New("config_file can't point at another config_file")
	}

	dir := filepath.Dir(c.ConfigFile)
	for name, class := range f.Classes {
		// A bare name is looked up like any fleeting plugin; a relative
		// path is taken from the file's directory.
		if strings.Contains(class.Plugin, "/") {
			class.Plugin = relativeTo(dir, class.Plugin)
		}
		if class.Connector.KeyPath != "" {
			class.Connector.KeyPath = relativeTo(dir, class.Connector.KeyPath)
		}
		f.Classes[name] = class
	}
	if g := f.GitHub; g != nil {
		g.TokenFile = relativeTo(dir, g.TokenFile)
		g.AppPrivateKeyFile = relativeTo(dir, g.AppPrivateKeyFile)
		if w := g.Webhook; w != nil {
			w.SecretFile = relativeTo(dir, w.SecretFile)
			w.TLSCertFile = relativeTo(dir, w.TLSCertFile)
			w.TLSKeyFile = relativeTo(dir, w.TLSKeyFile)
		}
	}

	if f.GitLab != nil {
		for i := range f.GitLab.Runners {
			f.GitLab.Runners[i].TokenFile = relativeTo(dir, f.GitLab.Runners[i].TokenFile)
		}
		if f.GitLab.ConfigFile != "" {
			f.GitLab.ConfigFile = relativeTo(dir, f.GitLab.ConfigFile)
		}
	}

	c.DefaultClass, c.Classes, c.GitHub, c.GitLab, c.Host = f.DefaultClass, f.Classes, f.GitHub, f.GitLab, f.Host
	c.StateDir = relativeTo(dir, f.StateDir)
	if c.Listen == "" {
		c.Listen = f.Listen
	}
	if c.NestingPath == "" {
		c.NestingPath = f.NestingPath
	}
	return nil
}

func relativeTo(dir, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(dir, path)
}

// Load reads a fleetci.toml.
func Load(path string) (Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Config{}, err
	}
	c := Config{ConfigFile: abs}
	if err := c.loadFile(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate checks the settings Init would reject, without starting
// anything.
func (c Config) Validate() error {
	if len(c.Classes) == 0 {
		return errors.New("no classes")
	}
	if _, ok := c.Classes[c.DefaultClass]; !ok {
		return fmt.Errorf("default_class %q is not in classes", c.DefaultClass)
	}
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(c.Classes)) {
		class := c.Classes[name]
		errs = append(errs, class.Validate(name), fleet.CheckOverflow(name, c.Classes))
		if c.GitHub != nil && class.Reuses() {
			errs = append(errs, fmt.Errorf("class %q reuses VMs (capacity_per_instance %d, max_use_count %d), which a config with [github] can't: "+
				"GitHub steps run on the VM itself, so a job could read or change another job's VM. "+
				"Set both to 1 here, or keep reuse for GitLab and serve GitHub from a separate config with single-use classes and groups of their own",
				name, max(class.CapacityPerInstance, 1), max(class.MaxUseCount, 1)))
		}
	}
	if c.GitLab != nil {
		errs = append(errs, c.GitLab.Validate(slices.Collect(maps.Keys(c.Classes))))
	}
	if _, _, _, err := fleet.CheckHost(c.Host, c.Classes); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// MaxInstances is the most GitLab jobs fleetci runs at once: the sum of
// the classes' slots, max_instances × capacity_per_instance.
func (c Config) MaxInstances() int {
	n := 0
	for _, class := range c.Classes {
		n += class.Slots()
	}
	return n
}

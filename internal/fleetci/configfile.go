package fleetci

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// loadFile reads ConfigFile, a TOML file with fleetci's own settings, so
// config.toml only has to point at it:
//
//	[runners.autoscaler.plugin_config]
//	  config_file = "/etc/fleetci/fleetci.toml"
//
// The file holds default_class, classes and github with short table names
// ([classes.4vcpu-8gb], [[classes.4vcpu-8gb.policy]], [github]). Unknown
// keys are errors, so a misspelled setting is not silently ignored.
// Relative paths in it are relative to the file.
func (c *Config) loadFile() error {
	if c.ConfigFile == "" {
		return nil
	}
	if c.DefaultClass != "" || len(c.Classes) > 0 || c.GitHub != nil {
		return errors.New("with config_file, put default_class, classes and github in that file, not in plugin_config")
	}

	var raw map[string]any
	if _, err := toml.DecodeFile(c.ConfigFile, &raw); err != nil {
		return fmt.Errorf("config_file: %w", err)
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("config_file: %w", err)
	}

	var f Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return fmt.Errorf("config_file %s: %w", c.ConfigFile, err)
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
	if f.GitHub != nil && f.GitHub.TokenFile != "" {
		f.GitHub.TokenFile = relativeTo(dir, f.GitHub.TokenFile)
	}

	c.DefaultClass, c.Classes, c.GitHub = f.DefaultClass, f.Classes, f.GitHub
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

package fleetci

import (
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
)

// Finding is one problem in gitlab-runner's config.toml.
type Finding struct {
	Key  string // as written in config.toml, such as runners.autoscaler.max_instances
	Have string // what config.toml has; empty when unset
	Fix  string // the line to write
	Why  string
}

func (f Finding) String() string {
	have := f.Have
	if have == "" {
		have = "unset"
	}
	return fmt.Sprintf("%s is %s; set %s (%s)", f.Key, have, f.Fix, f.Why)
}

// CheckRunnerConfig reads gitlab-runner's config.toml, finds the [[runners]]
// entry that uses this fleetci.toml and reports every setting fleetci
// needs but the entry lacks.
func (c Config) CheckRunnerConfig(path string) ([]Finding, error) {
	var raw struct {
		Concurrent int64            `toml:"concurrent"`
		Runners    []map[string]any `toml:"runners"`
	}
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		return nil, err
	}

	var entry map[string]any
	for _, r := range raw.Runners {
		if file, _ := lookup(r, "autoscaler.plugin_config.config_file"); file == c.ConfigFile {
			entry = r
			break
		}
	}
	if entry == nil {
		return nil, fmt.Errorf("no [[runners]] entry in %s has plugin_config.config_file = %q", path, c.ConfigFile)
	}

	var findings []Finding
	for _, s := range c.required() {
		have, ok := lookup(entry, s.key)
		bad := !ok
		switch want := s.want.(type) {
		case string:
			if want == "set" {
				bad = !ok || have == ""
			} else {
				bad = have != want
			}
		case int64:
			n, isInt := have.(int64)
			bad = !isInt || (s.atMin && n < want) || (!s.atMin && n != want)
		case bool:
			bad = have != want
		}
		if !bad {
			continue
		}
		f := Finding{Key: "runners." + s.key, Why: s.why}
		if ok {
			f.Have = fmt.Sprint(have)
		}
		switch want := s.want.(type) {
		case string:
			if want == "set" {
				f.Fix = lastKey(s.key) + ` = "…"`
			} else {
				f.Fix = lastKey(s.key) + " = " + tomlString(want)
			}
		default:
			f.Fix = fmt.Sprintf("%s = %v", lastKey(s.key), want)
		}
		findings = append(findings, f)
	}

	if need := int64(c.MaxInstances()); raw.Concurrent < need {
		findings = append(findings, Finding{
			Key: "concurrent", Have: fmt.Sprint(raw.Concurrent), Fix: fmt.Sprintf("concurrent = %d", need),
			Why: "gitlab-runner runs no more jobs than this, across all its runners",
		})
	}
	return findings, nil
}

func lookup(m map[string]any, key string) (any, bool) {
	var v any = m
	for _, part := range strings.Split(key, ".") {
		table, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = table[part]; !ok {
			return nil, false
		}
	}
	return v, true
}

func lastKey(key string) string { return key[strings.LastIndex(key, ".")+1:] }

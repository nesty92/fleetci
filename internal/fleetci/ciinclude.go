package fleetci

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// CIInclude writes a GitLab CI include with one hidden job per class, so a
// job picks its machine with one line:
//
//	include:
//	  - local: .gitlab/fleetci.yml
//	build:
//	  extends: .fleetci-4vcpu-8gb
//
// A job's tags never reach the runner as variables, so the class travels in
// FLEETCI_CLASS; each hidden job sets the runner tag and the variable
// together. A misspelled class fails when the pipeline is created ("unknown
// keys in extends"), before any VM boots.
func (c Config) CIInclude(tag string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `# fleetci classes for runner tag %q. Written by "fleetci ci-include";
# write it again when the classes change.
#
#   extends: .fleetci              # the default class, %s
#   extends: .fleetci-<class>      # one of the classes below

.fleetci:
  tags: [%s]
`, tag, c.DefaultClass, yamlString(tag))
	for _, name := range slices.Sorted(maps.Keys(c.Classes)) {
		fmt.Fprintf(&b, `
.fleetci-%s:
  extends: .fleetci
  variables:
    FLEETCI_CLASS: %s
`, name, yamlString(name))
	}
	return b.String()
}

// yamlString quotes s when YAML would read it as something else.
func yamlString(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") == "" &&
		strings.ContainsAny(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		return s
	}
	return fmt.Sprintf("%q", s)
}

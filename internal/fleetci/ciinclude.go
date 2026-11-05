package fleetci

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// StandardClasses are the classes of the public GitLab CI template,
// templates/gitlab-ci.yml: a ladder of sizes, each also on arm64. An install
// whose classes carry these names needs no CI file of its own; other
// classes go in a local include after the template (CIExtra).
var StandardClasses = func() []string {
	var names []string
	for _, arch := range []string{"", "-arm"} {
		for _, size := range []string{"1vcpu-2gb", "2vcpu-4gb", "4vcpu-8gb", "8vcpu-16gb", "16vcpu-32gb"} {
			names = append(names, size+arch)
		}
	}
	return names
}()

// StandardTag is the runner tag the public template sets.
const StandardTag = "fleetci"

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
`, tag, c.DefaultClass)
	writeBase(&b, tag)
	writeClasses(&b, slices.Sorted(maps.Keys(c.Classes)))
	return b.String()
}

// CIExtra writes the classes the public template lacks, to include after
// it. It is "" when the template has them all.
func (c Config) CIExtra() string {
	var extra []string
	for _, name := range slices.Sorted(maps.Keys(c.Classes)) {
		if !slices.Contains(StandardClasses, name) {
			extra = append(extra, name)
		}
	}
	if len(extra) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`# fleetci classes the public template lacks. Written by
# "fleetci ci-include -extra"; include it after the template.
`)
	writeClasses(&b, extra)
	return b.String()
}

// StandardTemplate is templates/gitlab-ci.yml.
func StandardTemplate() string {
	var b strings.Builder
	b.WriteString(`# fleetci's GitLab CI template: one hidden job per standard class.
# Written from StandardClasses in internal/fleetci: go test ./internal/fleetci -update
#
#   include:
#     - remote: https://raw.githubusercontent.com/nesty92/fleetci/<version>/templates/gitlab-ci.yml
#   build:
#     extends: .fleetci-4vcpu-8gb   # .fleetci: the runner's default class
#
# A class the runner lacks fails when the job starts. Classes of your own
# go in a local include after this one ("fleetci ci-include -extra").
`)
	writeBase(&b, StandardTag)
	writeClasses(&b, StandardClasses)
	return b.String()
}

func writeBase(b *strings.Builder, tag string) {
	fmt.Fprintf(b, `
.fleetci:
  tags: [%s]
`, yamlString(tag))
}

func writeClasses(b *strings.Builder, names []string) {
	for _, name := range names {
		fmt.Fprintf(b, `
.fleetci-%s:
  extends: .fleetci
  variables:
    FLEETCI_CLASS: %s
`, name, yamlString(name))
	}
}

// yamlString quotes s when YAML would read it as something else.
func yamlString(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") == "" &&
		strings.ContainsAny(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		return s
	}
	return fmt.Sprintf("%q", s)
}

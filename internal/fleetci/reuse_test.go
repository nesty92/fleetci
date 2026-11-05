package fleetci

import (
	"strings"
	"testing"

	"github.com/nesty92/fleetci/internal/fleet"
	"github.com/nesty92/fleetci/internal/github"
)

func TestReuseIsForGitLabOnly(t *testing.T) {
	reuse := fleet.Class{Plugin: "p", MaxInstances: 4, MaxUseCount: 10}
	fresh := fleet.Class{Plugin: "p", MaxInstances: 2}
	c := Config{DefaultClass: "fresh", Classes: map[string]fleet.Class{"reuse": reuse, "fresh": fresh}}
	if err := c.Validate(); err != nil {
		t.Fatalf("reuse without [github]: %v", err)
	}

	c.GitHub = &github.Config{Repos: []string{"o/r"}, TokenFile: "t"}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), `class "reuse" reuses VMs`) || strings.Contains(err.Error(), `"fresh"`) {
		t.Fatalf("reuse with [github]: %v, want only the reusing class refused", err)
	}

	c.Classes["reuse"] = fleet.Class{Plugin: "p", MaxInstances: 4, CapacityPerInstance: 2}
	if err := c.Validate(); err == nil {
		t.Fatal("two jobs at once on one VM passed with [github]")
	}

	c.Classes["reuse"] = fleet.Class{Plugin: "p", MaxInstances: 4, CapacityPerInstance: 1, MaxUseCount: 1}
	if err := c.Validate(); err != nil {
		t.Fatalf("explicit single use with [github]: %v", err)
	}
}

func TestMaxInstancesCountsSlots(t *testing.T) {
	c := Config{Classes: map[string]fleet.Class{
		"a": {MaxInstances: 3, CapacityPerInstance: 2, MaxUseCount: 10},
		"b": {MaxInstances: 4},
	}}
	if n := c.MaxInstances(); n != 10 {
		t.Fatalf("MaxInstances = %d, want 3×2 + 4", n)
	}
}

package fleetci

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/nesty92/fleetci/internal/fleet"
)

var update = flag.Bool("update", false, "write templates/gitlab-ci.yml from StandardClasses")

// TestStandardTemplate keeps templates/gitlab-ci.yml as StandardTemplate
// writes it.
func TestStandardTemplate(t *testing.T) {
	path := filepath.Join("..", "..", "templates", "gitlab-ci.yml")
	want := StandardTemplate()
	if *update {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s is stale; run go test ./internal/fleetci -update", path)
	}
	var parsed map[string]map[string]any
	if err := yaml.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("not YAML: %v", err)
	}
	if len(parsed) != len(StandardClasses)+1 {
		t.Errorf("%d jobs, want .fleetci and %d classes", len(parsed), len(StandardClasses))
	}
	for _, class := range []string{"1vcpu-2gb", "4vcpu-8gb", "16vcpu-32gb-arm"} {
		if v := parsed[".fleetci-"+class]["variables"].(map[string]any)["FLEETCI_CLASS"]; v != class {
			t.Errorf(".fleetci-%s sets %v", class, v)
		}
	}
}

func TestCIExtra(t *testing.T) {
	c := Config{Classes: map[string]fleet.Class{"2vcpu-4gb": {}, "4vcpu-6gb": {}, "gpu": {}}}
	out := c.CIExtra()
	var parsed map[string]map[string]any
	if err := yaml.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("not YAML: %v\n%s", err, out)
	}
	if len(parsed) != 2 || parsed[".fleetci-4vcpu-6gb"] == nil || parsed[".fleetci-gpu"] == nil {
		t.Errorf("want only the two classes the template lacks:\n%s", out)
	}
	if strings.Contains(out, ".fleetci:") {
		t.Error("redefines .fleetci, which the template has")
	}
	if (Config{Classes: map[string]fleet.Class{"2vcpu-4gb": {}}}).CIExtra() != "" {
		t.Error("output for an install the template covers")
	}
}

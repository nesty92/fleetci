package fleetci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func loadSample(t *testing.T) Config {
	t.Helper()
	c, err := Load(writeFile(t, sampleFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRunnerConfigPassesCheck(t *testing.T) {
	c := loadSample(t)
	out, err := c.RunnerConfig(RunnerOptions{ConfigFile: c.ConfigFile, Token: "glrt-x", Global: true})
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if _, err := toml.Decode(out, &parsed); err != nil {
		t.Fatalf("not TOML: %v\n%s", err, out)
	}
	for _, want := range []string{"capacity_per_instance = 6", "concurrent = 6", `image = "class-${FLEETCI_CLASS}"`, `timeout = "5m0s"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	findings, err := c.CheckRunnerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Errorf("generated config fails its own check: %s", f)
	}
}

func TestCheckRunnerConfigFindings(t *testing.T) {
	c := loadSample(t)
	out, _ := c.RunnerConfig(RunnerOptions{ConfigFile: c.ConfigFile, Global: true})
	for _, r := range [][2]string{
		{"capacity_per_instance = 6", "capacity_per_instance = 4"},
		{`image = "class-${FLEETCI_CLASS}"`, `image = "ubuntu:24.04"`},
		{"concurrent = 6", "concurrent = 2"},
		{"        use_static_credentials = true\n", ""},
	} {
		out = strings.Replace(out, r[0], r[1], 1)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	_ = os.WriteFile(path, []byte(out), 0o600)

	findings, err := c.CheckRunnerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, f := range findings {
		keys = append(keys, f.Key)
	}
	want := []string{
		"runners.autoscaler.capacity_per_instance",
		"runners.autoscaler.vm_isolation.image",
		"runners.autoscaler.vm_isolation.connector_config.use_static_credentials",
		"concurrent",
	}
	if strings.Join(keys, " ") != strings.Join(want, " ") {
		t.Errorf("findings %v; want %v", keys, want)
	}
	if got := findings[1].String(); !strings.Contains(got, `set image = "class-${FLEETCI_CLASS}"`) {
		t.Errorf("fix not spelled out: %s", got)
	}

	if _, err := c.CheckRunnerConfig(writeFile(t, "concurrent = 1\n")); err == nil || !strings.Contains(err.Error(), "no [[runners]] entry") {
		t.Errorf("config without fleetci's entry: %v", err)
	}
}

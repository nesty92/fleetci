package fleetci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHostInTheConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleetci.toml")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	classes := `
default_class = "small"

[classes.small]
plugin = "fleeting-plugin-fleetingd"
max_instances = 8
plugin_config = { vm_num_cpu_cores = 1, vm_memory_mb = 2048 }

[classes.big]
plugin = "fleeting-plugin-fleetingd"
max_instances = 2
idle_count = 1
plugin_config = { vm_num_cpu_cores = 4, vm_memory_mb = 8192 }
`
	write(classes + `
[host]
cpus = 10
memory = "20GB"
reserve = { cpus = 2 }
nominate_after = "90s"
placement = "spread"
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Host == nil || c.Host.CPUs != 10 || c.Host.Memory != 20*1024 || *c.Host.Reserve.CPUs != 2 || time.Duration(*c.Host.NominateAfter) != 90*time.Second || c.Host.Placement != "spread" {
		t.Fatalf("host %+v", c.Host)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("fits: %v", err)
	}

	// A big job takes the warm VM and another boots to stay warm: 8 cpus.
	write(classes + "\n[host]\ncpus = 7\nmemory = \"12GB\"\n")
	c, _ = Load(path)
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), `class "big"`) {
		t.Fatalf("too small: %v", err)
	}

	write(classes + "\n[host]\ncpu = 6\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "host.cpu") {
		t.Fatalf("misspelt key: %v", err)
	}

	write(classes + "\n[host]\nenabled = false\ncpus = 1\nmemory = \"1GB\"\n")
	c, _ = Load(path)
	if err := c.Validate(); err != nil {
		t.Fatalf("enabled = false: %v", err)
	}
}

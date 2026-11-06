package fleet

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	for in, want := range map[string]Size{
		"8GB": 8192, "8gb": 8192, "8G": 8192, "8192m": 8192, "512MB": 512, "512": 512, "1.5GB": 1536, "1TB": 1 << 20, " 2 GB ": 2048,
	} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "lots", "-1GB", "8KB"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q): no error", in)
		}
	}
	if Size(8192).String() != "8GB" || Size(1536).String() != "1536MB" {
		t.Errorf("String: %s %s", Size(8192), Size(1536))
	}
}

func TestClassSize(t *testing.T) {
	yes := true
	for name, tc := range map[string]struct {
		class  Class
		want   Resources
		onHost bool
	}{
		"fleetingd": {Class{Plugin: "/usr/bin/fleeting-plugin-fleetingd", PluginConfig: RawConfig(`{"vm_num_cpu_cores":4,"vm_memory_mb":8192}`)},
			Resources{4, 8192}, true},
		"dockervm": {Class{Plugin: "/data/bin/fleeting-plugin-dockervm", PluginConfig: RawConfig(`{"cpus":"1.5","memory":"2048m"}`)},
			Resources{2, 2048}, true},
		"explicit beats the plugin": {Class{Plugin: "fleeting-plugin-fleetingd", CPUs: 2, Memory: 4096,
			PluginConfig: RawConfig(`{"vm_num_cpu_cores":4,"vm_memory_mb":8192}`)}, Resources{2, 4096}, true},
		"an unknown plugin with host and a size": {Class{Plugin: "/opt/mine", Host: &yes, CPUs: 8, Memory: 16384},
			Resources{8, 16384}, true},
	} {
		got, err := tc.class.Size()
		if err != nil || got != tc.want || tc.class.OnHost() != tc.onHost {
			t.Errorf("%s: %v %v on host %v; want %v", name, got, err, tc.class.OnHost(), tc.want)
		}
	}
	if (Class{Plugin: "fleeting-plugin-aws"}).OnHost() {
		t.Error("an aws class is on the host")
	}
	if _, err := (Class{Plugin: "/opt/mine", Host: &yes}).Size(); err == nil || !strings.Contains(err.Error(), "set cpus and memory") {
		t.Errorf("unknown size: %v", err)
	}
}

func onHostClass(cpus int, mem int64, max int) Class {
	return Class{Plugin: "fleeting-plugin-fleetingd", MaxInstances: max,
		PluginConfig: RawConfig(`{"vm_num_cpu_cores":` + strconv.Itoa(cpus) + `,"vm_memory_mb":` + strconv.FormatInt(mem, 10) + `}`)}
}

func withDetected(t *testing.T, r Resources, err error) {
	t.Helper()
	old := detect
	detect = func() (Resources, error) { return r, err }
	t.Cleanup(func() { detect = old })
}

func TestHostBudget(t *testing.T) {
	withDetected(t, Resources{CPUs: 32, MemoryMB: 128 * 1024}, nil)
	classes := map[string]Class{"vm": onHostClass(4, 8192, 4)}

	b, on, err := HostBudget(nil, classes)
	if err != nil || !on || b.CPUs != 31 || b.MemoryMB != 128*1024-128*1024/10 {
		t.Fatalf("detected: %v %v %v; want 31 cpus and a tenth of the memory kept", b, on, err)
	}
	if !strings.Contains(b.String(), "detected 32 cpus, reserve 1") {
		t.Errorf("String: %s", b)
	}

	two, mem := 2, Size(16384)
	b, _, _ = HostBudget(&HostConfig{Reserve: &Reserve{CPUs: &two, Memory: &mem}}, classes)
	if b.CPUs != 30 || b.MemoryMB != 112*1024 {
		t.Errorf("reserve: %v", b)
	}

	b, _, _ = HostBudget(&HostConfig{CPUs: 48, Memory: 64 * 1024}, classes)
	if b.CPUs != 48 || b.MemoryMB != 64*1024 || b.Detected != (Resources{}) {
		t.Errorf("explicit: %v", b)
	}

	off := false
	if _, on, _ := HostBudget(&HostConfig{Enabled: &off}, classes); on {
		t.Error("enabled = false still has a budget")
	}
	if _, on, _ := HostBudget(nil, map[string]Class{"aws": {Plugin: "fleeting-plugin-aws", MaxInstances: 1}}); on {
		t.Error("cloud classes only, yet a budget")
	}

	small := Resources{CPUs: 1, MemoryMB: 1024}
	withDetected(t, small, nil)
	if _, _, err := HostBudget(nil, classes); err == nil || !strings.Contains(err.Error(), "leaves the VMs nothing") {
		t.Errorf("tiny host: %v", err)
	}
	withDetected(t, Resources{}, errors.New("can't detect"))
	if _, _, err := HostBudget(nil, classes); err == nil || !strings.Contains(err.Error(), "set [host] cpus and memory") {
		t.Errorf("no detection: %v", err)
	}
	if b, on, err := HostBudget(&HostConfig{CPUs: 8, Memory: 16384}, classes); err != nil || !on || b.NominateAfter != defaultNominateAfter {
		t.Errorf("no detection but explicit: %v %v %v", on, err, b.NominateAfter)
	}
	if b, _, _ := HostBudget(&HostConfig{CPUs: 8, Memory: 16384}, classes); b.Placement != Pack {
		t.Errorf("default placement %q", b.Placement)
	}
	if b, _, _ := HostBudget(&HostConfig{CPUs: 8, Memory: 16384, Placement: "spread"}, classes); b.Placement != Spread {
		t.Errorf("placement %q, want spread", b.Placement)
	}
	if _, _, err := HostBudget(&HostConfig{CPUs: 8, Memory: 16384, Placement: "binpack"}, classes); err == nil || !strings.Contains(err.Error(), `want "pack" or "spread"`) {
		t.Errorf("unknown placement: %v", err)
	}
	minus := Duration(-time.Second)
	if _, _, err := HostBudget(&HostConfig{CPUs: 8, Memory: 16384, NominateAfter: &minus}, classes); err == nil {
		t.Error("negative nominate_after accepted")
	}
}

func TestFixedShares(t *testing.T) {
	withDetected(t, Resources{CPUs: 17, MemoryMB: 40960}, nil) // 16 cpus, 36GB after the reserve
	reuse := onHostClass(4, 8192, 2)
	reuse.MaxUseCount = 5
	warm := onHostClass(2, 4096, 4)
	warm.IdleCount = 1
	classes := map[string]Class{"reuse": reuse, "warm": warm, "single": onHostClass(1, 2048, 8)}

	_, reserved, on, err := CheckHost(nil, classes)
	if err != nil || !on || reserved != (Resources{CPUs: 10, MemoryMB: 20480}) {
		t.Fatalf("reserved %v %v %v; want the reused class's 2 VMs and 1 warm VM", reserved, on, err)
	}

	classes["huge"] = onHostClass(8, 16384, 1)
	if _, _, _, err := CheckHost(nil, classes); err == nil || !strings.Contains(err.Error(), `class "huge"`) {
		t.Errorf("a class that never fits: %v", err)
	}
	reuse.MaxInstances = 5
	classes = map[string]Class{"reuse": reuse}
	if _, _, _, err := CheckHost(nil, classes); err == nil || !strings.Contains(err.Error(), "fixed shares") {
		t.Errorf("fixed shares over the budget: %v", err)
	}
}

func TestDetectFromFiles(t *testing.T) {
	dir := t.TempDir()
	meminfo := filepath.Join(dir, "meminfo")
	write(t, meminfo, "MemTotal:       131072000 kB\nMemFree: 1 kB\n")
	if mb, err := memTotalMB(meminfo); err != nil || mb != 128000 {
		t.Errorf("memTotal %d %v", mb, err)
	}

	root := filepath.Join(dir, "cgroup")
	self := filepath.Join(dir, "self")
	write(t, self, "0::/machine.slice/fleetci.service\n")
	write(t, filepath.Join(root, "machine.slice", "cpu.max"), "800000 100000\n")
	write(t, filepath.Join(root, "machine.slice", "memory.max"), "68719476736\n")
	write(t, filepath.Join(root, "machine.slice", "fleetci.service", "cpu.max"), "max 100000\n")
	write(t, filepath.Join(root, "machine.slice", "fleetci.service", "memory.max"), "max\n")
	if cpus, mem := cgroupLimits(self, root); cpus != 8 || mem != 65536 {
		t.Errorf("cgroup limits %d %d; want the parent's 8 cpus and 64GB", cpus, mem)
	}
	if cpus, mem := cgroupLimits(filepath.Join(dir, "missing"), root); cpus != 0 || mem != 0 {
		t.Errorf("no cgroup file: %d %d", cpus, mem)
	}
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

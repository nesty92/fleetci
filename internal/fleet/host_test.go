package fleet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
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
	if _, on, err := HostBudget(&HostConfig{CPUs: 8, Memory: 16384}, classes); err != nil || !on {
		t.Errorf("no detection but explicit: %v %v", on, err)
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

// hostPools is a backend whose classes share a host of the given budget.
func hostPools(t *testing.T, budget Resources, classes map[string]Class) *Backend {
	t.Helper()
	hb, err := newHostBudget(&HostConfig{CPUs: budget.CPUs, Memory: Size(budget.MemoryMB)}, classes)
	if err != nil {
		t.Fatal(err)
	}
	b := &Backend{log: hclog.NewNullLogger(), vms: map[string]*pool{}, pools: map[string]*pool{}}
	for name, c := range classes {
		p := &pool{name: name, class: c, ts: &fakeScaler{free: c.MaxInstances}, timeout: 5 * time.Second}
		if hb != nil && c.OnHost() {
			p.host, p.dynamic = hb, !c.Reuses()
			p.size, _ = c.Size()
		}
		b.pools[name] = p
	}
	for _, p := range b.pools {
		if o := p.class.Overflow; o != "" {
			p.overflow = b.pools[o]
		}
	}
	return b
}

func TestTheBudgetIsNeverExceeded(t *testing.T) {
	b := hostPools(t, Resources{CPUs: 8, MemoryMB: 16384}, map[string]Class{
		"small": onHostClass(1, 2048, 8),
		"big":   onHostClass(4, 8192, 2),
	})
	h := b.pools["small"].host
	ctx := context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var peak Resources
	for i := range 40 {
		class := "small"
		if i%3 == 0 {
			class = "big"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			vm, err := b.Create(ctx, class)
			if err != nil {
				t.Error(err)
				return
			}
			h.mu.Lock()
			if !h.used.fits(h.Resources) {
				t.Errorf("over the budget: %v of %v", h.used, h.Resources)
			}
			mu.Lock()
			peak = Resources{max(peak.CPUs, h.used.CPUs), max(peak.MemoryMB, h.used.MemoryMB)}
			mu.Unlock()
			h.mu.Unlock()
			time.Sleep(time.Millisecond)
			_ = b.Delete(ctx, vm.ID)
		}()
	}
	wg.Wait()
	if h.used != (Resources{}) || len(h.waiting) != 0 {
		t.Errorf("after all jobs: used %v, %d waiting", h.used, len(h.waiting))
	}
	if peak.CPUs < 4 {
		t.Errorf("peak %v: the budget was never used", peak)
	}
}

func TestABigJobIsNotStarved(t *testing.T) {
	overflowPoll = time.Hour // only a release may wake a waiting job
	defer func() { overflowPoll = time.Second }()
	b := hostPools(t, Resources{CPUs: 4, MemoryMB: 8192}, map[string]Class{
		"small": onHostClass(1, 2048, 8),
		"big":   onHostClass(4, 8192, 1),
	})
	ctx := context.Background()
	var smalls []VM
	for range 4 {
		vm, err := b.Create(ctx, "small")
		if err != nil {
			t.Fatal(err)
		}
		smalls = append(smalls, vm)
	}

	big := make(chan VM, 1)
	go func() {
		vm, err := b.Create(ctx, "big")
		if err != nil {
			t.Error(err)
		}
		big <- vm
	}()
	waitFor(t, func() bool { return blocked(b.pools["big"].host) == 1 })

	// One small job ends: its room is the big job's, not a new small one's.
	_ = b.Delete(ctx, smalls[0].ID)
	late := make(chan VM, 1)
	go func() {
		vm, _ := b.Create(ctx, "small")
		late <- vm
	}()
	time.Sleep(50 * time.Millisecond)
	select {
	case vm := <-late:
		t.Fatalf("a later small job took %s while the big one waited", vm.Class)
	default:
	}

	for _, vm := range smalls[1:] {
		_ = b.Delete(ctx, vm.ID)
	}
	select {
	case vm := <-big:
		if vm.Class != "big" {
			t.Fatalf("big job got %q", vm.Class)
		}
		_ = b.Delete(ctx, vm.ID)
	case <-time.After(2 * time.Second):
		t.Fatal("the big job never started once the small ones ended")
	}
	select {
	case <-late:
	case <-time.After(2 * time.Second):
		t.Fatal("the later small job never started")
	}
}

func TestAJobBehindAWaitingOneOverflows(t *testing.T) {
	small := onHostClass(1, 2048, 8)
	small.Overflow = "small-aws"
	b := hostPools(t, Resources{CPUs: 4, MemoryMB: 8192}, map[string]Class{
		"small":     small,
		"small-aws": {Plugin: "fleeting-plugin-aws", MaxInstances: 4},
		"big":       onHostClass(4, 8192, 1),
	})
	ctx := context.Background()
	first, err := b.Create(ctx, "small")
	if err != nil || first.Class != "small" {
		t.Fatalf("first small: %v %v", first.Class, err)
	}
	bigCtx, cancel := context.WithCancel(ctx)
	bigDone := make(chan struct{})
	go func() {
		defer close(bigDone)
		_, _ = b.Create(bigCtx, "big")
	}()
	defer func() { cancel(); <-bigDone }()
	waitFor(t, func() bool { return blocked(b.pools["big"].host) == 1 })

	vm, err := b.Create(ctx, "small")
	if err != nil || vm.Class != "small-aws" {
		t.Fatalf("small job behind the waiting big one: %q %v, want its overflow", vm.Class, err)
	}
}

func TestAReleaseWakesAWaitingJob(t *testing.T) {
	overflowPoll = time.Hour
	defer func() { overflowPoll = time.Second }()
	b := hostPools(t, Resources{CPUs: 2, MemoryMB: 4096}, map[string]Class{"vm": onHostClass(2, 4096, 4)})
	ctx := context.Background()
	first, _ := b.Create(ctx, "vm")
	done := make(chan time.Time, 1)
	go func() {
		if _, err := b.Create(ctx, "vm"); err != nil {
			t.Error(err)
		}
		done <- time.Now()
	}()
	waitFor(t, func() bool { return blocked(b.pools["vm"].host) == 1 })
	released := time.Now()
	_ = b.Delete(ctx, first.ID)
	select {
	case at := <-done:
		if at.Sub(released) > 500*time.Millisecond {
			t.Errorf("woke after %v", at.Sub(released))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a release didn't wake the waiting job")
	}
}

func TestNoRoomSaysWhy(t *testing.T) {
	b := hostPools(t, Resources{CPUs: 2, MemoryMB: 4096}, map[string]Class{"vm": onHostClass(2, 4096, 4)})
	b.pools["vm"].timeout = 50 * time.Millisecond
	ctx := context.Background()
	if _, err := b.Create(ctx, "vm"); err != nil {
		t.Fatal(err)
	}
	_, err := b.Create(ctx, "vm")
	if err == nil || !strings.Contains(err.Error(), "the host has no room for one more of its VMs (cpus=2 memory=4GB; host cpus=2 memory=4GB in use of cpus=2 memory=4GB)") {
		t.Fatalf("err %v", err)
	}
	if n := len(b.pools["vm"].host.waiting); n != 0 {
		t.Fatalf("%d still waiting after the timeout", n)
	}
}

func blocked(h *hostBudget) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, w := range h.waiting {
		if w.blocked {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition never held")
}

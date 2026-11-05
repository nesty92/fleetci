package fleet

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// HostConfig is fleetci.toml's [host]: the CPUs and memory this machine
// lends to the VMs of its classes. Every key is optional. By default the
// budget is the machine's own cores and memory, as its cgroup limits them,
// less a reserve for the host itself.
type HostConfig struct {
	// Enabled false turns the budget off: each class is then held only to
	// its own max_instances.
	Enabled *bool `json:"enabled" toml:"enabled"`
	// CPUs and Memory set the budget outright instead of detecting it.
	// More CPUs than the machine has over-commits them on purpose.
	CPUs   int  `json:"cpus" toml:"cpus"`
	Memory Size `json:"memory" toml:"memory"`
	// Reserve is what a detected budget leaves the host: default 1 CPU and
	// a tenth of the memory, at least 2GB.
	Reserve *Reserve `json:"reserve" toml:"reserve"`
}

// Reserve is what the host keeps for itself out of a detected budget.
type Reserve struct {
	CPUs   *int  `json:"cpus" toml:"cpus"`
	Memory *Size `json:"memory" toml:"memory"`
}

// Resources is an amount of CPUs and memory.
type Resources struct {
	CPUs     int
	MemoryMB int64
}

func (r Resources) add(o Resources) Resources {
	return Resources{r.CPUs + o.CPUs, r.MemoryMB + o.MemoryMB}
}

func (r Resources) times(n int) Resources {
	return Resources{r.CPUs * n, r.MemoryMB * int64(n)}
}

func (r Resources) fits(in Resources) bool {
	return r.CPUs <= in.CPUs && r.MemoryMB <= in.MemoryMB
}

func (r Resources) String() string {
	if r == (Resources{}) {
		return "none"
	}
	return fmt.Sprintf("cpus=%d memory=%s", r.CPUs, Size(r.MemoryMB))
}

// Budget is the host's budget and where it came from.
type Budget struct {
	Resources
	// Detected and Reserve are set for the parts that were detected.
	Detected, Reserve Resources
}

func (b Budget) String() string {
	s := b.Resources.String()
	var from []string
	if b.Detected.CPUs > 0 {
		from = append(from, fmt.Sprintf("detected %d cpus, reserve %d", b.Detected.CPUs, b.Reserve.CPUs))
	}
	if b.Detected.MemoryMB > 0 {
		from = append(from, fmt.Sprintf("detected %s memory, reserve %s", Size(b.Detected.MemoryMB), Size(b.Reserve.MemoryMB)))
	}
	if len(from) > 0 {
		s += " (" + strings.Join(from, "; ") + ")"
	}
	return s
}

const minReserveMB = 2048

// detect reads this machine's CPUs and memory. A variable for tests.
var detect = detectResources

// HostBudget works out the host's budget. It is false when the budget is
// off: [host] enabled = false, or no class is on the host.
func HostBudget(h *HostConfig, classes map[string]Class) (Budget, bool, error) {
	if h == nil {
		h = &HostConfig{}
	}
	if h.Enabled != nil && !*h.Enabled {
		return Budget{}, false, nil
	}
	onHost := false
	for _, c := range classes {
		onHost = onHost || c.OnHost()
	}
	if !onHost {
		return Budget{}, false, nil
	}
	if h.CPUs < 0 || h.Memory < 0 {
		return Budget{}, false, errors.New("host: cpus and memory can't be negative")
	}

	var b Budget
	b.CPUs, b.MemoryMB = h.CPUs, int64(h.Memory)
	if b.CPUs == 0 || b.MemoryMB == 0 {
		found, err := detect()
		if err != nil {
			return Budget{}, false, fmt.Errorf("host: %w; set [host] cpus and memory, or enabled = false", err)
		}
		if b.CPUs == 0 {
			reserve := 1
			if h.Reserve != nil && h.Reserve.CPUs != nil {
				reserve = *h.Reserve.CPUs
			}
			b.Detected.CPUs, b.Reserve.CPUs = found.CPUs, reserve
			b.CPUs = found.CPUs - reserve
		}
		if b.MemoryMB == 0 {
			reserve := max(found.MemoryMB/10, minReserveMB)
			if h.Reserve != nil && h.Reserve.Memory != nil {
				reserve = int64(*h.Reserve.Memory)
			}
			b.Detected.MemoryMB, b.Reserve.MemoryMB = found.MemoryMB, reserve
			b.MemoryMB = found.MemoryMB - reserve
		}
	}
	if b.CPUs <= 0 || b.MemoryMB <= 0 {
		return Budget{}, false, fmt.Errorf("host: the reserve leaves the VMs nothing (%s)", b)
	}
	return b, true, nil
}

// OnHost reports whether the class's VMs run on this machine and so count
// against its budget: by default those of the plugins known to boot VMs
// locally (fleetingd, dockervm); host = true or false says otherwise.
func (c Class) OnHost() bool {
	if c.Host != nil {
		return *c.Host
	}
	return localPlugin(c.Plugin) != ""
}

// localPlugin names the known plugin that boots VMs on this machine, or "".
func localPlugin(plugin string) string {
	base := path.Base(filepath.ToSlash(plugin))
	for _, name := range []string{"fleetingd", "dockervm"} {
		if strings.HasPrefix(base, "fleeting-plugin-"+name) {
			return name
		}
	}
	return ""
}

// Size is the CPUs and memory of one of the class's VMs: cpus and memory
// when set, otherwise read from a known plugin's plugin_config.
func (c Class) Size() (Resources, error) {
	r := Resources{CPUs: c.CPUs, MemoryMB: int64(c.Memory)}
	if r.CPUs > 0 && r.MemoryMB > 0 {
		return r, nil
	}
	var fromPlugin Resources
	switch localPlugin(c.Plugin) {
	case "fleetingd":
		var pc struct {
			CPUs   float64 `json:"vm_num_cpu_cores"`
			Memory float64 `json:"vm_memory_mb"`
		}
		_ = json.Unmarshal(c.PluginConfig, &pc)
		fromPlugin = Resources{int(math.Ceil(pc.CPUs)), int64(pc.Memory)}
	case "dockervm":
		var pc struct {
			CPUs   any `json:"cpus"`
			Memory any `json:"memory"`
		}
		_ = json.Unmarshal(c.PluginConfig, &pc)
		if f, err := strconv.ParseFloat(fmt.Sprint(pc.CPUs), 64); err == nil {
			fromPlugin.CPUs = int(math.Ceil(f))
		}
		if s, ok := pc.Memory.(string); ok {
			if m, err := ParseSize(s); err == nil {
				fromPlugin.MemoryMB = int64(m)
			}
		}
	}
	if r.CPUs == 0 {
		r.CPUs = fromPlugin.CPUs
	}
	if r.MemoryMB == 0 {
		r.MemoryMB = fromPlugin.MemoryMB
	}
	if r.CPUs <= 0 || r.MemoryMB <= 0 {
		return Resources{}, errors.New("its VM size is unknown: set cpus and memory on the class")
	}
	return r, nil
}

// warm is how many of the class's VMs can be booted with no job: the most
// any of its policies keeps idle.
func (c Class) warm() int {
	n := 0
	for _, s := range c.schedules() {
		w := s.IdleCount
		if s.ScaleFactor > 0 {
			if s.ScaleFactorLimit > 0 {
				w = max(w, s.ScaleFactorLimit)
			} else {
				w = c.MaxInstances
			}
		}
		n = max(n, w)
	}
	return min(n, c.MaxInstances)
}

// Reserved is what the class holds of the host's budget whether or not it
// runs jobs: all its VMs when they are reused, which stay up between jobs,
// else its warm VMs. A single-use class's jobs take their share as they
// start.
func (c Class) Reserved(size Resources) Resources {
	if c.Reuses() {
		return size.times(c.MaxInstances)
	}
	return size.times(c.warm())
}

// Size is an amount of memory in MB. It reads "8GB", "512MB", "8g" or
// "8192m", and a bare number as MB; GB is 1024 MB.
type Size int64

// ParseSize reads a Size.
func ParseSize(s string) (Size, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"TB", 1 << 20}, {"GB", 1 << 10}, {"MB", 1}, {"T", 1 << 20}, {"G", 1 << 10}, {"M", 1}} {
		if strings.HasSuffix(t, u.suffix) {
			t, mult = strings.TrimSpace(strings.TrimSuffix(t, u.suffix)), u.mult
			break
		}
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("memory %q: want a size such as 8GB or 512MB", s)
	}
	return Size(math.Round(f * float64(mult))), nil
}

func (s Size) String() string {
	if s >= 1024 && s%1024 == 0 {
		return fmt.Sprintf("%dGB", s/1024)
	}
	return fmt.Sprintf("%dMB", int64(s))
}

func (s *Size) UnmarshalText(b []byte) error {
	v, err := ParseSize(string(b))
	*s = v
	return err
}

func (s *Size) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err == nil {
		return s.UnmarshalText([]byte(str))
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return errors.New("memory must be a size such as \"8GB\"")
	}
	*s = Size(n)
	return nil
}

func (s Size) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// detectResources reads the CPUs this process may use and the machine's
// memory, each lowered by a cgroup v2 limit when there is one.
func detectResources() (Resources, error) {
	if runtime.GOOS != "linux" {
		return Resources{}, fmt.Errorf("can't detect the CPUs and memory on %s", runtime.GOOS)
	}
	r := Resources{CPUs: runtime.NumCPU()}
	mem, err := memTotalMB("/proc/meminfo")
	if err != nil {
		return Resources{}, err
	}
	r.MemoryMB = mem
	cpus, memLimit := cgroupLimits("/proc/self/cgroup", "/sys/fs/cgroup")
	if cpus > 0 {
		r.CPUs = min(r.CPUs, cpus)
	}
	if memLimit > 0 {
		r.MemoryMB = min(r.MemoryMB, memLimit)
	}
	return r, nil
}

func memTotalMB(meminfo string) (int64, error) {
	f, err := os.Open(meminfo)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, err
			}
			return kb / 1024, nil
		}
	}
	return 0, fmt.Errorf("no MemTotal in %s", meminfo)
}

// cgroupLimits reads the tightest cgroup v2 cpu.max and memory.max from the
// process's cgroup up to the root; 0 means no limit.
func cgroupLimits(selfCgroup, root string) (cpus int, memMB int64) {
	b, err := os.ReadFile(selfCgroup)
	if err != nil {
		return 0, 0
	}
	var rel string
	for _, line := range strings.Split(string(b), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			rel = p
		}
	}
	if rel == "" {
		return 0, 0
	}
	for dir := filepath.Join(root, rel); ; dir = filepath.Dir(dir) {
		if b, err := os.ReadFile(filepath.Join(dir, "cpu.max")); err == nil {
			if f := strings.Fields(string(b)); len(f) == 2 && f[0] != "max" {
				quota, err1 := strconv.ParseFloat(f[0], 64)
				period, err2 := strconv.ParseFloat(f[1], 64)
				if err1 == nil && err2 == nil && period > 0 {
					n := max(int(quota/period), 1)
					if cpus == 0 || n < cpus {
						cpus = n
					}
				}
			}
		}
		if b, err := os.ReadFile(filepath.Join(dir, "memory.max")); err == nil {
			if v := strings.TrimSpace(string(b)); v != "max" {
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					mb := n / (1 << 20)
					if memMB == 0 || mb < memMB {
						memMB = mb
					}
				}
			}
		}
		if dir == root || len(dir) <= len(root) {
			return cpus, memMB
		}
	}
}

// CheckHost works out the host's budget as New would, and the fixed shares
// its classes take of it. It is false when there is no budget.
func CheckHost(h *HostConfig, classes map[string]Class) (Budget, Resources, bool, error) {
	hb, err := newHostBudget(h, classes)
	if err != nil || hb == nil {
		return Budget{}, Resources{}, false, err
	}
	return hb.Budget, hb.used, true, nil
}

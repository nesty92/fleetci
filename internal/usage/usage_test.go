package usage

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"

	"github.com/nesty92/fleetci/internal/leakcheck"
	"github.com/nesty92/fleetci/internal/vmjob/vmjobtest"
)

func TestMain(m *testing.M) { leakcheck.Main(m) }

// counters as snap prints them.
func snapText(uptime, cpuSome, memSome, memFull, ioSome string, busy, idle int, total, avail int) string {
	return "uptime " + uptime + "\n" +
		"cpu some avg10=0.00 avg60=0.00 avg300=0.00 total=" + cpuSome + "\n" +
		"cpu full avg10=0.00 avg60=0.00 avg300=0.00 total=0\n" +
		"memory some avg10=0.00 avg60=0.00 avg300=0.00 total=" + memSome + "\n" +
		"memory full avg10=0.00 avg60=0.00 avg300=0.00 total=" + memFull + "\n" +
		"io some avg10=0.00 avg60=0.00 avg300=0.00 total=" + ioSome + "\n" +
		"io full avg10=0.00 avg60=0.00 avg300=0.00 total=0\n" +
		"cpu  " + itoa(busy) + " 0 0 " + itoa(idle) + " 0 0 0 0 0 0\n" +
		"MemTotal:       " + itoa(total) + " kB\n" +
		"MemAvailable:   " + itoa(avail) + " kB\n" +
		"cpus 4\n"
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestParse(t *testing.T) {
	// 100 s: CPU some 60 s, memory some 3 s and full 0.5 s, io 10 s; 1/4
	// of the 4 CPUs' jiffies busy; peaks from the sampler.
	a := snapText("1000.00", "0", "0", "0", "0", 1000, 9000, 8<<20, 7<<20)
	b := snapText("1100.00", "60000000", "3000000", "500000", "10000000", 11000, 39000, 8<<20, 6<<20)
	r, err := parse(a + "---\n" + b + "---\npeak_mem_kb 5242880\npeak_cpu_x100 390\n")
	if err != nil {
		t.Fatal(err)
	}
	want := Report{Seconds: 100, CPUs: 4, MemoryMB: 8192, HasPSI: true,
		CPUSome: 60, MemSome: 3, MemFull: 0.5, IOSome: 10, CPUAvg: 1, CPUPeak: 3.9, MemPeakMB: 5120}
	if r != want {
		t.Fatalf("got %+v\nwant %+v", r, want)
	}
}

func TestParseWithoutPSI(t *testing.T) {
	strip := func(s string) string {
		var keep []string
		for l := range strings.SplitSeq(s, "\n") {
			if !strings.Contains(l, "avg10") {
				keep = append(keep, l)
			}
		}
		return strings.Join(keep, "\n")
	}
	a := strip(snapText("10", "0", "0", "0", "0", 0, 100, 2<<20, 1<<20))
	b := strip(snapText("70", "0", "0", "0", "0", 50, 150, 2<<20, 1<<20))
	r, err := parse(a + "---\n" + b + "---\n")
	if err != nil {
		t.Fatal(err)
	}
	if r.HasPSI || r.Seconds != 60 || r.CPUAvg != 2 || r.CPUPeak != 2 || r.MemPeakMB != 1024 {
		t.Fatalf("got %+v", r)
	}
}

func TestJudge(t *testing.T) {
	base := Report{Seconds: 300, CPUs: 4, MemoryMB: 8192, HasPSI: true, CPUAvg: 2.5, CPUPeak: 3.6, MemPeakMB: 5000}
	with := func(f func(*Report)) Report { r := base; f(&r); return r }
	for _, c := range []struct {
		name   string
		r      Report
		fit    Fit
		cpus   int
		memory int
	}{
		{"fits", base, Fits, 0, 0},
		{"short", with(func(r *Report) { r.Seconds = 10; r.CPUSome = 99 }), Short, 0, 0},
		{"cpu starved", with(func(r *Report) { r.CPUSome = 98 }), Undersized, 8, 0},
		{"cpu waiting some", with(func(r *Report) { r.CPUSome = 30 }), Undersized, 6, 0},
		{"memory stalls", with(func(r *Report) { r.MemSome = 8 }), Undersized, 0, 12288},
		{"memory all but full", with(func(r *Report) { r.MemPeakMB = 7800 }), Undersized, 0, 12288},
		{"memory short, cpu idle: only memory", with(func(r *Report) { r.MemFull = 2; r.CPUPeak = 1 }), Undersized, 0, 12288},
		{"cpu short, memory idle: only cpu", with(func(r *Report) { r.CPUSome = 60; r.MemPeakMB = 1000 }), Undersized, 8, 0},
		{"idle", with(func(r *Report) { r.CPUPeak = 1.2; r.MemPeakMB = 1500 }), Oversized, 2, 3072},
		{"cpu idle only", with(func(r *Report) { r.CPUPeak = 0.9 }), Oversized, 2, 0},
		{"disk", with(func(r *Report) { r.IOSome = 45 }), IOBound, 0, 0},
		{"disk and memory: memory", with(func(r *Report) { r.IOSome = 45; r.MemSome = 9 }), Undersized, 0, 12288},
		{"disk waits, memory near full: memory", with(func(r *Report) { r.IOSome = 21; r.MemPeakMB = 6800 }), Undersized, 0, 12288},
		{"disk waits, memory free: disk", with(func(r *Report) { r.IOSome = 35; r.MemPeakMB = 3000 }), IOBound, 0, 0},
		{"one cpu stays", with(func(r *Report) { r.CPUs = 1; r.CPUPeak = 0.1 }), Fits, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := Judge(c.r)
			if v.Fit != c.fit || v.CPUs != c.cpus || v.MemoryMB != c.memory {
				t.Fatalf("got %+v (%s), want %s cpus %d memory %d", v, v.Advice(), c.fit, c.cpus, c.memory)
			}
		})
	}
}

func TestAdvice(t *testing.T) {
	v := Judge(Report{Seconds: 120, CPUs: 2, MemoryMB: 4096, HasPSI: true, CPUSome: 97, CPUPeak: 2, MemPeakMB: 1000})
	want := "undersized: tasks waited for a CPU 97% of the time; try 4 vCPU"
	if got := v.Advice(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTrend(t *testing.T) {
	rec := NewRecorder("", hclog.NewNullLogger())
	run := func(memMB int, cpuSome float64) Run {
		return rec.Record(Run{Forge: "github", Job: "o/r build", Class: "4vcpu-8gb",
			Report: Report{Seconds: 200, CPUs: 4, MemoryMB: 8192, HasPSI: true, CPUSome: cpuSome, CPUPeak: 3, MemPeakMB: memMB}})
	}
	for range 3 {
		if r := run(4000, 2); len(r.Trend) > 0 {
			t.Fatalf("steady runs: trend %v", r.Trend)
		}
	}
	r := run(5500, 2)
	if !slices.ContainsFunc(r.Trend, func(s string) bool { return strings.HasPrefix(s, "memory peak grew") }) {
		t.Fatalf("memory grew: trend %v", r.Trend)
	}
	r = run(4000, 40)
	if !slices.ContainsFunc(r.Trend, func(s string) bool { return strings.HasPrefix(s, "CPU stall rose from 2% to 40%") }) {
		t.Fatalf("CPU stall rose: trend %v", r.Trend)
	}
	run(4000, 40)
	r = run(4000, 40)
	if !slices.Contains(r.Trend, "undersized in 3 of its last 5 runs") {
		t.Fatalf("undersized mostly: trend %v", r.Trend)
	}
	// Another job has no history.
	other := rec.Record(Run{Forge: "github", Job: "o/r test", Report: Report{Seconds: 200, CPUs: 4, MemoryMB: 8192, MemPeakMB: 7000}})
	if len(other.Trend) > 0 {
		t.Fatalf("new job: trend %v", other.Trend)
	}
}

func TestRecorderFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "usage.db")
	rec := NewRecorder(file, hclog.NewNullLogger())
	for range 3 {
		rec.Record(Run{Forge: "gitlab", Job: "g/p build", Report: Report{Seconds: 100, CPUs: 2, MemoryMB: 4096, MemPeakMB: 2000, CPUPeak: 2}})
	}
	rec.Close()
	// A new process reads the runs back, and the trend goes on.
	rec = NewRecorder(file, hclog.NewNullLogger())
	defer rec.Close()
	if n := len(rec.Runs("gitlab", "g/p build")); n != 3 {
		t.Fatalf("read back %d runs, want 3", n)
	}
	r := rec.Record(Run{Forge: "gitlab", Job: "g/p build", Report: Report{Seconds: 100, CPUs: 2, MemoryMB: 4096, MemPeakMB: 3000, CPUPeak: 2}})
	if len(r.Trend) == 0 {
		t.Fatal("no trend after reading the file back")
	}
	runs, err := ReadFile(file)
	if err != nil || len(runs) != 4 {
		t.Fatalf("file: %d runs, %v", len(runs), err)
	}
}

func TestNilRecorder(t *testing.T) {
	var rec *Recorder
	rec.Start(context.Background(), nil, "vm")
	if _, ok := rec.Collect(context.Background(), nil, "github", "j", "c", "vm"); ok {
		t.Fatal("a nil recorder collected")
	}
}

func client(t *testing.T, vm *vmjobtest.VM) *ssh.Client {
	t.Helper()
	c, err := ssh.Dial("tcp", vm.Addr, &ssh.ClientConfig{User: "t", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// TestOnThisMachine runs the sampler for real, on this machine's kernel, as
// it runs on a VM: a busy loop and some memory held should show.
func TestOnThisMachine(t *testing.T) {
	if _, err := os.Stat("/proc/stat"); err != nil {
		t.Skip("no /proc")
	}
	vm := vmjobtest.New(t)
	c := client(t, vm)
	ctx := context.Background()
	if err := Start(ctx, c, "fleetci-test-1"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The VM would end the sampler; here its process group goes.
		_ = run(ctx, c, `pkill -f "$HOME/.fleetci/usage/fleetci-test-1/sampler" || true`, nil, nil)
	})
	// Two CPUs busy for 6 s, past the sampler's first 5 s round.
	busy := `end=$(($(date +%s) + 6)); for i in 1 2; do (while [ $(date +%s) -lt $end ]; do :; done) & done; wait`
	if err := run(ctx, c, busy, nil, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond) // the sampler's write after its round
	r, err := Collect(ctx, c, "fleetci-test-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", r)
	if r.Seconds < 6 || r.CPUs < 1 || r.MemoryMB < 100 || r.MemPeakMB < 1 {
		t.Fatalf("implausible report %+v", r)
	}
	if _, err := os.Stat("/proc/pressure/cpu"); err == nil && !r.HasPSI {
		t.Fatal("this machine has PSI, the report hasn't")
	}
	if r.CPUs >= 2 && r.CPUPeak < 1.5 {
		t.Fatalf("2 CPUs busy, peak %.1f", r.CPUPeak)
	}
}

func TestCollectWithoutStart(t *testing.T) {
	vm := vmjobtest.New(t)
	_, err := Collect(context.Background(), client(t, vm), "never-started")
	if err == nil || !strings.Contains(err.Error(), "no usage recorded") {
		t.Fatalf("err %v", err)
	}
	if _, err := Collect(context.Background(), client(t, vm), "bad key"); err == nil {
		t.Fatal("a key with a space was taken")
	}
}

func TestSummarize(t *testing.T) {
	rec := NewRecorder("", hclog.NewNullLogger())
	add := func(job string, r Report) { rec.Record(Run{Forge: "github", Job: job, Class: "c", Report: r}) }
	fine := Report{Seconds: 100, CPUs: 4, MemoryMB: 8192, HasPSI: true, CPUPeak: 3, MemPeakMB: 5000}
	add("a fine", fine)
	add("b idle", Report{Seconds: 100, CPUs: 4, MemoryMB: 8192, HasPSI: true, CPUPeak: 1, MemPeakMB: 1000})
	starved := fine
	starved.CPUSome = 90
	add("c starved", starved)
	add("c starved", Report{Seconds: 5, CPUs: 4, MemoryMB: 8192})
	var all []Run
	for _, j := range []string{"a fine", "b idle", "c starved"} {
		all = append(all, rec.Runs("github", j)...)
	}
	s := Summarize(all)
	var got []string
	for _, x := range s {
		got = append(got, x.Job+" "+string(x.Last.Verdict.Fit))
	}
	want := []string{"c starved undersized", "b idle oversized", "a fine fits"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if s[0].Runs != 2 || s[0].Fits[Undersized] != 1 {
		t.Fatalf("starved: %+v", s[0])
	}
}

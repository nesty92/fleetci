package usage

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"
)

// Run is one job's record: what it used, of which class, and the verdict.
type Run struct {
	Time    time.Time `json:"time"`
	Forge   string    `json:"forge"`
	Job     string    `json:"job"` // the project or repository, and the job's name
	Class   string    `json:"class"`
	VM      string    `json:"vm"`
	Report  Report    `json:"report"`
	Verdict Verdict   `json:"verdict"`
	// Trend is what changed against the job's earlier runs.
	Trend []string `json:"trend,omitempty"`
}

// keepRuns is how many runs of each job the recorder keeps in memory for
// trends; the database keeps keepStored.
const keepRuns = 50

// Recorder measures jobs and keeps their runs: in memory, and in a SQLite
// database when it has one.
type Recorder struct {
	file string
	db   *store // nil: memory only
	log  hclog.Logger

	mu   sync.Mutex
	runs map[string][]Run // job → its runs, oldest first

	metrics *metrics
}

// NewRecorder keeps runs in the SQLite database at file ("" for memory
// only), reading the runs already in it for trends. A database it can't
// open leaves it measuring in memory only, with a warning.
func NewRecorder(file string, log hclog.Logger) *Recorder {
	r := &Recorder{file: file, log: log, runs: map[string][]Run{}, metrics: newMetrics()}
	if file == "" {
		return r
	}
	db, err := openStore(file)
	if err != nil {
		log.Warn("job usage is kept in memory only", "file", file, "err", err)
		return r
	}
	r.db = db
	runs, err := db.last(keepRuns)
	if err != nil {
		log.Warn("reading earlier job usage", "file", file, "err", err)
	}
	for _, run := range runs {
		r.add(run)
	}
	return r
}

// Close closes the recorder's database.
func (r *Recorder) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db.close()
}

func key(forge, job string) string { return forge + " " + job }

func (r *Recorder) add(run Run) {
	k := key(run.Forge, run.Job)
	runs := append(r.runs[k], run)
	if len(runs) > keepRuns {
		runs = slices.Clone(runs[len(runs)-keepRuns:])
	}
	r.runs[k] = runs
}

// Start starts measuring the job about to run on the VM. A recorder that
// is nil measures nothing. Failing to start leaves the job to run
// unmeasured, so it is only logged.
func (r *Recorder) Start(ctx context.Context, client *ssh.Client, vm string) {
	if r == nil {
		return
	}
	// A VM slow to answer delays its job by no more than this.
	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	if err := Start(ctx, client, vm); err != nil {
		r.log.Warn("not measuring the job's usage", "vm", vm, "err", err)
	}
}

// Collect reads what the job on the VM used, judges it, compares it with
// the job's earlier runs and keeps it, and logs the verdict: a warning
// when the job outgrew its class or is outgrowing it.
func (r *Recorder) Collect(ctx context.Context, client *ssh.Client, forge, job, class, vm string) (Run, bool) {
	if r == nil {
		return Run{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, collectTimeout)
	defer cancel()
	rep, err := Collect(ctx, client, vm)
	if err != nil {
		r.log.Debug("no usage for the job", "vm", vm, "err", err)
		return Run{}, false
	}
	return r.Record(Run{Time: time.Now().UTC(), Forge: forge, Job: job, Class: class, VM: vm, Report: rep}), true
}

// Record judges a measured run and keeps it.
func (r *Recorder) Record(run Run) Run {
	run.Verdict = Judge(run.Report)
	r.mu.Lock()
	run.Trend = Trend(r.runs[key(run.Forge, run.Job)], run)
	r.add(run)
	r.mu.Unlock()
	r.append(run)
	r.metrics.observe(run)

	rep := run.Report
	args := []any{"forge", run.Forge, "job", run.Job, "class", run.Class, "vm", run.VM,
		"fit", string(run.Verdict.Fit), "advice", run.Verdict.Advice(),
		"seconds", int(rep.Seconds), "cpus", rep.CPUs, "cpu_avg", rep.CPUAvg, "cpu_peak", rep.CPUPeak,
		"mem_peak_mb", rep.MemPeakMB, "memory_mb", rep.MemoryMB}
	if rep.HasPSI {
		args = append(args, "cpu_some", rep.CPUSome, "mem_some", rep.MemSome, "mem_full", rep.MemFull, "io_some", rep.IOSome)
	}
	if len(run.Trend) > 0 {
		args = append(args, "trend", run.Trend)
	}
	if run.Verdict.Fit == Undersized || len(run.Trend) > 0 {
		r.log.Warn("job usage", args...)
	} else {
		r.log.Info("job usage", args...)
	}
	return run
}

func (r *Recorder) append(run Run) {
	if r.db == nil {
		return
	}
	if err := r.db.add(run); err != nil {
		r.log.Warn("keeping job usage", "file", r.file, "err", err)
	}
}

// Runs are the job's runs the recorder holds, oldest first.
func (r *Recorder) Runs(forge, job string) []Run {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.runs[key(forge, job)])
}

// trendRuns is how many earlier runs a run is compared with, and
// trendMin the fewest that make a trend.
const (
	trendRuns = 10
	trendMin  = 3
)

// Trend says what grew in run against the median of the job's earlier
// runs, as their sizes allow: its memory peak or CPUs busy by a quarter,
// its CPU or memory stall by 10 points, its time by a quarter; and a class
// found undersized in most of the last runs.
func Trend(earlier []Run, run Run) []string {
	var prev []Run
	for _, e := range earlier {
		if e.Verdict.Fit != Short {
			prev = append(prev, e)
		}
	}
	if len(prev) > trendRuns {
		prev = prev[len(prev)-trendRuns:]
	}
	if len(prev) < trendMin || run.Verdict.Fit == Short {
		return nil
	}
	med := func(f func(Report) float64) float64 {
		v := make([]float64, len(prev))
		for i, p := range prev {
			v[i] = f(p.Report)
		}
		slices.Sort(v)
		if n := len(v); n%2 == 0 {
			return (v[n/2-1] + v[n/2]) / 2
		}
		return v[len(v)/2]
	}
	var out []string
	grew := func(what string, f func(Report) float64, unit string) {
		m, now := med(f), f(run.Report)
		if m > 0 && now >= 1.25*m {
			out = append(out, fmt.Sprintf("%s grew from %.1f%s to %.1f%s", what, m, unit, now, unit))
		}
	}
	rose := func(what string, f func(Report) float64) {
		if !run.Report.HasPSI {
			return
		}
		m, now := med(f), f(run.Report)
		if now-m >= 10 {
			out = append(out, fmt.Sprintf("%s rose from %.0f%% to %.0f%%", what, m, now))
		}
	}
	grew("memory peak", func(r Report) float64 { return float64(r.MemPeakMB) / 1024 }, " GB")
	grew("CPUs busy at peak", func(r Report) float64 { return r.CPUPeak }, "")
	rose("CPU stall", func(r Report) float64 { return r.CPUSome })
	rose("memory stall", func(r Report) float64 { return r.MemSome })
	grew("time", func(r Report) float64 { return r.Seconds / 60 }, " min")

	last := slices.Concat(prev[max(0, len(prev)-4):], []Run{run})
	under := 0
	for _, l := range last {
		if l.Verdict.Fit == Undersized {
			under++
		}
	}
	if len(last) >= trendMin && under*2 > len(last) {
		out = append(out, fmt.Sprintf("undersized in %d of its last %d runs", under, len(last)))
	}
	return out
}

var (
	defaultMu sync.Mutex
	def       *Recorder
)

// SetDefault makes r the recorder Default returns: the one the job sources
// measure their jobs with. Until it is set, nothing is measured.
func SetDefault(r *Recorder) {
	defaultMu.Lock()
	def = r
	defaultMu.Unlock()
}

// Default is the recorder SetDefault set, or nil.
func Default() *Recorder {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	return def
}

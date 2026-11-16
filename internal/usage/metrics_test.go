package usage

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestMetrics(t *testing.T) {
	file := filepath.Join(t.TempDir(), "usage.db")
	rec := NewRecorder(file, hclog.NewNullLogger())
	defer rec.Close()
	starved := Report{Seconds: 100, CPUs: 2, MemoryMB: 4096, MemPeakMB: 1000, CPUPeak: 2, HasPSI: true, CPUSome: 60}
	rec.Record(Run{Forge: "github", Job: "a/b build", Class: "small", Report: starved})
	rec.Record(Run{Forge: "github", Job: "a/b build", Class: "small", Report: Report{Seconds: 5, CPUs: 2, MemoryMB: 4096}})

	m := rec.metrics
	if n := testutil.ToFloat64(m.fit.WithLabelValues("github", "small", "undersized")); n != 1 {
		t.Errorf("undersized = %v, want 1", n)
	}
	if n := testutil.ToFloat64(m.fit.WithLabelValues("github", "small", "short")); n != 1 {
		t.Errorf("short = %v, want 1", n)
	}
	// The short run is counted, not observed.
	want := `
# HELP fleetci_job_cpu_wait_ratio The share of a job's time some task waited for a CPU (PSI cpu some).
# TYPE fleetci_job_cpu_wait_ratio histogram
fleetci_job_cpu_wait_ratio_bucket{class="small",forge="github",le="0.01"} 0
fleetci_job_cpu_wait_ratio_bucket{class="small",forge="github",le="0.05"} 0
fleetci_job_cpu_wait_ratio_bucket{class="small",forge="github",le="0.1"} 0
fleetci_job_cpu_wait_ratio_bucket{class="small",forge="github",le="0.2"} 0
fleetci_job_cpu_wait_ratio_bucket{class="small",forge="github",le="0.3"} 0
fleetci_job_cpu_wait_ratio_bucket{class="small",forge="github",le="0.5"} 0
fleetci_job_cpu_wait_ratio_bucket{class="small",forge="github",le="0.75"} 1
fleetci_job_cpu_wait_ratio_bucket{class="small",forge="github",le="1"} 1
fleetci_job_cpu_wait_ratio_bucket{class="small",forge="github",le="+Inf"} 1
fleetci_job_cpu_wait_ratio_sum{class="small",forge="github"} 0.6
fleetci_job_cpu_wait_ratio_count{class="small",forge="github"} 1
`
	if err := testutil.CollectAndCompare(rec.Metrics(), strings.NewReader(want), "fleetci_job_cpu_wait_ratio"); err != nil {
		t.Error(err)
	}

	// Runs read back from the file are history, not this process's jobs.
	again := NewRecorder(file, hclog.NewNullLogger())
	defer again.Close()
	if n := testutil.CollectAndCount(again.Metrics(), "fleetci_job_fit_total"); n != 0 {
		t.Errorf("a new recorder counts %d series from its file, want 0", n)
	}
}

func TestMetricsTrend(t *testing.T) {
	rec := NewRecorder("", hclog.NewNullLogger())
	r := Report{Seconds: 100, CPUs: 4, MemoryMB: 8192, MemPeakMB: 3000, CPUPeak: 3, HasPSI: true}
	for range 3 {
		rec.Record(Run{Forge: "gitlab", Job: "g/p test", Class: "big", Report: r})
	}
	r.MemPeakMB = 5000
	rec.Record(Run{Forge: "gitlab", Job: "g/p test", Class: "big", Report: r})
	if n := testutil.ToFloat64(rec.metrics.outgrow.WithLabelValues("gitlab", "big")); n != 1 {
		t.Errorf("outgrowing = %v, want 1", n)
	}
}

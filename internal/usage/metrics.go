package usage

import (
	"github.com/prometheus/client_golang/prometheus"
)

// metrics are the recorder's figures for Prometheus, by forge and class:
// a job's own name would make a series per job, so jobs stay in the
// recorder's file and in "fleetci usage". Each job is counted once, by the
// process that ran it: with several controllers, sum them.
type metrics struct {
	fit       *prometheus.CounterVec
	outgrow   *prometheus.CounterVec
	cpuWait   *prometheus.HistogramVec
	memWait   *prometheus.HistogramVec
	ioWait    *prometheus.HistogramVec
	cpuPeak   *prometheus.HistogramVec
	memPeak   *prometheus.HistogramVec
	collected []prometheus.Collector
}

var (
	waitBuckets = []float64{0.01, 0.05, 0.1, 0.2, 0.3, 0.5, 0.75, 1}
	peakBuckets = []float64{0.1, 0.25, 0.5, 0.75, 0.9, 1}
)

func newMetrics() *metrics {
	labels := []string{"forge", "class"}
	hist := func(name, help string, buckets []float64) *prometheus.HistogramVec {
		return prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets}, labels)
	}
	m := &metrics{
		fit: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fleetci_job_fit_total",
			Help: "Jobs measured on their VM, by how their class fit them: undersized, oversized, io-bound, fits, or short (under 30 s, not judged).",
		}, []string{"forge", "class", "fit"}),
		outgrow: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fleetci_job_outgrowing_total",
			Help: "Jobs that needed more than their earlier runs: memory or CPU peak, CPU or memory wait, or time up, or undersized in most of their last runs.",
		}, labels),
		cpuWait: hist("fleetci_job_cpu_wait_ratio", "The share of a job's time some task waited for a CPU (PSI cpu some).", waitBuckets),
		memWait: hist("fleetci_job_memory_wait_ratio", "The share of a job's time some task waited for memory (PSI memory some).", waitBuckets),
		ioWait:  hist("fleetci_job_io_wait_ratio", "The share of a job's time some task waited for the disk (PSI io some).", waitBuckets),
		cpuPeak: hist("fleetci_job_cpu_peak_ratio", "The CPUs busy over a job's busiest 5 s, of its VM's CPUs.", peakBuckets),
		memPeak: hist("fleetci_job_memory_peak_ratio", "A job's peak memory in use, of its VM's memory.", peakBuckets),
	}
	m.collected = []prometheus.Collector{m.fit, m.outgrow, m.cpuWait, m.memWait, m.ioWait, m.cpuPeak, m.memPeak}
	return m
}

func (m *metrics) observe(run Run) {
	m.fit.WithLabelValues(run.Forge, run.Class, string(run.Verdict.Fit)).Inc()
	if len(run.Trend) > 0 {
		m.outgrow.WithLabelValues(run.Forge, run.Class).Inc()
	}
	if run.Verdict.Fit == Short {
		return
	}
	r := run.Report
	if r.HasPSI {
		m.cpuWait.WithLabelValues(run.Forge, run.Class).Observe(r.CPUSome / 100)
		m.memWait.WithLabelValues(run.Forge, run.Class).Observe(r.MemSome / 100)
		m.ioWait.WithLabelValues(run.Forge, run.Class).Observe(r.IOSome / 100)
	}
	if r.CPUs > 0 {
		m.cpuPeak.WithLabelValues(run.Forge, run.Class).Observe(min(r.CPUPeak/float64(r.CPUs), 1))
	}
	if r.MemoryMB > 0 {
		m.memPeak.WithLabelValues(run.Forge, run.Class).Observe(min(float64(r.MemPeakMB)/float64(r.MemoryMB), 1))
	}
}

func (m *metrics) Describe(ch chan<- *prometheus.Desc) {
	for _, c := range m.collected {
		c.Describe(ch)
	}
}

func (m *metrics) Collect(ch chan<- prometheus.Metric) {
	for _, c := range m.collected {
		c.Collect(ch)
	}
}

// Metrics are the recorder's jobs' figures for Prometheus, counted from
// when the recorder started: the runs read back from its file are not.
func (r *Recorder) Metrics() prometheus.Collector { return r.metrics }

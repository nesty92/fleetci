package cluster

import (
	"cmp"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// metricsHandler serves /metrics.
func (c *Controller) metricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(c.metrics.reg, promhttp.HandlerOpts{}))
	return mux
}

// metrics are a controller's, for Prometheus. Every controller has the
// whole fleet's: the workers report to all of them, and each counts the
// jobs it sees end, so any one can be scraped; with more, dashboards take
// the max by the labels they keep.
type metrics struct {
	reg      *prometheus.Registry
	jobs     *prometheus.CounterVec
	vms      *prometheus.CounterVec
	duration *prometheus.HistogramVec
	boot     *prometheus.HistogramVec

	cacheFrom     *prometheus.CounterVec
	leaderChanges prometheus.Counter
	queueWait     *prometheus.HistogramVec
	pickup        *prometheus.HistogramVec
	ghRequests    *prometheus.CounterVec
	ghSeconds     prometheus.Histogram
	ghRemaining   *prometheus.GaugeVec
}

func newMetrics(c *Controller) *metrics {
	m := &metrics{
		reg: prometheus.NewRegistry(),
		jobs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fleetci_fleet_jobs_total", Help: "Jobs the workers ran, by how they ended.",
		}, []string{"forge", "class", "worker", "result"}),
		vms: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fleetci_fleet_vms_total",
			Help: "VMs the leader asked for, by what came of them: job (one ran), unused (cancelled before a job), boot_failed, no_room.",
		}, []string{"class", "outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "fleetci_fleet_job_duration_seconds", Help: "How long jobs ran, on their VM.",
			Buckets: []float64{10, 30, 60, 120, 300, 600, 1200, 1800, 3600, 7200, 14400},
		}, []string{"forge", "class"}),
		boot: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "fleetci_fleet_vm_boot_seconds", Help: "From the leader's assignment to the VM being up, for the VMs jobs ran on.",
			Buckets: []float64{1, 2, 5, 10, 20, 30, 60, 120, 300, 600},
		}, []string{"class"}),
		cacheFrom: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fleetci_fleet_job_cache_total",
			Help: "Jobs of classes with cache disks, by what their disk started from: own, fallback (main's), empty (the first), none (no disk).",
		}, []string{"forge", "class", "from"}),
		leaderChanges: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "fleetci_fleet_leader_changes_total", Help: "Times this controller saw the lead change hands.",
		}),
		queueWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "fleetci_fleet_queue_wait_seconds", Help: "From the forge queuing a job to its VM being up, as this controller led.",
			Buckets: []float64{1, 2, 5, 10, 20, 30, 60, 120, 300, 600, 1800, 3600},
		}, []string{"forge", "class"}),
		pickup: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "fleetci_fleet_pickup_seconds", Help: "From the forge queuing a job to fleetci first taking it, by how it came (poll or webhook), as this controller led: fleetci's own part of the queue wait.",
			Buckets: []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300},
		}, []string{"forge", "class", "via"}),
		ghRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fleetci_github_api_requests_total", Help: "Calls to GitHub's API, by HTTP status; code 0 for those that got no answer.",
		}, []string{"code"}),
		ghSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "fleetci_github_api_request_seconds", Help: "How long GitHub's API took to answer.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30},
		}),
		ghRemaining: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "fleetci_github_api_ratelimit_remaining", Help: "The calls GitHub's rate limit had left, as its last answer said, by resource.",
		}, []string{"resource"}),
	}
	m.reg.MustRegister(m.jobs, m.vms, m.duration, m.boot, m.cacheFrom, m.leaderChanges, m.queueWait, m.pickup, m.ghRequests, m.ghSeconds, m.ghRemaining, fleetCollector{c},
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// vmDone counts an assignment that ended.
func (m *metrics) vmDone(s AssignmentStatus) {
	outcome := "job"
	switch {
	case s.Job != "":
	case s.Reason == ReasonNoRoom:
		outcome = "no_room"
	case s.Booted.IsZero():
		outcome = "boot_failed"
	default:
		outcome = "unused"
	}
	m.vms.WithLabelValues(s.Class, outcome).Inc()
}

// jobDone counts a job that ended.
func (m *metrics) jobDone(r JobRecord) {
	m.jobs.WithLabelValues(r.Forge, r.Class, r.Worker, r.Result).Inc()
	if d := r.Duration(); d > 0 {
		m.duration.WithLabelValues(r.Forge, r.Class).Observe(d.Seconds())
	}
	if d := r.Boot(); d > 0 {
		m.boot.WithLabelValues(r.Class).Observe(d.Seconds())
	}
	if r.CacheFrom != "" {
		m.cacheFrom.WithLabelValues(r.Forge, r.Class, r.CacheFrom).Inc()
	}
}

// fleetCollector reads the fleet's gauges from the controller's status at
// each scrape.
type fleetCollector struct{ c *Controller }

var (
	descController = prometheus.NewDesc("fleetci_fleet_controller_info", "This controller: its id and fleetci version.", []string{"controller", "version"}, nil)
	descLeader     = prometheus.NewDesc("fleetci_fleet_leader", "1 if this controller leads: it finds jobs and places them.", nil, nil)
	descLive       = prometheus.NewDesc("fleetci_fleet_controllers_live", "Controllers this one heard from lately, itself included.", nil, nil)
	descWorker     = prometheus.NewDesc("fleetci_fleet_worker_info", "A worker and its fleetci version.", []string{"worker", "version"}, nil)
	descState      = prometheus.NewDesc("fleetci_fleet_worker_state", "1 for a worker's state: Ready, NotReady, Draining or Unreachable.", []string{"worker", "state"}, nil)
	descBudgetCPU  = prometheus.NewDesc("fleetci_fleet_worker_budget_cpus", "The CPUs a worker gives its VMs.", []string{"worker"}, nil)
	descBudgetMem  = prometheus.NewDesc("fleetci_fleet_worker_budget_memory_bytes", "The memory a worker gives its VMs.", []string{"worker"}, nil)
	descUsedCPU    = prometheus.NewDesc("fleetci_fleet_worker_used_cpus", "The CPUs a worker's VMs have.", []string{"worker"}, nil)
	descUsedMem    = prometheus.NewDesc("fleetci_fleet_worker_used_memory_bytes", "The memory a worker's VMs have.", []string{"worker"}, nil)
	descRoom       = prometheus.NewDesc("fleetci_fleet_worker_room", "How many more VMs of a class a worker has room for.", []string{"worker", "class"}, nil)
	descVMs        = prometheus.NewDesc("fleetci_fleet_worker_vms", "A worker's VMs, by class and job: github, gitlab, or idle.", []string{"worker", "class", "job"}, nil)

	descCacheUsed     = prometheus.NewDesc("fleetci_fleet_worker_cache_used_bytes", "What a worker's cache disks take.", []string{"worker"}, nil)
	descCacheBudget   = prometheus.NewDesc("fleetci_fleet_worker_cache_budget_bytes", "What a worker's cache disks may take.", []string{"worker"}, nil)
	descCacheLow      = prometheus.NewDesc("fleetci_fleet_worker_cache_low_space", "1 when a worker's cache disks' filesystem is under min_free: its jobs get no cache disk.", []string{"worker"}, nil)
	descCacheEvicted  = prometheus.NewDesc("fleetci_fleet_worker_cache_evictions_total", "Caches a worker dropped, over budget or too old, since it started.", []string{"worker"}, nil)
	descCacheFlattens = prometheus.NewDesc("fleetci_fleet_worker_cache_flattens_total", "Cache chains a worker flattened since it started.", []string{"worker"}, nil)
	descCacheCopied   = prometheus.NewDesc("fleetci_fleet_worker_cache_copied_bytes_total", "Bytes of cache a worker copied from its neighbours since it started.", []string{"worker"}, nil)
)

func (fleetCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descController, descLeader, descLive, descWorker, descState,
		descBudgetCPU, descBudgetMem, descUsedCPU, descUsedMem, descRoom, descVMs,
		descCacheUsed, descCacheBudget, descCacheLow, descCacheEvicted, descCacheFlattens, descCacheCopied} {
		ch <- d
	}
}

func (f fleetCollector) Collect(ch chan<- prometheus.Metric) {
	st := f.c.Status()
	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	gauge(descController, 1, st.Controller, st.Versions[st.Controller])
	leader, live := 0.0, 0.0
	if st.Leader == st.Controller {
		leader = 1
	}
	for _, seen := range st.Controllers {
		if time.Since(seen) < leaderTTL {
			live++
		}
	}
	gauge(descLeader, leader)
	gauge(descLive, live)
	const mb = 1 << 20
	for _, w := range st.Workers {
		gauge(descWorker, 1, w.ID, w.Facts.Version)
		for _, s := range []string{Ready, NotReady, Draining, Unreachable} {
			v := 0.0
			if w.State == s {
				v = 1
			}
			gauge(descState, v, w.ID, s)
		}
		gauge(descBudgetCPU, float64(w.Facts.CPUs), w.ID)
		gauge(descBudgetMem, float64(w.Facts.MemoryMB*mb), w.ID)
		gauge(descUsedCPU, float64(w.Report.UsedCPUs), w.ID)
		gauge(descUsedMem, float64(w.Report.UsedMemory*mb), w.ID)
		for _, cl := range w.Fit.Classes {
			gauge(descRoom, float64(w.Report.Room[cl.Name]), w.ID, cl.Name)
		}
		type key struct{ class, job string }
		n := map[key]int{}
		for _, v := range w.Report.VMs {
			job := v.Job
			if job == "" {
				job = "idle"
			}
			n[key{v.Class, job}]++
		}
		for k, v := range n {
			gauge(descVMs, float64(v), w.ID, k.class, k.job)
		}
		if cs := w.Report.CacheStats; cs != nil {
			low := 0.0
			if cs.LowSpace {
				low = 1
			}
			gauge(descCacheUsed, float64(cs.Used), w.ID)
			gauge(descCacheBudget, float64(cs.Budget), w.ID)
			gauge(descCacheLow, low, w.ID)
			counter := func(d *prometheus.Desc, v int64) {
				ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, float64(v), w.ID)
			}
			counter(descCacheEvicted, cs.Evicted)
			counter(descCacheFlattens, cs.Flattens)
			counter(descCacheCopied, cs.Copied)
		}
	}
}

// GitHubMetrics are what the GitHub dispatcher reports to the metrics: its
// API calls, through the transport, and each job's wait for a VM.
func (c *Controller) GitHubMetrics() (http.RoundTripper, func(class string, wait time.Duration)) {
	m := c.metrics
	return githubTransport{m: m, next: http.DefaultTransport}, func(class string, wait time.Duration) {
		m.queueWait.WithLabelValues("github", class).Observe(wait.Seconds())
	}
}

// GitHubPickup is what the GitHub dispatcher tells how long each job took
// to reach it.
func (c *Controller) GitHubPickup() func(class, via string, wait time.Duration) {
	return func(class, via string, wait time.Duration) {
		c.metrics.pickup.WithLabelValues("github", class, via).Observe(wait.Seconds())
	}
}

// GitLabQueueWait is what the GitLab dispatcher tells how long each job
// waited, from GitLab queuing it to its VM being up.
func (c *Controller) GitLabQueueWait() func(class string, wait time.Duration) {
	return func(class string, wait time.Duration) {
		c.metrics.queueWait.WithLabelValues("gitlab", class).Observe(wait.Seconds())
	}
}

// githubTransport counts and times GitHub's API calls, and keeps what the
// rate limit has left.
type githubTransport struct {
	m    *metrics
	next http.RoundTripper
}

func (t githubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.next.RoundTrip(r)
	t.m.ghSeconds.Observe(time.Since(start).Seconds())
	if err != nil {
		t.m.ghRequests.WithLabelValues("0").Inc()
		return resp, err
	}
	t.m.ghRequests.WithLabelValues(strconv.Itoa(resp.StatusCode)).Inc()
	if left, err := strconv.ParseFloat(resp.Header.Get("X-RateLimit-Remaining"), 64); err == nil {
		t.m.ghRemaining.WithLabelValues(cmp.Or(resp.Header.Get("X-RateLimit-Resource"), "core")).Set(left)
	}
	return resp, nil
}

package fleet

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	flmetrics "gitlab.com/gitlab-org/fleeting/fleeting/metrics"
	tsmetrics "gitlab.com/gitlab-org/fleeting/taskscaler/metrics"
)

// metrics are a backend's figures for Prometheus, by class: what its jobs
// waited for, what its VMs did, and its hosts' budgets. The counters and
// histograms are kept as things happen; the gauges are read at each
// scrape, or pushed by each class's taskscaler and fleeting provisioner,
// which report through the collectors below with the class as a label.
//
// gitlab-runner has its own fleeting_* metrics for its instance group; in
// plugin mode that group is fleetci's one gateway, so these are the only
// figures about the classes' VMs.
type metrics struct {
	wait      *prometheus.HistogramVec
	acquired  *prometheus.CounterVec
	failed    *prometheus.CounterVec
	held      *prometheus.HistogramVec
	overflow  *prometheus.CounterVec
	nominated *prometheus.CounterVec
	notices   *prometheus.CounterVec

	// Reported by taskscaler and fleeting, per class.
	slots       *prometheus.GaugeVec
	instances   *prometheus.GaugeVec
	ready       *prometheus.HistogramVec
	running     *prometheus.HistogramVec
	lifetime    *prometheus.HistogramVec
	scaleOps    *prometheus.CounterVec
	instanceOps *prometheus.CounterVec

	collected []prometheus.Collector
}

var (
	waitBuckets = []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300, 600, 1800}
	bootBuckets = []float64{0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300, 600}
	jobBuckets  = []float64{10, 30, 60, 120, 300, 600, 1200, 1800, 3600, 7200, 14400}
)

func newMetrics() *metrics {
	class := []string{"class"}
	counter := func(name, help string, labels ...string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, append(class, labels...))
	}
	gauge := func(name, help string, labels ...string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, append(class, labels...))
	}
	hist := func(name, help string, buckets []float64) *prometheus.HistogramVec {
		return prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets}, class)
	}
	m := &metrics{
		wait: hist("fleetci_vm_wait_seconds",
			"From a job asking for a VM of its class to having one: the wait for room, for a VM to boot, or neither.", waitBuckets),
		acquired: counter("fleetci_vms_acquired_total",
			"VMs jobs got, by whether the VM was up before the job asked (warm: idle_count or a VM left by an ended wait) or booted for it (cold).", "start"),
		failed: counter("fleetci_vm_acquire_failures_total",
			"Jobs that got no VM: timeout (acquire_timeout passed, waiting for room or a boot), cancelled (the job stopped waiting) or error.", "reason"),
		held: hist("fleetci_vm_held_seconds",
			"How long a job held its VM, from getting it to giving it back: about the job's run.", jobBuckets),
		overflow: counter("fleetci_overflow_total",
			"Jobs of a full class that ran on its overflow class, by that class.", "to"),
		nominated: counter("fleetci_nominations_total",
			"Jobs that waited nominate_after and had a host hold the room that frees for them."),
		notices: counter("fleetci_reclaim_notices_total",
			"Notices the cloud gave about a class's VMs: reclaim (the VM is being taken back; it takes no new jobs) or rebalance (a recommendation).", "kind"),

		slots: gauge("fleetci_class_slots",
			"The class's job slots as its taskscaler counts them: idle (ready for a job), pending (booting), acquired, reserved, unavailable, unhealthy, suspended.", "state"),
		instances: gauge("fleetci_class_instances",
			"The class's instances as its fleeting provisioner counts them: requested, pending, creating, running, deleting and others.", "state"),
		ready: hist("fleetci_instance_ready_seconds",
			"From an instance running to fleetci admitting it: connecting to it and its checks (Docker, mirrors).", bootBuckets),
		running: hist("fleetci_instance_boot_seconds",
			"From asking the plugin for an instance to the plugin reporting it running.", bootBuckets),
		lifetime: hist("fleetci_instance_lifetime_seconds",
			"How long instances lived, from running to deleted.", jobBuckets),
		scaleOps: counter("fleetci_class_scale_operations_total",
			"The class's taskscaler scaling up or down.", "operation"),
		instanceOps: counter("fleetci_class_instance_operations_total",
			"Instance operations the class's fleeting provisioner did: create, delete, suspend, resume.", "operation"),
	}
	m.collected = []prometheus.Collector{m.wait, m.acquired, m.failed, m.held, m.overflow, m.nominated, m.notices,
		m.slots, m.instances, m.ready, m.running, m.lifetime, m.scaleOps, m.instanceOps}
	return m
}

// The recorders below take a nil m for no metrics.

// got records a job getting a VM of p's class, asked for as class at
// start; warm says the VM was up before then.
func (m *metrics) got(p *pool, class string, start time.Time, warm bool) {
	if m == nil {
		return
	}
	m.wait.WithLabelValues(class).Observe(time.Since(start).Seconds())
	how := "cold"
	if warm {
		how = "warm"
	}
	m.acquired.WithLabelValues(p.name, how).Inc()
	if p.name != class {
		m.overflow.WithLabelValues(class, p.name).Inc()
	}
}

// gotNone records a job of the class that got no VM.
func (m *metrics) gotNone(class string, err error) {
	if m == nil {
		return
	}
	reason := "error"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		reason = "timeout"
	case errors.Is(err, context.Canceled):
		reason = "cancelled"
	}
	m.failed.WithLabelValues(class, reason).Inc()
}

// released records a VM given back after being held since.
func (m *metrics) released(class string, since time.Time) {
	if m == nil || since.IsZero() {
		return
	}
	m.held.WithLabelValues(class).Observe(time.Since(since).Seconds())
}

func (m *metrics) nominate(class string) {
	if m != nil {
		m.nominated.WithLabelValues(class).Inc()
	}
}

// notice returns what a class's reclaim watch reports its notices to.
func (m *metrics) notice(class string) func(kind string) {
	if m == nil {
		return nil
	}
	return func(kind string) {
		if kind != "rebalance" {
			kind = "reclaim"
		}
		m.notices.WithLabelValues(class, kind).Inc()
	}
}

// scaler and provisioner are what a class's taskscaler and fleeting
// provisioner report to; nil metrics give nil, which they take for none.
func (m *metrics) scaler(class string) tsmetrics.Collector {
	if m == nil {
		return nil
	}
	return tsCollector{m, class}
}

func (m *metrics) provisioner(class string) flmetrics.Collector {
	if m == nil {
		return nil
	}
	return flCollector{m, class}
}

// tsCollector is taskscaler's metrics.Collector for one class.
type tsCollector struct {
	m     *metrics
	class string
}

func (c tsCollector) MaxUseCountSet(int)         {}
func (c tsCollector) MaxTasksPerInstanceSet(int) {}
func (c tsCollector) DesiredInstancesSet(int)    {}
func (c tsCollector) TaskOperationInc(string)    {}
func (c tsCollector) ScaleOperationInc(op string) {
	c.m.scaleOps.WithLabelValues(c.class, op).Inc()
}
func (c tsCollector) TasksCountSet(state string, v int) {
	c.m.slots.WithLabelValues(c.class, state).Set(float64(v))
}
func (c tsCollector) TaskInstanceReadinessTimeObserve(d time.Duration) {
	c.m.ready.WithLabelValues(c.class).Observe(d.Seconds())
}

// flCollector is fleeting's metrics.Collector for one class.
type flCollector struct {
	m     *metrics
	class string
}

func (c flCollector) MaxInstancesSet(int)                       {}
func (c flCollector) InternalOperationInc(string)               {}
func (c flCollector) MissedUpdateInc()                          {}
func (c flCollector) InstanceCreationTimeObserve(time.Duration) {}
func (c flCollector) InstanceDeletionTimeObserve(time.Duration) {}
func (c flCollector) InstanceOperationInc(op string) {
	c.m.instanceOps.WithLabelValues(c.class, op).Inc()
}
func (c flCollector) InstancesCountSet(state string, v int) {
	c.m.instances.WithLabelValues(c.class, state).Set(float64(v))
}
func (c flCollector) InstanceIsRunningTimeObserve(d time.Duration) {
	c.m.running.WithLabelValues(c.class).Observe(d.Seconds())
}
func (c flCollector) InstanceLifeDurationObserve(d time.Duration) {
	c.m.lifetime.WithLabelValues(c.class).Observe(d.Seconds())
}

// upTimes keeps when each of a pool's instances was admitted, so a job's
// VM can be told warm (up before the job asked) from cold.
type upTimes struct {
	mu sync.Mutex
	at map[string]time.Time
}

func (u *upTimes) up(instance string, at time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.at == nil {
		u.at = map[string]time.Time{}
	}
	u.at[instance] = at
}

func (u *upTimes) forget(instance string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.at, instance)
}

// before reports whether instance was admitted before t.
func (u *upTimes) before(instance string, t time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	at, ok := u.at[instance]
	return ok && at.Before(t)
}

// Metrics are the backend's figures for Prometheus.
func (b *Backend) Metrics() prometheus.Collector { return backendCollector{b} }

// backendCollector adds the gauges read at each scrape to the backend's
// kept metrics.
type backendCollector struct{ b *Backend }

var (
	descJobs = prometheus.NewDesc("fleetci_class_jobs",
		"The class's jobs: waiting for a VM (for room, or for one to boot) or running on one.", []string{"class", "state"}, nil)
	descMax = prometheus.NewDesc("fleetci_class_max_instances",
		"The class's max_instances.", []string{"class"}, nil)
	descDraining = prometheus.NewDesc("fleetci_class_draining_vms",
		"The class's VMs with a reclaim notice: they take no new jobs and go once their jobs end.", []string{"class"}, nil)
	descDied = prometheus.NewDesc("fleetci_instances_lost_total",
		"The class's instances that went without fleetci removing them: VMs that died, cloud instances taken away.", []string{"class"}, nil)
	descBudgetCPU = prometheus.NewDesc("fleetci_host_budget_cpus",
		"The CPUs a host lends its classes' VMs ([host]).", []string{"host"}, nil)
	descBudgetMem = prometheus.NewDesc("fleetci_host_budget_memory_bytes",
		"The memory a host lends its classes' VMs ([host]).", []string{"host"}, nil)
	descUsedCPU = prometheus.NewDesc("fleetci_host_used_cpus",
		"The CPUs of a host's budget taken: its VMs' and the reserved classes' fixed shares.", []string{"host"}, nil)
	descUsedMem = prometheus.NewDesc("fleetci_host_used_memory_bytes",
		"The memory of a host's budget taken: its VMs' and the reserved classes' fixed shares.", []string{"host"}, nil)
	descCacheUsed = prometheus.NewDesc("fleetci_cache_used_bytes",
		"What the cache disks take.", nil, nil)
	descCacheBudget = prometheus.NewDesc("fleetci_cache_budget_bytes",
		"What the cache disks may take.", nil, nil)
	descCacheLow = prometheus.NewDesc("fleetci_cache_low_space",
		"1 when the cache disks' filesystem is under min_free: jobs get no cache disk.", nil, nil)
	descCacheEvicted = prometheus.NewDesc("fleetci_cache_evictions_total",
		"Caches dropped, over budget or too old.", nil, nil)
	descCacheFlattens = prometheus.NewDesc("fleetci_cache_flattens_total",
		"Cache chains flattened.", nil, nil)
)

func (c backendCollector) Describe(ch chan<- *prometheus.Desc) {
	if c.b.metrics != nil {
		for _, m := range c.b.metrics.collected {
			m.Describe(ch)
		}
	}
	for _, d := range []*prometheus.Desc{descJobs, descMax, descDraining, descDied, descBudgetCPU, descBudgetMem, descUsedCPU, descUsedMem,
		descCacheUsed, descCacheBudget, descCacheLow, descCacheEvicted, descCacheFlattens} {
		ch <- d
	}
}

func (c backendCollector) Collect(ch chan<- prometheus.Metric) {
	b := c.b
	if b.metrics != nil {
		for _, m := range b.metrics.collected {
			m.Collect(ch)
		}
	}
	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	counter := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v, labels...)
	}

	running := map[string]int{}
	b.mu.Lock()
	for _, p := range b.vms {
		running[p.name]++
	}
	b.mu.Unlock()
	var queue *hostQueue
	for _, name := range b.Classes() {
		var jobs, draining int
		var died int64
		max := 0
		for _, p := range b.pools[name] {
			p.mu.Lock()
			jobs += p.jobs + p.held
			p.mu.Unlock()
			draining += p.reclaim.draining()
			died += p.died.count()
			max += p.class.MaxInstances
			if p.queue != nil {
				queue = p.queue
			}
		}
		run := running[name]
		gauge(descJobs, float64(run), name, "running")
		gauge(descJobs, float64(max0(jobs-run)), name, "waiting")
		gauge(descMax, float64(max), name)
		gauge(descDraining, float64(draining), name)
		counter(descDied, float64(died), name)
	}
	if queue != nil {
		const mb = 1 << 20
		queue.mu.Lock()
		for _, h := range queue.hosts {
			gauge(descBudgetCPU, float64(h.CPUs), h.name)
			gauge(descBudgetMem, float64(h.MemoryMB*mb), h.name)
			gauge(descUsedCPU, float64(h.used.CPUs), h.name)
			gauge(descUsedMem, float64(h.used.MemoryMB*mb), h.name)
		}
		queue.mu.Unlock()
	}
	if cs := b.CacheStats(); cs != nil {
		low := 0.0
		if cs.LowSpace {
			low = 1
		}
		gauge(descCacheUsed, float64(cs.Used))
		gauge(descCacheBudget, float64(cs.Budget))
		gauge(descCacheLow, low)
		counter(descCacheEvicted, float64(cs.Evicted))
		counter(descCacheFlattens, float64(cs.Flattens))
	}
}

func max0(n int) int { return max(n, 0) }

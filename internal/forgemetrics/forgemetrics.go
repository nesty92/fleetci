// Package forgemetrics has the metrics fleetci keeps about the forges it
// takes jobs from: how long jobs waited, and GitHub's API calls. The fleet
// controller and fleetci serve (and its plugin mode) keep the same ones.
package forgemetrics

import (
	"cmp"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics are the forge metrics of one process.
type Metrics struct {
	queueWait   *prometheus.HistogramVec
	pickup      *prometheus.HistogramVec
	ghRequests  *prometheus.CounterVec
	ghSeconds   prometheus.Histogram
	ghRemaining *prometheus.GaugeVec
}

// New makes the metrics. The wait histograms are named prefix +
// "queue_wait_seconds" and prefix + "pickup_seconds"; the GitHub API ones
// are always fleetci_github_api_*.
func New(prefix string) *Metrics {
	return &Metrics{
		queueWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: prefix + "queue_wait_seconds", Help: "From the forge queuing a job to its VM being up.",
			Buckets: []float64{1, 2, 5, 10, 20, 30, 60, 120, 300, 600, 1800, 3600},
		}, []string{"forge", "class"}),
		pickup: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: prefix + "pickup_seconds", Help: "From the forge queuing a job to fleetci first taking it, by how it came (poll or webhook): fleetci's own part of the queue wait.",
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
}

// Collectors are the metrics, to register.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.queueWait, m.pickup, m.ghRequests, m.ghSeconds, m.ghRemaining}
}

// QueueWait is told how long each of the forge's jobs waited for its VM.
func (m *Metrics) QueueWait(forge string) func(class string, wait time.Duration) {
	return func(class string, wait time.Duration) {
		m.queueWait.WithLabelValues(forge, class).Observe(wait.Seconds())
	}
}

// Pickup is told how long each of the forge's jobs took to reach fleetci.
func (m *Metrics) Pickup(forge string) func(class, via string, wait time.Duration) {
	return func(class, via string, wait time.Duration) {
		m.pickup.WithLabelValues(forge, class, via).Observe(wait.Seconds())
	}
}

// GitHubTransport counts and times GitHub's API calls made through it,
// and keeps what the rate limit has left.
func (m *Metrics) GitHubTransport(next http.RoundTripper) http.RoundTripper {
	return githubTransport{m: m, next: cmp.Or[http.RoundTripper](next, http.DefaultTransport)}
}

type githubTransport struct {
	m    *Metrics
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

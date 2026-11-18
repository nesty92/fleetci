package fleetci

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/nesty92/fleetci/internal/usage"
)

func TestServeMetrics(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()

	rec := usage.NewRecorder("", hclog.NewNullLogger())
	rec.Record(usage.Run{Forge: "github", Job: "a/b build", Class: "small",
		Report: usage.Report{Seconds: 100, CPUs: 2, MemoryMB: 4096, MemPeakMB: 3900, CPUPeak: 1}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Config{MetricsListen: addr}
	reg, err := c.ServeMetrics(ctx, rec, hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	// What starts after the endpoint, the backend and the job sources,
	// joins it.
	later := prometheus.NewCounter(prometheus.CounterOpts{Name: "fleetci_later_total", Help: "x"})
	later.Inc()
	reg.MustRegister(later)
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, want := range []string{
		`fleetci_job_fit_total{class="small",fit="undersized",forge="github"} 1`,
		`fleetci_job_memory_peak_ratio_count{class="small",forge="github"} 1`,
		`go_goroutines`,
		`fleetci_later_total 1`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics has no %s", want)
		}
	}
	http.DefaultClient.CloseIdleConnections()

	reg, err = (&Config{}).ServeMetrics(ctx, rec, hclog.NewNullLogger())
	if err != nil || reg == nil {
		t.Fatalf("without metrics_listen: %v, %v", reg, err)
	}
}

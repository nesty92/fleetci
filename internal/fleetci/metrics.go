package fleetci

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/nesty92/fleetci/internal/usage"
)

// ServeMetrics serves the recorder's metrics, with the process's own, at
// metrics_listen until ctx ends, and returns the registry they are in, for
// the backend's and the job sources' metrics once they start. Without
// metrics_listen the registry is served nowhere.
func (c *Config) ServeMetrics(ctx context.Context, rec *usage.Recorder, log hclog.Logger) (*prometheus.Registry, error) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(rec.Metrics(), collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	if c.MetricsListen == "" {
		return reg, nil
	}
	lis, err := net.Listen("tcp", c.MetricsListen)
	if err != nil {
		return nil, fmt.Errorf("metrics_listen: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() {
		if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serving metrics", "addr", c.MetricsListen, "err", err)
		}
	}()
	log.Info("serving metrics", "addr", lis.Addr().String())
	return reg, nil
}

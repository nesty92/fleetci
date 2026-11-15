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
// metrics_listen until ctx ends. Without metrics_listen it does nothing.
func (c *Config) ServeMetrics(ctx context.Context, rec *usage.Recorder, log hclog.Logger) error {
	if c.MetricsListen == "" {
		return nil
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(rec.Metrics(), collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	lis, err := net.Listen("tcp", c.MetricsListen)
	if err != nil {
		return fmt.Errorf("metrics_listen: %w", err)
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
	return nil
}

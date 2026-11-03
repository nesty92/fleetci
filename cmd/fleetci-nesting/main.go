// Command fleetci-nesting serves the nesting API that gitlab-runner's
// vm_isolation calls once per job. It decides the job's machine class from
// the VM name and creates a fresh VM of that class.
//
// The socket path comes from NESTING_SOCKET.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"gitlab.com/gitlab-org/fleeting/nesting/api"

	"github.com/nesty92/fleetci/internal/dockervm"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fleetci-nesting:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "", "path to the daemon config (JSON)")
	passwordPath := flag.String("password-file", "", "file holding the VM SSH password")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	var cfg dockervm.Config
	raw, err := os.ReadFile(*configPath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parsing %s: %w", *configPath, err)
	}
	pw, err := os.ReadFile(*passwordPath)
	if err != nil {
		return err
	}
	cfg.Password = strings.TrimSpace(string(pw))

	hv, err := dockervm.New(cfg, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("serving nesting api", "socket", os.Getenv("NESTING_SOCKET"), "classes", len(cfg.Classes))
	return api.Serve(ctx, hv)
}

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/watchdog"
)

// watchdogCmd restarts gitlab-runner when its fleetci plugin is gone.
func watchdogCmd(args []string) error {
	fs := flag.NewFlagSet("watchdog", flag.ContinueOnError)
	plugin := fs.String("plugin", "fleeting-plugin-fleetci", "the fleetci plugin as gitlab-runner starts it: a path or a name")
	restart := fs.String("restart", "systemctl restart gitlab-runner", "shell command that restarts gitlab-runner")
	interval := fs.Duration("interval", 10*time.Second, "time between checks")
	grace := fs.Duration("grace", 30*time.Second, "how long the plugin may be missing before a restart")
	maxBackoff := fs.Duration("max-backoff", 10*time.Minute, "longest wait between restarts while the plugin stays gone")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("watchdog takes no arguments")
	}
	log := hclog.New(&hclog.LoggerOptions{Name: "fleetci-watchdog", Output: os.Stderr})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("watching", "plugin", *plugin, "grace", grace.String())
	return watchdog.Run(ctx, watchdog.Config{
		Plugin: *plugin,
		Restart: func(ctx context.Context) error {
			out, err := exec.CommandContext(ctx, "sh", "-c", *restart).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s: %w: %s", *restart, err, out)
			}
			return nil
		},
		Interval:   *interval,
		Grace:      *grace,
		MinBackoff: time.Minute,
		MaxBackoff: *maxBackoff,
	}, log)
}

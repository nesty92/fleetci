// Package watchdog restarts gitlab-runner when its fleetci plugin is gone.
//
// gitlab-runner starts fleetci as a fleeting plugin and never starts it
// again: if the plugin dies, every class on GitLab and GitHub stops until
// gitlab-runner restarts. The watchdog runs next to it and restarts it
// once the plugin has been missing for a grace period.
package watchdog

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"
)

type Config struct {
	// Plugin is the fleetci plugin's path, as gitlab-runner starts it.
	Plugin string
	// Restart restarts gitlab-runner.
	Restart func(context.Context) error
	// Interval between checks, Grace before a missing plugin counts as gone.
	Interval, Grace time.Duration
	// The first restart may come at once; each next one waits twice as long
	// as the last, up to MaxBackoff, so a plugin that can't start (a broken
	// config) doesn't restart gitlab-runner every Grace. The wait resets
	// once the plugin has run for MaxBackoff.
	MinBackoff, MaxBackoff time.Duration

	// alive and now are swapped in tests.
	alive func(string) bool
	now   func() time.Time
}

// Run checks until ctx ends.
func Run(ctx context.Context, cfg Config, log hclog.Logger) error {
	if cfg.alive == nil {
		cfg.alive = PluginAlive
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	w := &watch{cfg: cfg, log: log, backoff: cfg.MinBackoff}
	tick := time.NewTicker(cfg.Interval)
	defer tick.Stop()
	for {
		w.check(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

type watch struct {
	cfg       Config
	log       hclog.Logger
	missing   time.Time // since when the plugin is missing; zero while it runs
	upSince   time.Time
	nextAllow time.Time
	backoff   time.Duration
	restarts  int
}

func (w *watch) check(ctx context.Context) {
	now := w.cfg.now()
	if w.cfg.alive(w.cfg.Plugin) {
		if w.upSince.IsZero() {
			w.upSince = now
		}
		if now.Sub(w.upSince) >= w.cfg.MaxBackoff {
			w.backoff = w.cfg.MinBackoff
		}
		w.missing = time.Time{}
		return
	}
	w.upSince = time.Time{}
	if w.missing.IsZero() {
		w.missing = now
		w.log.Warn("fleetci plugin not running", "plugin", w.cfg.Plugin)
	}
	if now.Sub(w.missing) < w.cfg.Grace || now.Before(w.nextAllow) {
		return
	}
	w.restarts++
	w.log.Error("fleetci plugin gone; restarting gitlab-runner", "missing_for", now.Sub(w.missing).Round(time.Second).String(), "restarts", w.restarts)
	if err := w.cfg.Restart(ctx); err != nil {
		w.log.Error("restarting gitlab-runner failed", "err", err)
	}
	w.nextAllow = now.Add(w.backoff)
	w.backoff = min(w.backoff*2, w.cfg.MaxBackoff)
	w.missing = time.Time{} // the new gitlab-runner gets its grace too
}

// PluginAlive is whether a live (not zombie) process runs the plugin: a
// path, or a bare name as gitlab-runner's config may give it.
func PluginAlive(plugin string) bool {
	byName := !strings.Contains(plugin, "/")
	want := filepath.Clean(plugin)
	procs, _ := filepath.Glob("/proc/[0-9]*")
	for _, p := range procs {
		cmd, err := os.ReadFile(filepath.Join(p, "cmdline"))
		if err != nil || len(cmd) == 0 {
			continue // zombies have an empty cmdline
		}
		argv0, _, _ := bytes.Cut(cmd, []byte{0})
		arg := filepath.Clean(string(argv0))
		if arg != want && !(byName && filepath.Base(arg) == want) {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(p, "stat"))
		if err != nil {
			continue
		}
		if end := bytes.LastIndexByte(stat, ')'); end > 0 && end+2 < len(stat) && stat[end+2] != 'Z' {
			return true
		}
	}
	return false
}

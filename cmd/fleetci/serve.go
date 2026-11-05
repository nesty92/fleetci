package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/fleet"
	"github.com/nesty92/fleetci/internal/fleetci"
	"github.com/nesty92/fleetci/internal/github"
)

// serve runs GitHub Actions jobs on fleetci.toml's classes as a process of
// its own, without gitlab-runner, until SIGINT or SIGTERM; then it removes
// its VMs. Run it under a service manager that restarts it.
//
// Its classes are its own: a group driven by two processes (serve and a
// gitlab-runner's fleetci plugin) has each remove the other's VMs, so
// GitLab jobs need a fleetci.toml, and classes, of their own.
func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := configFlag(fs)
	_ = fs.Parse(args)

	c, err := fleetci.Load(*path)
	if err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if c.GitHub == nil {
		return errors.New(c.ConfigFile + ": serve runs GitHub Actions jobs and needs a [github] table")
	}

	log := hclog.New(&hclog.LoggerOptions{Name: "fleetci", Output: os.Stderr, Level: hclog.Info})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	backend, err := fleet.New(startCtx, c.Classes, c.Host, log)
	cancel()
	if err != nil {
		return err
	}
	defer func() {
		log.Info("stopping: removing this process's VMs")
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		_ = backend.Shutdown(sctx)
	}()

	gh, err := github.New(*c.GitHub, c.DefaultClass, backend, nil, log.Named("github"))
	if err != nil {
		return err
	}
	log.Info("fleetci serving", "classes", len(c.Classes), "default_class", c.DefaultClass)
	gh.Run(ctx)
	return nil
}

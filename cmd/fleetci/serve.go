package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/fleet"
	"github.com/nesty92/fleetci/internal/fleetci"
	"github.com/nesty92/fleetci/internal/github"
	"github.com/nesty92/fleetci/internal/gitlab"
	"github.com/nesty92/fleetci/internal/source"
)

// serve runs GitHub Actions jobs, and with [gitlab] GitLab CI jobs, on
// fleetci.toml's classes as a process of its own, without gitlab-runner,
// until SIGINT or SIGTERM; then it removes its VMs. Run it under a service
// manager that restarts it.
//
// Its classes are its own: a group driven by two processes (serve and a
// gitlab-runner's fleetci plugin) has each remove the other's VMs, so
// GitLab jobs need a fleetci.toml, and classes, of their own.
func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := configFlag(fs)
	removeAll := fs.Bool("remove-all", false, "on stopping, remove every VM, those running jobs too, even with state_dir")
	_ = fs.Parse(args)

	c, err := fleetci.Load(*path)
	if err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if c.GitHub == nil && c.GitLab == nil {
		return errors.New(c.ConfigFile + ": serve runs GitHub Actions and GitLab CI jobs and needs a [github] or [gitlab] table")
	}

	log := hclog.New(&hclog.LoggerOptions{Name: "fleetci", Output: os.Stderr, Level: hclog.Info})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var opts []fleet.Option
	if c.StateDir != "" {
		opts = append(opts, fleet.WithStateDir(c.StateDir))
	}
	startCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	backend, err := fleet.New(startCtx, c.Classes, c.Host, log, opts...)
	cancel()
	if err != nil {
		return err
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if c.StateDir == "" || *removeAll {
			log.Info("stopping: removing this process's VMs")
			_ = backend.Shutdown(sctx)
			return
		}
		log.Info("stopping: removing idle VMs; VMs running GitHub jobs are left to the next fleetci", "state_dir", c.StateDir)
		_ = backend.Stop(sctx)
	}()

	// The job sources, each over the same capacity.
	capacity := source.NewCapacity(backend, c.Classes, c.DefaultClass)
	type named struct {
		name  string
		src   source.Source
		table any
	}
	var sources []named
	if c.GitHub != nil {
		sources = append(sources, named{"github", &github.Source{}, c.GitHub})
	}
	if c.GitLab != nil {
		sources = append(sources, named{"gitlab", &gitlab.Source{}, c.GitLab})
	}
	for _, s := range sources {
		config, err := source.Configure(s.table)
		if err != nil {
			return err
		}
		if err := s.src.Init(ctx, config, capacity, log.Named(s.name)); err != nil {
			return err
		}
	}
	log.Info("fleetci serving", "classes", len(c.Classes), "default_class", c.DefaultClass, "github", c.GitHub != nil, "gitlab", c.GitLab != nil)
	errs := make([]error, len(sources))
	var wg sync.WaitGroup
	for i, s := range sources {
		wg.Go(func() { errs[i] = s.src.Run(ctx) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

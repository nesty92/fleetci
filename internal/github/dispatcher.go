// Package github runs GitHub Actions jobs on fleetci's classes. A job asks
// for a class in runs-on:
//
//	runs-on: fleetci-4vcpu-8gb   # class 4vcpu-8gb
//	runs-on: fleetci             # the default class
//
// For each queued job with such a label, fleetci takes a VM of that class,
// registers a just-in-time runner with the job's labels and starts it on the
// VM over SSH. The runner takes one job and exits, and the VM is removed. The
// VMs come from the same pools as gitlab-runner's jobs.
package github

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/fleet"
)

// Config is plugin_config's [github] table.
type Config struct {
	// Repos are the repositories to serve, as "owner/name".
	Repos []string `json:"repos"`
	// TokenFile holds a token that may register runners on Repos: a
	// fine-grained token with "Administration: write", or a classic one
	// with "repo".
	TokenFile string `json:"token_file"`
	// LabelPrefix starts the runs-on labels fleetci serves. Default
	// "fleetci".
	LabelPrefix string `json:"label_prefix"`
	// PollInterval is how often queued jobs are listed. Default 10s.
	PollInterval fleet.Duration `json:"poll_interval"`
	// IdleTimeout removes a runner that has not started a job by then, such
	// as one whose job was cancelled while its VM booted. Default 10m.
	IdleTimeout fleet.Duration `json:"idle_timeout"`
	// RunnerVersion is the actions/runner release to run. Default
	// DefaultRunnerVersion.
	RunnerVersion string `json:"runner_version"`
	// RunnerGroupID is the runner group to register in. Default 1, the
	// default group.
	RunnerGroupID int64 `json:"runner_group_id"`
	// APIURL is GitHub's REST API. Default https://api.github.com.
	APIURL string `json:"api_url"`
}

// DefaultRunnerVersion is the actions/runner release used when the config
// names none.
const DefaultRunnerVersion = "2.338.2"

// genericLabels may sit next to a fleetci label in runs-on; every fleetci VM
// has them.
var genericLabels = []string{"self-hosted", "linux"}

// Backend hands out one VM per job.
type Backend interface {
	Has(class string) bool
	Create(ctx context.Context, class string) (fleet.VM, error)
	Delete(ctx context.Context, id string) error
}

// StartFunc runs a just-in-time runner on the VM and returns when it exits.
type StartFunc func(ctx context.Context, vm fleet.VM, jitConfig string) error

type Dispatcher struct {
	cfg          Config
	defaultClass string
	backend      Backend
	start        StartFunc
	gh           *client
	log          hclog.Logger

	mu     sync.Mutex
	taken  map[int64]time.Time // job id → when a runner was started for it
	warned map[int64]bool      // jobs already logged as unservable
	wg     sync.WaitGroup
}

// New checks the config and reads the token. Runners are started with
// SSHStart unless start is set.
func New(cfg Config, defaultClass string, backend Backend, start StartFunc, log hclog.Logger) (*Dispatcher, error) {
	if len(cfg.Repos) == 0 {
		return nil, errors.New("github: repos is required")
	}
	for _, r := range cfg.Repos {
		if owner, name, ok := strings.Cut(r, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("github: repo %q must be owner/name", r)
		}
	}
	if cfg.TokenFile == "" {
		return nil, errors.New("github: token_file is required")
	}
	raw, err := os.ReadFile(cfg.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("github: reading token_file: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil, errors.New("github: token_file is empty")
	}
	if cfg.LabelPrefix == "" {
		cfg.LabelPrefix = "fleetci"
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = fleet.Duration(10 * time.Second)
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = fleet.Duration(10 * time.Minute)
	}
	if cfg.RunnerVersion == "" {
		cfg.RunnerVersion = DefaultRunnerVersion
	}
	if cfg.RunnerGroupID == 0 {
		cfg.RunnerGroupID = 1
	}
	if cfg.APIURL == "" {
		cfg.APIURL = "https://api.github.com"
	}
	if start == nil {
		start = SSHStart(cfg.RunnerVersion, log)
	}
	return &Dispatcher{
		cfg:          cfg,
		defaultClass: defaultClass,
		backend:      backend,
		start:        start,
		gh:           newClient(cfg.APIURL, token),
		log:          log,
		taken:        map[int64]time.Time{},
		warned:       map[int64]bool{},
	}, nil
}

// Run polls for queued jobs until ctx ends, then waits for its runners to
// stop and their VMs to be released.
func (d *Dispatcher) Run(ctx context.Context) {
	d.log.Info("serving github jobs", "repos", d.cfg.Repos, "label_prefix", d.cfg.LabelPrefix)
	t := time.NewTicker(time.Duration(d.cfg.PollInterval))
	defer t.Stop()
	for {
		for _, repo := range d.cfg.Repos {
			d.poll(ctx, repo)
		}
		select {
		case <-ctx.Done():
			d.wg.Wait()
			return
		case <-t.C:
		}
	}
}

func (d *Dispatcher) poll(ctx context.Context, repo string) {
	jobs, err := d.gh.queuedJobs(ctx, repo)
	if err != nil {
		if ctx.Err() == nil {
			d.log.Warn("listing queued jobs", "repo", repo, "err", err)
		}
		return
	}

	queued := map[int64]bool{}
	for _, j := range jobs {
		queued[j.ID] = true
		class, ok := d.classFor(repo, j)
		if !ok || !d.take(j.ID) {
			continue
		}
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.serve(ctx, repo, j, class)
		}()
	}
	d.prune(queued)
}

// classFor picks the class a job asks for. Jobs without a fleetci label
// belong to other runners. Labels fleetci can't serve are logged once.
func (d *Dispatcher) classFor(repo string, j job) (string, bool) {
	class, found := "", false
	for _, l := range j.Labels {
		l = strings.ToLower(l)
		switch {
		case l == d.cfg.LabelPrefix:
			class, found = d.defaultClass, true
		case strings.HasPrefix(l, d.cfg.LabelPrefix+"-"):
			class, found = strings.TrimPrefix(l, d.cfg.LabelPrefix+"-"), true
		}
	}
	if !found {
		return "", false
	}

	var problem string
	if !d.backend.Has(class) {
		problem = fmt.Sprintf("no class %q", class)
	}
	for _, l := range j.Labels {
		l = strings.ToLower(l)
		if l != d.cfg.LabelPrefix && !strings.HasPrefix(l, d.cfg.LabelPrefix+"-") && !slices.Contains(genericLabels, l) {
			problem = fmt.Sprintf("label %q is not served by fleetci", l)
		}
	}
	if problem != "" {
		d.mu.Lock()
		if !d.warned[j.ID] {
			d.warned[j.ID] = true
			d.log.Warn("job left queued: "+problem, "repo", repo, "job", j.Name, "job_id", j.ID, "labels", j.Labels)
		}
		d.mu.Unlock()
		return "", false
	}
	return class, true
}

func (d *Dispatcher) take(id int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.taken[id]; ok {
		return false
	}
	d.taken[id] = time.Now()
	return true
}

func (d *Dispatcher) forget(id int64) {
	d.mu.Lock()
	delete(d.taken, id)
	d.mu.Unlock()
}

// prune drops jobs that are no longer queued after an hour, so the map stays
// small but a job is never served twice while it waits.
func (d *Dispatcher) prune(queued map[int64]bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, at := range d.taken {
		if !queued[id] && time.Since(at) > time.Hour {
			delete(d.taken, id)
		}
	}
	for id := range d.warned {
		if !queued[id] {
			delete(d.warned, id)
		}
	}
}

// serve boots one runner for a queued job. GitHub may hand that runner any
// queued job with the same labels; the count is what matters.
func (d *Dispatcher) serve(ctx context.Context, repo string, j job, class string) {
	log := d.log.With("repo", repo, "job", j.Name, "job_id", j.ID, "class", class)
	start := time.Now()

	vm, err := d.backend.Create(ctx, class)
	if err != nil {
		if ctx.Err() == nil {
			log.Error("no vm for job; retrying on the next poll", "err", err)
		}
		d.forget(j.ID)
		return
	}
	defer func() {
		_ = d.backend.Delete(context.WithoutCancel(ctx), vm.ID)
	}()
	log = log.With("vm", vm.ID, "instance", vm.Instance)

	labels := runnerLabels(j.Labels)
	runnerID, jit, err := d.gh.jitConfig(ctx, repo, vm.ID, d.cfg.RunnerGroupID, labels)
	if err != nil {
		log.Error("registering runner; retrying on the next poll", "err", err)
		d.forget(j.ID)
		return
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := d.gh.deleteRunner(dctx, repo, runnerID); err != nil {
			log.Warn("removing runner", "runner_id", runnerID, "err", err)
		}
	}()
	log.Info("runner starting", "runner_id", runnerID, "labels", labels, "waited", time.Since(start).Round(time.Second).String())

	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go d.watchIdle(rctx, cancel, repo, runnerID, log)

	if err := d.start(rctx, vm, jit); err != nil && rctx.Err() == nil {
		log.Error("runner failed", "err", err)
		return
	}
	log.Info("runner done", "took", time.Since(start).Round(time.Second).String())
}

// watchIdle stops a runner that has not picked up a job within IdleTimeout.
func (d *Dispatcher) watchIdle(ctx context.Context, stop context.CancelFunc, repo string, id int64, log hclog.Logger) {
	deadline := time.Now().Add(time.Duration(d.cfg.IdleTimeout))
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		r, err := d.gh.runner(ctx, repo, id)
		switch {
		case errors.Is(err, errNotFound):
			return // the job finished and GitHub removed the runner
		case err != nil:
			continue
		case r.Busy:
			return // a job runs; its own timeout applies from here
		case time.Now().After(deadline):
			log.Warn("runner got no job; stopping it", "idle_timeout", time.Duration(d.cfg.IdleTimeout).String())
			stop()
			return
		}
	}
}

// runnerLabels registers the job's own labels, so the runner can take it.
func runnerLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, strings.ToLower(l))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

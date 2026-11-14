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
	"cmp"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/cachebucket"
	"github.com/nesty92/fleetci/internal/cachedisk"
	"github.com/nesty92/fleetci/internal/fleet"
	"github.com/nesty92/fleetci/internal/proxy"
	"github.com/nesty92/fleetci/internal/source"
)

// Config is plugin_config's [github] table.
type Config struct {
	// Repos are the repositories to serve, as "owner/name". Their runners
	// are registered on the repo.
	Repos []string `json:"repos" toml:"repos"`
	// Orgs are the organizations whose every repo is served, by runners
	// registered on the org in RunnerGroupID. A repo of one of them is
	// served this way even when Repos lists it too.
	Orgs []string `json:"orgs" toml:"orgs"`
	// TokenFile holds a token that may register runners: for Repos a
	// fine-grained token with "Administration: write" (a classic one with
	// "repo"), for Orgs "Self-hosted runners: write" on the org (classic:
	// "admin:org"). Or authenticate as a GitHub App instead.
	TokenFile string `json:"token_file" toml:"token_file"`
	// CacheDisk says which jobs write the caches the rest of their repo
	// reads, for classes with a cache disk (see cachedisk.Policy); repos
	// match by "owner/name". Default: jobs on protected branches.
	CacheDisk *cachedisk.Policy `json:"cache_disk" toml:"cache_disk"`
	// AppID and AppPrivateKeyFile authenticate as a GitHub App, in place of
	// TokenFile. The App is found installed on each owner, and its
	// installation tokens are renewed as they expire.
	AppID             int64  `json:"app_id" toml:"app_id"`
	AppPrivateKeyFile string `json:"app_private_key_file" toml:"app_private_key_file"`
	// Webhook, when set, takes workflow_job events, so a job gets a VM as
	// soon as it is queued. Polling goes on, by default every 5m, for the
	// events that never arrive.
	Webhook *WebhookConfig `json:"webhook" toml:"webhook"`
	// LabelPrefix starts the runs-on labels fleetci serves. Default
	// "fleetci".
	LabelPrefix string `json:"label_prefix" toml:"label_prefix"`
	// PollInterval is how often queued jobs are listed. Default 10s, or 5m
	// with Webhook.
	PollInterval fleet.Duration `json:"poll_interval" toml:"poll_interval"`
	// IdleTimeout removes a runner that has not started a job by then, such
	// as one whose job was cancelled while its VM booted. Default 10m.
	IdleTimeout fleet.Duration `json:"idle_timeout" toml:"idle_timeout"`
	// RunnerVersion is the actions/runner release to run. Default the
	// newest in the VM's image, or DefaultRunnerVersion if it has none.
	RunnerVersion string `json:"runner_version" toml:"runner_version"`
	// RunnerGroupID is the runner group to register in. Default 1, the
	// default group.
	RunnerGroupID int64 `json:"runner_group_id" toml:"runner_group_id"`
	// RunnerScope is where an org's runners are registered: "org", the
	// default, or "repo", on the job's repo. GitHub gives a repo's runner
	// only that repo's jobs, so a runner fleetci starts for a job takes
	// that job or another of its repo, whose cache disk is more often on
	// the VM's node. With "repo" the token needs "Administration: write"
	// on the org's repos, and RunnerGroupID does not apply: groups are an
	// org's.
	RunnerScope string `json:"runner_scope" toml:"runner_scope"`
	// APIURL is GitHub's REST API. Default https://api.github.com.
	APIURL string `json:"api_url" toml:"api_url"`
	// CacheURL sends actions/cache, the setup-* caches and buildx's
	// type=gha to a github-actions-cache-server (falcondev-oss) at this
	// URL instead of GitHub, for example one backed by S3 in the VMs'
	// region. The runner is patched on the VM to accept it; other
	// requests (artifacts) the server passes on to GitHub.
	CacheURL string `json:"cache_url" toml:"cache_url"`
	// CacheStore is where actions/cache keeps its caches, besides the
	// VM's cache disk, which is in front of it: "github", the default,
	// is GitHub's cache (or CacheURL's server); "bucket" is the bucket
	// in CacheBucket; "off" is the cache disk alone. Jobs' workflows
	// stay as they are.
	CacheStore string `json:"cache_store" toml:"cache_store"`
	// CacheBucket is the bucket for CacheStore "bucket", in the form of
	// gitlab-runner's [runners.cache]: Type "s3", "gcs" or "azure", Path,
	// and its [s3], [gcs] or [azure] table. The node lists it and signs
	// links for its VMs; the credentials stay on the node.
	CacheBucket *cachebucket.Config `json:"cache_bucket,omitempty" toml:"cache_bucket,omitempty"`
	// CacheTokenIssuer issues the jobs' tokens, whose keys the node
	// checks them with. Default https://token.actions.githubusercontent.com;
	// GitHub Enterprise Server's is https://HOST/_services/token.
	CacheTokenIssuer string `json:"cache_token_issuer" toml:"cache_token_issuer"`

	// NameByJob, set by fleetci's controller and never by fleetci.toml,
	// names each runner after the job it is started for and the try,
	// fleetci-<class>-j<job id>-<try>, in place of a random name. GitHub
	// refuses a name it has with 409 Conflict, so two controllers that
	// both lead for a while can't start two runners for one job: the one
	// refused leaves the job for ClaimTimeout.
	NameByJob bool `json:"-" toml:"-"`

	// HandOverIdle, set by fleetci's controller, hands a runner that has
	// taken no job yet to its VM's keeper when the dispatcher stops, as one
	// that has: a fleet worker follows it, and stops it when it stays
	// idle. The next leader leaves the job to it, by its name.
	HandOverIdle bool `json:"-" toml:"-"`

	// Transport, set by fleetci's controller, carries the API calls, which
	// its metrics count.
	Transport http.RoundTripper `json:"-" toml:"-"`
	// QueueWait, set by fleetci's controller, is told how long each job
	// waited, from GitHub queuing it to its VM being up.
	QueueWait func(class string, wait time.Duration) `json:"-" toml:"-"`
	// Pickup, set by fleetci's controller, is told how long each job took
	// to reach fleetci, from GitHub queuing it to fleetci first taking it,
	// and how it came: "poll" or "webhook". It is fleetci's own part of a
	// job's wait; the rest is waiting for room.
	Pickup func(class, via string, wait time.Duration) `json:"-" toml:"-"`
}

// claimTimeout is how long a job whose runner's name GitHub already has,
// registered by another controller, is left to that runner before a runner
// is started for it under the next try.
var claimTimeout = 3 * time.Minute

// WebhookConfig is the [github.webhook] table: where GitHub delivers
// workflow_job events.
type WebhookConfig struct {
	// Listen is the address to listen on, such as ":9480".
	Listen string `json:"listen" toml:"listen"`
	// Path is the URL path deliveries arrive at. Default "/".
	Path string `json:"path" toml:"path"`
	// SecretFile holds the webhook's secret, which signs each delivery.
	SecretFile string `json:"secret_file" toml:"secret_file"`
	// TLSCertFile and TLSKeyFile serve HTTPS; without them it is HTTP,
	// for behind a proxy that terminates TLS.
	TLSCertFile string `json:"tls_cert_file" toml:"tls_cert_file"`
	TLSKeyFile  string `json:"tls_key_file" toml:"tls_key_file"`
}

// DefaultRunnerVersion is the actions/runner release used when the config
// names none and the image has none.
const DefaultRunnerVersion = "2.338.2"

// genericLabels may sit next to a fleetci label in runs-on; every fleetci VM
// has them.
var genericLabels = []string{"self-hosted", "linux"}

// archLabels are GitHub's labels for a runner's architecture, by GOARCH,
// served by a class of that architecture.
var archLabels = map[string]string{"x64": "amd64", "arm64": "arm64", "arm": "arm"}

// VM is a job's VM and how to reach it.
type VM struct {
	source.VM
	// Conn is the connection to the VM's machine, shared: never closed by
	// a job.
	Conn *proxy.Conn
	// Cache says the VM's class has a cache disk: the runner's job waits
	// at its start while fleetci attaches it (see jobStartedHook).
	Cache bool
}

// StartFunc runs a just-in-time runner on the VM and returns when it exits.
// It calls took with the name of the job GitHub gives the runner: any
// queued job with matching labels, not always the one it was started for.
type StartFunc func(ctx context.Context, vm VM, jitConfig string, took func(job string)) error

// Source is the GitHub job source: Init builds the dispatcher, Run runs it.
type Source struct{ d *Dispatcher }

var _ source.Source = (*Source)(nil)

// Init reads the [github] table, as JSON, and builds the dispatcher over
// capacity.
func (s *Source) Init(ctx context.Context, config []byte, capacity source.Capacity, log hclog.Logger) error {
	var c Config
	if err := json.Unmarshal(config, &c); err != nil {
		return fmt.Errorf("github: config: %w", err)
	}
	d, err := New(ctx, c, capacity, nil, log)
	if err != nil {
		return err
	}
	s.d = d
	return nil
}

// Run serves jobs until ctx ends.
func (s *Source) Run(ctx context.Context) error {
	s.d.Run(ctx)
	return nil
}

type Dispatcher struct {
	cfg          Config
	defaultClass string
	classes      []string
	arches       map[string]string // class → GOARCH, where known
	capacity     source.Capacity
	start        StartFunc
	gh           *client
	log          hclog.Logger
	secret       []byte          // the webhook's; nil without one
	orgs         map[string]bool // lower case
	repos        map[string]bool // lower case
	listening    chan string     // the webhook's address, once it listens

	mu      sync.Mutex
	taken   map[int64]*attempt   // jobs a runner was started for
	warned  map[int64]bool       // jobs already logged as unservable
	watched map[int64]string     // jobs a webhook said are queued → repo
	active  map[string]time.Time // repos a webhook spoke of → when last
	orgList map[string][]string  // org → its repos, as last listed
	listed  time.Time            // when orgList was
	runCtx  context.Context      // Run's, for the runners webhooks start
	closing bool                 // Run is waiting for its runners: start no more
	wg      sync.WaitGroup

	orphans []orphan // runners to remove once GitHub lets us

	// What GitHub says of repos, runs and refs, for cache disks: jobs of
	// one run, or one repo, ask the same.
	repoInfos memo[repoInfo]
	runs      memo[workflowRun]
	refs      memo[bool]

	// settle is how long a runner whose registration went unanswered is
	// looked for, in case GitHub registers it late.
	settle time.Duration
}

// orphan is a runner left registered: one whose removal failed (by id), or
// one GitHub may have registered without the answer reaching fleetci (by
// name, the VM's id).
type orphan struct {
	scope string // "/repos/owner/name" or "/orgs/name"
	id    int64
	name  string
	at    time.Time
}

// orphanTTL is how long an orphan is tried before it is left to GitHub,
// which drops runners that stay offline.
const orphanTTL = 24 * time.Hour

// New checks the config and reads the token. Runners are started with
// SSHStart unless start is set.
func New(ctx context.Context, cfg Config, capacity source.Capacity, start StartFunc, log hclog.Logger) (*Dispatcher, error) {
	classes, defaultClass, err := capacity.Classes(ctx)
	if err != nil {
		return nil, fmt.Errorf("github: classes: %w", err)
	}
	if cfg.APIURL == "" {
		cfg.APIURL = "https://api.github.com"
	}
	hc := &http.Client{Timeout: 30 * time.Second, Transport: cfg.Transport}
	tokens, err := cfg.auth(hc)
	if err != nil {
		return nil, err
	}
	var secret []byte
	if w := cfg.Webhook; w != nil {
		s, err := w.check()
		if err != nil {
			return nil, err
		}
		secret = []byte(s)
	}
	if cfg.LabelPrefix == "" {
		cfg.LabelPrefix = "fleetci"
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = fleet.Duration(10 * time.Second)
		if cfg.Webhook != nil {
			cfg.PollInterval = fleet.Duration(5 * time.Minute)
		}
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = fleet.Duration(10 * time.Minute)
	}
	if strings.ContainsAny(cfg.RunnerVersion, "'\"\\ $`/") {
		return nil, fmt.Errorf("github runner_version %q: want a release such as %s", cfg.RunnerVersion, DefaultRunnerVersion)
	}
	if err := cfg.CacheDisk.Validate(); err != nil {
		return nil, fmt.Errorf("github: cache_disk %w", err)
	}
	switch cfg.RunnerScope {
	case "", "org":
	case "repo":
		if cfg.RunnerGroupID > 1 {
			return nil, errors.New(`github: runner_group_id with runner_scope = "repo": runner groups are an org's, and a repo's runners are in none`)
		}
	default:
		return nil, fmt.Errorf(`github runner_scope %q: want "org" or "repo"`, cfg.RunnerScope)
	}
	if cfg.RunnerGroupID == 0 {
		cfg.RunnerGroupID = 1
	}
	cs, err := NewCacheStore(cfg.CacheStore, cfg.CacheURL, cfg.CacheBucket, cfg.CacheTokenIssuer, log)
	if err != nil {
		return nil, err
	}
	cfg.CacheURL = cs.URL
	if start == nil {
		start = SSHStart(cfg.RunnerVersion, cs, log)
	}
	d := &Dispatcher{
		cfg:          cfg,
		defaultClass: defaultClass,
		classes:      source.Names(classes),
		arches:       arches(classes),
		capacity:     capacity,
		start:        start,
		gh:           newClient(cfg.APIURL, tokens, hc),
		log:          log,
		secret:       secret,
		orgs:         map[string]bool{},
		repos:        map[string]bool{},
		listening:    make(chan string, 1),
		taken:        map[int64]*attempt{},
		warned:       map[int64]bool{},
		watched:      map[int64]string{},
		active:       map[string]time.Time{},
		settle:       time.Minute,
	}
	for _, o := range cfg.Orgs {
		d.orgs[strings.ToLower(o)] = true
	}
	for _, r := range cfg.Repos {
		d.repos[strings.ToLower(r)] = true
	}
	return d, nil
}

// auth checks what to serve and how to sign in to GitHub.
func (cfg Config) auth(hc *http.Client) (tokens, error) {
	if len(cfg.Repos) == 0 && len(cfg.Orgs) == 0 {
		return nil, errors.New("github: repos or orgs is required")
	}
	for _, r := range cfg.Repos {
		if owner, name, ok := strings.Cut(r, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("github: repo %q must be owner/name", r)
		}
	}
	for _, o := range cfg.Orgs {
		if o == "" || strings.Contains(o, "/") {
			return nil, fmt.Errorf("github: org %q must be an org's name", o)
		}
	}
	app := cfg.AppID != 0 || cfg.AppPrivateKeyFile != ""
	switch {
	case app && cfg.TokenFile != "":
		return nil, errors.New("github: token_file or app_id, not both")
	case app && (cfg.AppID == 0 || cfg.AppPrivateKeyFile == ""):
		return nil, errors.New("github: app_id and app_private_key_file go together")
	case app:
		key, err := readSecret("app_private_key_file", cfg.AppPrivateKeyFile)
		if err != nil {
			return nil, err
		}
		t, err := newAppTokens(strings.TrimRight(cmp.Or(cfg.APIURL, "https://api.github.com"), "/"), cfg.AppID, []byte(key), hc)
		if err != nil {
			return nil, fmt.Errorf("github: app_private_key_file: %w", err)
		}
		return t, nil
	}
	if cfg.TokenFile == "" {
		return nil, errors.New("github: token_file, or app_id and app_private_key_file, is required")
	}
	tok, err := readSecret("token_file", cfg.TokenFile)
	if err != nil {
		return nil, err
	}
	return staticToken(tok), nil
}

// check reads the secret and checks the rest of the table.
func (w WebhookConfig) check() (string, error) {
	if w.Listen == "" {
		return "", errors.New("github: webhook.listen is required")
	}
	if w.Path != "" && !strings.HasPrefix(w.Path, "/") {
		return "", fmt.Errorf("github: webhook.path %q must start with /", w.Path)
	}
	if (w.TLSCertFile == "") != (w.TLSKeyFile == "") {
		return "", errors.New("github: webhook.tls_cert_file and tls_key_file go together")
	}
	return readSecret("webhook.secret_file", w.SecretFile)
}

// scope is where a repo's runners are registered: on its org when fleetci
// serves the org, otherwise on the repo.
func (d *Dispatcher) scope(repo string) string {
	owner, _, _ := strings.Cut(repo, "/")
	if d.orgs[strings.ToLower(owner)] && d.cfg.RunnerScope != "repo" {
		return "/orgs/" + owner
	}
	return "/repos/" + repo
}

// serves says whether fleetci serves a repo's jobs.
func (d *Dispatcher) serves(repo string) bool {
	owner, _, _ := strings.Cut(repo, "/")
	return d.repos[strings.ToLower(repo)] || d.orgs[strings.ToLower(owner)]
}

// Finding is one line of Check's report.
type Finding struct {
	Name string // "repo owner/name", "org name", "webhook" or "config"
	OK   string // what passed, when Err is nil
	Err  error
}

// Check proves the token can do what fleetci needs: on each repo list
// queued jobs (Actions: read) and manage runners (Administration), on each
// org list its repos and manage its runners (Self-hosted runners). It
// registers nothing; whether the token may also write is only seen when
// the first runner registers. With a GitHub App, minting each owner's
// installation token is part of it.
func Check(ctx context.Context, cfg Config) []Finding {
	hc := &http.Client{Timeout: 30 * time.Second}
	tokens, err := cfg.auth(hc)
	if err != nil {
		return []Finding{{Name: "config", Err: err}}
	}
	gh := newClient(cmp.Or(cfg.APIURL, "https://api.github.com"), tokens, hc)
	var out []Finding
	for _, repo := range cfg.Repos {
		var errs []error
		if err := gh.do(ctx, http.MethodGet, "/repos/"+repo+"/actions/runs?per_page=1", nil, nil); err != nil {
			errs = append(errs, fmt.Errorf("listing jobs (needs Actions: read): %w", err))
		}
		if err := gh.do(ctx, http.MethodGet, "/repos/"+repo+"/actions/runners?per_page=1", nil, nil); err != nil {
			errs = append(errs, fmt.Errorf("listing runners (needs Administration: read and write): %w", err))
		}
		out = append(out, Finding{"repo " + repo, "can list jobs and runners", errors.Join(errs...)})
	}
	for _, org := range cfg.Orgs {
		var errs []error
		if err := gh.do(ctx, http.MethodGet, "/orgs/"+org+"/repos?per_page=1", nil, nil); err != nil {
			errs = append(errs, fmt.Errorf("listing repos: %w", err))
		}
		if cfg.RunnerScope == "repo" {
			// The runners go on the repos: try one of them.
			var repos []struct {
				FullName string `json:"full_name"`
			}
			if err := gh.do(ctx, http.MethodGet, "/orgs/"+org+"/repos?per_page=1", nil, &repos); err == nil && len(repos) > 0 {
				if err := gh.do(ctx, http.MethodGet, "/repos/"+repos[0].FullName+"/actions/runners?per_page=1", nil, nil); err != nil {
					errs = append(errs, fmt.Errorf("listing %s's runners (runner_scope = \"repo\" needs Administration: read and write on the org's repos): %w", repos[0].FullName, err))
				}
			}
		} else if err := gh.do(ctx, http.MethodGet, "/orgs/"+org+"/actions/runners?per_page=1", nil, nil); err != nil {
			errs = append(errs, fmt.Errorf("listing runners (needs Self-hosted runners: read and write): %w", err))
		}
		out = append(out, Finding{"org " + org, "can list repos and runners", errors.Join(errs...)})
	}
	if w := cfg.Webhook; w != nil {
		_, err := w.check()
		if err == nil && w.TLSCertFile != "" {
			_, err = tls.LoadX509KeyPair(w.TLSCertFile, w.TLSKeyFile)
		}
		out = append(out, Finding{"webhook", "listens on " + w.Listen + cmp.Or(w.Path, "/") + " with a secret", err})
	}
	return out
}

// Run serves jobs until ctx ends: it polls, and with a webhook takes
// deliveries. Then it waits for its runners to stop and their VMs to be
// released.
func (d *Dispatcher) Run(ctx context.Context) {
	d.log.Info("serving github jobs", "repos", d.cfg.Repos, "orgs", d.cfg.Orgs, "label_prefix", d.cfg.LabelPrefix,
		"webhook", d.cfg.Webhook != nil, "poll_interval", time.Duration(d.cfg.PollInterval).String())
	d.mu.Lock()
	d.runCtx = ctx
	d.mu.Unlock()
	var srv *http.Server
	if d.cfg.Webhook != nil {
		var err error
		if srv, err = d.listen(ctx); err != nil {
			// Polling still serves every job, only later.
			d.log.Error("webhook not listening; polling only", "err", err)
		}
	}
	t := time.NewTicker(time.Duration(d.cfg.PollInterval))
	defer t.Stop()
	recheck := time.NewTicker(retryPause)
	defer recheck.Stop()
	d.sweep(ctx)
	d.pollAll(ctx)
	for {
		select {
		case <-ctx.Done():
			if srv != nil {
				// Deliveries take milliseconds; a connection left open
				// longer is a client's, not a delivery's.
				sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
				if srv.Shutdown(sctx) != nil {
					_ = srv.Close()
				}
				cancel()
			}
			d.mu.Lock()
			d.closing = true
			d.mu.Unlock()
			d.wg.Wait()
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			d.sweep(sctx)
			cancel()
			return
		case <-recheck.C:
			d.recheck(ctx)
		case <-t.C:
			d.sweep(ctx)
			d.pollAll(ctx)
		}
	}
}

// sweep removes the orphaned runners it can.
func (d *Dispatcher) sweep(ctx context.Context) {
	d.mu.Lock()
	list := d.orphans
	d.orphans = nil
	d.mu.Unlock()
	var keep []orphan
	for _, o := range list {
		if ctx.Err() != nil {
			keep = append(keep, o)
			continue
		}
		found, err := d.removeOrphan(ctx, o)
		switch {
		case err != nil && time.Since(o.at) > orphanTTL:
			d.log.Warn("leaving a runner registered", "scope", o.scope, "runner_id", strconv.FormatInt(o.id, 10), "runner", o.name, "err", err)
		case err != nil:
			keep = append(keep, o)
		case o.id == 0 && !found && time.Since(o.at) < d.settle:
			// GitHub may yet register it, from a request still on its way.
			keep = append(keep, o)
		}
	}
	d.mu.Lock()
	d.orphans = append(d.orphans, keep...)
	d.mu.Unlock()
}

// removeOrphan removes o, and says whether it was there to remove.
func (d *Dispatcher) removeOrphan(ctx context.Context, o orphan) (bool, error) {
	ids := []int64{o.id}
	if o.id == 0 {
		var err error
		if ids, err = d.gh.runnersNamed(ctx, o.scope, o.name); err != nil {
			return false, err
		}
	}
	for _, id := range ids {
		if err := d.gh.deleteRunner(ctx, o.scope, id); err != nil {
			return false, err
		}
		d.log.Info("removed an orphaned runner", "scope", o.scope, "runner_id", strconv.FormatInt(id, 10))
	}
	return len(ids) > 0, nil
}

func (d *Dispatcher) orphaned(o orphan) {
	o.at = time.Now()
	d.mu.Lock()
	d.orphans = append(d.orphans, o)
	d.mu.Unlock()
}

// activeFor is how long a repo a webhook spoke of is polled with the
// explicit repos, for deliveries that never came.
const activeFor = 24 * time.Hour

// listEvery is how often the orgs' repos are listed again.
const listEvery = 10 * time.Minute

// pollRepos are the repos to poll: those listed, and the orgs'. With a
// webhook, of the orgs' repos only those it spoke of lately: the others
// are left to their deliveries.
func (d *Dispatcher) pollRepos(ctx context.Context) []string {
	repos := slices.Clone(d.cfg.Repos)
	seen := map[string]bool{}
	for _, r := range repos {
		seen[strings.ToLower(r)] = true
	}
	add := func(r string) {
		if !seen[strings.ToLower(r)] {
			seen[strings.ToLower(r)] = true
			repos = append(repos, r)
		}
	}
	if d.cfg.Webhook != nil {
		d.mu.Lock()
		defer d.mu.Unlock()
		for r, at := range d.active {
			if time.Since(at) > activeFor {
				delete(d.active, r)
				continue
			}
			add(r)
		}
		return repos
	}
	d.mu.Lock()
	stale := time.Since(d.listed) > listEvery
	d.mu.Unlock()
	if stale && len(d.cfg.Orgs) > 0 {
		lists := map[string][]string{}
		ok := true
		for _, org := range d.cfg.Orgs {
			l, err := d.gh.orgRepos(ctx, org)
			if err != nil {
				if ctx.Err() == nil {
					d.log.Warn("listing an org's repos; using the last list", "org", org, "err", err)
				}
				ok = false
				continue
			}
			lists[org] = l
		}
		d.mu.Lock()
		if d.orgList == nil {
			d.orgList = map[string][]string{}
		}
		maps.Copy(d.orgList, lists)
		if ok {
			d.listed = time.Now()
		}
		d.mu.Unlock()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, org := range d.cfg.Orgs {
		for _, r := range d.orgList[org] {
			add(r)
		}
	}
	return repos
}

// pollAll lists every repo's queued jobs and offers each to a runner. Only
// when every list came back whole are jobs no longer queued forgotten.
func (d *Dispatcher) pollAll(ctx context.Context) {
	queued := map[int64]bool{}
	whole := true
	for _, repo := range d.pollRepos(ctx) {
		if ctx.Err() != nil {
			return
		}
		jobs, err := d.gh.queuedJobs(ctx, repo)
		if err != nil && ctx.Err() == nil {
			d.log.Warn("listing queued jobs", "repo", repo, "err", err)
		}
		// Rate limited, a runner could not be registered anyway; and a VM
		// costs.
		if ctx.Err() != nil || errors.As(err, new(rateLimited)) {
			return
		}
		if err != nil {
			whole = false
		}
		for _, j := range jobs {
			queued[j.ID] = true
			d.offer(ctx, repo, j, "poll")
		}
	}
	if whole {
		d.mu.Lock()
		for id := range d.watched {
			queued[id] = true // a job of a repo not polled
		}
		d.mu.Unlock()
		d.unwait(queued)
		d.prune(queued) // a job missing from a partial list may still wait
	}
}

// unwait stops the wait for a VM of each job no longer queued: another
// runner took it, or it was cancelled, while it waited for room. Without,
// it would get a VM only to find it has nothing to run.
func (d *Dispatcher) unwait(queued map[int64]bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, a := range d.taken {
		if !queued[id] && a.unwait != nil {
			a.unwait()
			a.unwait = nil
		}
	}
}

// offer starts a runner for a queued job, unless it has one or isn't
// fleetci's.
func (d *Dispatcher) offer(ctx context.Context, repo string, j job, via string) {
	class, ok := d.classFor(repo, j)
	if !ok {
		return
	}
	took, first := d.take(j.ID)
	if first && d.cfg.Pickup != nil && !j.CreatedAt.IsZero() {
		d.cfg.Pickup(class, via, max(time.Since(j.CreatedAt), 0))
	}
	if !took {
		return
	}
	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		return
	}
	d.wg.Add(1)
	d.mu.Unlock()
	go func() {
		defer d.wg.Done()
		d.serve(ctx, repo, j, class)
	}()
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
	if !slices.Contains(d.classes, class) {
		problem = fmt.Sprintf("no class %q; classes are %s", class, strings.Join(d.classes, ", "))
	}
	for _, l := range j.Labels {
		l = strings.ToLower(l)
		if l == d.cfg.LabelPrefix || strings.HasPrefix(l, d.cfg.LabelPrefix+"-") || slices.Contains(genericLabels, l) {
			continue
		}
		arch, isArch := archLabels[l]
		switch {
		case !isArch:
			problem = fmt.Sprintf("label %q is not served by fleetci", l)
		case d.arches[class] == "":
			problem = fmt.Sprintf("label %q: class %q's architecture is not known; set its connector_config.arch", l, class)
		case d.arches[class] != arch:
			problem = fmt.Sprintf("label %q: class %q is %s", l, class, d.arches[class])
		}
	}
	if problem != "" {
		d.mu.Lock()
		if !d.warned[j.ID] {
			d.warned[j.ID] = true
			d.log.Warn("job left queued: "+problem, "repo", repo, "job", j.Name, "job_id", strconv.FormatInt(j.ID, 10), "labels", j.Labels)
		}
		d.mu.Unlock()
		return "", false
	}
	return class, true
}

func (d *Dispatcher) take(id int64) (took, first bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	a, ok := d.taken[id]
	switch {
	case !ok:
		a = &attempt{}
		d.taken[id] = a
		first = true
	case a.running || a.tries >= maxTries || time.Now().Before(a.retryAt):
		return false, false
	}
	a.tries++
	a.serves++
	a.running = true
	a.at = time.Now()
	return true, first
}

// attempt tracks the runners started for one job.
type attempt struct {
	at      time.Time
	tries   int
	serves  int // every serve, those forgotten unused too
	running bool
	retryAt time.Time
	// unwait stops the wait for the job's VM, set while it lasts.
	unwait context.CancelFunc
}

// maxTries caps the runners started for one job whose runners keep failing
// before they take it.
const maxTries = 3

// retryPause separates the runners started for one job.
var retryPause = 30 * time.Second

// forget lets the next poll start a new runner for the job if it is still
// queued. A try that used counts against maxTries; one that pauses holds
// the next off for retryPause.
func (d *Dispatcher) forget(id int64, used, pause bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	a, ok := d.taken[id]
	if !ok {
		return
	}
	a.running = false
	if pause {
		a.retryAt = time.Now().Add(retryPause)
	}
	if !used {
		a.tries--
	}
}

// done marks the job's runner finished. GitHub hands a runner any queued
// job with its labels, so the one it took may be another: a job still
// queued once retryPause has given GitHub time to show it taken gets a
// runner again, without using a try.
func (d *Dispatcher) done(id int64) {
	d.forget(id, false, true)
}

// claimed leaves a job to the runner another controller started for it:
// the next try waits claimTimeout, and this one doesn't count.
func (d *Dispatcher) claimed(id int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if a, ok := d.taken[id]; ok {
		a.running = false
		a.tries--
		a.retryAt = time.Now().Add(claimTimeout)
	}
}

// prune drops jobs that are no longer queued after an hour, so the map stays
// small but a job is never served twice while it waits.
func (d *Dispatcher) prune(queued map[int64]bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, a := range d.taken {
		if !queued[id] && !a.running && time.Since(a.at) > time.Hour {
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
	// IDs as strings: plugin logs travel as JSON, which turns them into floats.
	// The job this runner is started for; GitHub may give it another with
	// the same labels, logged as job once the runner says which.
	log := d.log.With("repo", repo, "queued_job", j.Name, "queued_job_id", strconv.FormatInt(j.ID, 10), "class", class)
	start := time.Now()
	labels := runnerLabels(j.Labels)
	scope := d.scope(repo)

	// The runner is registered while the VM is taken: each takes most of a
	// second, and neither needs the other.
	d.mu.Lock()
	try := 1
	if a := d.taken[j.ID]; a != nil {
		try = a.serves
	}
	d.mu.Unlock()
	name := d.runnerName(class, j.ID, try)
	wctx, unwait := context.WithCancel(ctx)
	regc := make(chan registration, 1)
	go func() {
		r := d.register(ctx, scope, name, labels)
		if d.cfg.NameByJob && conflict(r.err) {
			// Another controller's runner holds the job: no VM for it.
			unwait()
		}
		regc <- r
	}()
	d.mu.Lock()
	if a := d.taken[j.ID]; a != nil {
		a.unwait = unwait
	}
	d.mu.Unlock()
	// A fleet places the VM where the job's cache is, as far as the job
	// queued says: the runner may take another, which then starts cold.
	cctx := wctx
	var ahead *factsAhead
	if relay, ok := d.capacity.(source.CacheRelay); ok && relay.ClassHasCache(class) {
		ahead = d.factsAhead(ctx, repo, j)
		if f, ok := ahead.within(ctx, placeWait); ok {
			cctx = source.WithCaches(wctx, cacheScope(d.cfg.CacheDisk, d.instance(), f).Dirs())
		}
	}
	created, err := d.capacity.Create(cctx, class)
	d.mu.Lock()
	if a := d.taken[j.ID]; a != nil {
		a.unwait = nil
	}
	d.mu.Unlock()
	gone := wctx.Err() != nil && ctx.Err() == nil
	unwait()
	reg := <-regc
	if err == nil && reg.err == nil && time.Since(reg.at) > registrationFresh {
		// A long wait for room: a config that old is swapped for a new one
		// rather than trusted to still work.
		d.unregister(ctx, scope, reg.id, log)
		reg = d.register(ctx, scope, name, labels)
	}
	// handedOver: fleetci is stopping, and the runner, running its job, is
	// left on its VM to the next fleetci, which removes both after it.
	handedOver := false
	if reg.err == nil {
		defer func() {
			if !handedOver {
				d.unregister(ctx, scope, reg.id, log)
			}
		}()
	} else if !refused(reg.err) {
		// GitHub may have registered it all the same.
		d.orphaned(orphan{scope: scope, name: reg.name})
	}

	if d.cfg.NameByJob && conflict(reg.err) {
		log.Info("a runner of this name is registered already, by another controller: the job is left to it", "runner", reg.name, "for", claimTimeout.String())
		if err == nil {
			_ = d.capacity.Delete(context.WithoutCancel(ctx), created.ID)
		}
		d.claimed(j.ID)
		return
	}
	if err != nil && gone {
		log.Info("job taken by another runner while it waited for a vm", "waited", time.Since(start).Round(time.Second).String())
		d.done(j.ID)
		return
	}
	if err != nil {
		if ctx.Err() == nil {
			log.Error("no vm for job; retrying", "err", err)
		}
		d.forget(j.ID, false, false)
		return
	}
	defer func() {
		if !handedOver {
			_ = d.capacity.Delete(context.WithoutCancel(ctx), created.ID)
		}
	}()
	log = log.With("vm", created.ID, "instance", created.Instance)
	if d.takenElsewhere(ctx, repo, j, start, log) {
		return
	}
	if d.cfg.QueueWait != nil && !j.CreatedAt.IsZero() {
		d.cfg.QueueWait(class, max(time.Since(j.CreatedAt), 0))
	}
	conn, err := d.capacity.Conn(created.ID)
	if err != nil {
		log.Error("no connection to the vm; retrying", "err", err)
		d.forget(j.ID, false, false)
		return
	}
	vm := VM{VM: created, Conn: conn}
	if ca, ok := d.capacity.(source.CacheAttacher); ok && ca.HasCache(created.ID) {
		vm.Cache = true
	}

	if reg.err != nil {
		log.Error("registering runner; retrying", "err", reg.err)
		// A GitHub that errs or limits is no fault of the job's: only a
		// refusal uses a try.
		d.forget(j.ID, refused(reg.err), true)
		return
	}
	runnerID, jit := reg.id, reg.jit
	log.Info("runner starting", "runner", reg.name, "runner_id", strconv.FormatInt(runnerID, 10), "labels", labels, "waited", time.Since(start).Round(time.Second).String())

	rctx, cancel := context.WithCancel(ctx)
	// The cache disk is attached, or given up, before the VM goes.
	var attaching sync.WaitGroup
	defer func() { cancel(); attaching.Wait() }()
	go d.watchIdle(rctx, cancel, scope, runnerID, log)
	if vm.Cache && ahead == nil {
		ahead = d.factsAhead(rctx, repo, j)
	}

	var took atomic.Pointer[string]
	err = d.start(rctx, vm, jit, func(job string) {
		first := took.Load() == nil
		took.Store(&job)
		log.Info("runner took a job", "job", job)
		if vm.Cache && first {
			attaching.Add(1)
			go func() {
				defer attaching.Done()
				d.attachCache(rctx, vm, repo, j.ID, reg.name, ahead, log.With("job", job))
			}()
		}
	})
	if job := took.Load(); job != nil {
		log = log.With("job", *job)
	}
	switch {
	case ctx.Err() != nil:
		// A runner that took no job yet goes with its VM, and its job
		// waits for the next fleetci, unless HandOverIdle leaves it.
		if (took.Load() != nil || d.cfg.HandOverIdle) && d.capacity.HandOver(context.WithoutCancel(ctx), created.ID) {
			handedOver = true
			log.Info("fleetci stopping: runner left on its vm, for the next fleetci", "took", took.Load() != nil)
		}
	case err != nil || rctx.Err() != nil:
		// Retried while the job is still queued: a runner that failed or
		// was stopped idle never took it.
		if rctx.Err() == nil {
			log.Error("runner failed; retrying if the job is still queued", "err", err)
		}
		d.forget(j.ID, true, true)
	default:
		d.done(j.ID)
		log.Info("runner done", "took", time.Since(start).Round(time.Second).String())
	}
}

// recheckAfter is how long a job may wait for its VM before it is checked
// again: by then another runner may have taken it.
var recheckAfter = 5 * time.Second

// takenElsewhere reports whether the job no longer waits, so its runner
// would find nothing to do: GitHub gives a runner any queued job its labels
// fit, and while this job waited for a VM, or since its last runner took
// another job, a runner of another job may have taken it. Checked only then,
// sparing a job whose VM came at once the call.
func (d *Dispatcher) takenElsewhere(ctx context.Context, repo string, j job, start time.Time, log hclog.Logger) bool {
	d.mu.Lock()
	retry := d.taken[j.ID] != nil && d.taken[j.ID].serves > 1
	d.mu.Unlock()
	if !retry && time.Since(start) < recheckAfter {
		return false
	}
	cur, err := d.gh.job(ctx, repo, j.ID)
	if err != nil || cur.Status == "queued" {
		return false // unsure: a runner too many beats a job left waiting
	}
	log.Info("job taken by another runner; vm not used", "status", cur.Status)
	d.done(j.ID)
	return true
}

// registrationFresh is how old a runner's registration may be when its VM
// comes. Most come within a second; one that waited for room longer is
// registered again.
var registrationFresh = time.Minute

// registration is a just-in-time runner GitHub registered, or why it did
// not.
type registration struct {
	name string
	id   int64
	jit  string
	at   time.Time
	err  error
}

// register registers a runner under name.
func (d *Dispatcher) register(ctx context.Context, scope, name string, labels []string) registration {
	r := registration{name: name}
	group := d.cfg.RunnerGroupID
	if strings.HasPrefix(scope, "/repos/") {
		group = 1 // a repo's runners are in no group of the org's
	}
	r.id, r.jit, r.err = d.gh.jitConfig(ctx, scope, r.name, group, labels)
	r.at = time.Now()
	return r
}

// unregister removes a runner, or leaves it to the orphan sweep.
func (d *Dispatcher) unregister(ctx context.Context, scope string, id int64, log hclog.Logger) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := d.gh.deleteRunner(dctx, scope, id); err != nil {
		log.Warn("removing runner; trying again later", "runner_id", strconv.FormatInt(id, 10), "err", err)
		d.orphaned(orphan{scope: scope, id: id})
	}
}

// runnerName names a runner after its class, as fleetci's VMs are, and with
// NameByJob after its job and try; otherwise the name is its own, as the VM
// it will run on is not known yet.
func (d *Dispatcher) runnerName(class string, job int64, try int) string {
	if d.cfg.NameByJob {
		return fmt.Sprintf("fleetci-%s-j%d-%d", class, job, try)
	}
	return runnerName(class)
}

func runnerName(class string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "fleetci-" + class + "-" + hex.EncodeToString(b)
}

// watchIdle stops a runner that has not picked up a job within IdleTimeout.
func (d *Dispatcher) watchIdle(ctx context.Context, stop context.CancelFunc, scope string, id int64, log hclog.Logger) {
	deadline := time.Now().Add(time.Duration(d.cfg.IdleTimeout))
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		r, err := d.gh.runner(ctx, scope, id)
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

// arches maps each class to its architecture, where known.
func arches(classes []source.Class) map[string]string {
	m := map[string]string{}
	for _, c := range classes {
		if c.Arch != "" {
			m[c.Name] = c.Arch
		}
	}
	return m
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

package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/cachedisk"
	"github.com/nesty92/fleetci/internal/proxy"
	"github.com/nesty92/fleetci/internal/source"
)

// A GitHub job's cache disk is attached once the job runs: GitHub gives a
// runner any queued job its labels fit, so which repo and branch the VM
// serves is known only then. The runner's job-started hook
// (jobStartedHook) writes the job's repo and run, and waits; fleetci finds
// the job on GitHub's API, from what GitHub says of it and not what the
// VM says, attaches its disk, and writes the hook its answer, which goes
// in the job's log.

// waitJobScript waits for the hook to say which run the job is in, and
// claims the decision; it exits 4 when the hook gave up first.
const waitJobScript = `d=/run/fleetci
i=0
until [ -s "$d/job" ]; do
  i=$((i+1)); [ $i -lt 1200 ] || exit 3
  sleep 0.1
done
mkdir "$d/decided" 2>/dev/null || exit 4
cat "$d/job"`

// answerScript gives the hook its answer, from stdin.
const answerScript = `cat > /run/fleetci/done.new && mv /run/fleetci/done.new /run/fleetci/done`

// ErrHookGaveUp says the job's hook stopped waiting for its cache disk
// before fleetci claimed the decision: the job runs without one.
var ErrHookGaveUp = errors.New("the job's hook stopped waiting")

// WaitJobHook waits for the VM's job-started hook to say which run the
// job is in, "owner/repo run-id", and claims the decision.
func WaitJobHook(ctx context.Context, conn *proxy.Conn) (string, error) {
	wctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	out, err := onVM(wctx, conn, waitJobScript, "")
	var exit interface{ ExitStatus() int }
	if errors.As(err, &exit) && exit.ExitStatus() == 4 {
		return "", ErrHookGaveUp
	}
	if err != nil {
		return "", fmt.Errorf("the job's hook did not say which job it is: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// AnswerJobHook gives the hook its answer, for the job's log; the job goes
// on then.
func AnswerJobHook(ctx context.Context, conn *proxy.Conn, msg string) error {
	_, err := onVM(ctx, conn, answerScript, msg+"\n")
	return err
}

// factsAhead is what GitHub says of the job a runner was started for,
// asked while its VM and runner start, so its disk is attached as soon as
// the runner has the job, if it is that one.
type factsAhead struct {
	done  chan struct{}
	facts cacheFacts
	err   error
}

func (d *Dispatcher) factsAhead(ctx context.Context, repo string, j job) *factsAhead {
	if j.RunID <= 0 {
		return nil
	}
	a := &factsAhead{done: make(chan struct{})}
	go func() {
		defer close(a.done)
		a.facts, a.err = d.cacheFacts(ctx, repo, j)
	}()
	return a
}

// placeWait is how long a fleet's placement waits for the facts.
const placeWait = 2 * time.Second

// within is get, waiting no longer than wait.
func (a *factsAhead) within(ctx context.Context, wait time.Duration) (cacheFacts, bool) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	return a.get(ctx)
}

// get is the facts, if they came without error.
func (a *factsAhead) get(ctx context.Context) (cacheFacts, bool) {
	if a == nil {
		return cacheFacts{}, false
	}
	select {
	case <-a.done:
		return a.facts, a.err == nil
	case <-ctx.Done():
		return cacheFacts{}, false
	}
}

// attachCache gives the VM's job its cache disk, and the hook its answer.
// queued is the job the runner was started for; runner is its name; ahead,
// if not nil, the facts of the queued job.
func (d *Dispatcher) attachCache(ctx context.Context, vm VM, repo string, queued int64, runner string, ahead *factsAhead, log hclog.Logger) {
	ca, ok := d.capacity.(source.CacheAttacher)
	if !ok {
		return
	}
	// A fleet's VM is on a worker, which relays the hook.
	relay, remote := d.capacity.(source.CacheRelay)
	var hint string
	var err error
	if remote {
		hint, err = relay.JobHint(ctx, vm.ID)
	} else {
		hint, err = WaitJobHook(ctx, vm.Conn)
	}
	if err != nil {
		log.Warn("no cache disk", "err", err)
		return
	}
	scope, msg := d.jobScope(ctx, repo, queued, runner, hint, ahead, log)
	if remote {
		if _, err := relay.GiveCache(ctx, vm.ID, scope, msg); err != nil {
			log.Warn("no cache disk", "err", err)
		}
		return
	}
	if scope != nil {
		msg = d.attach(ctx, ca, vm, *scope, log)
	}
	if err := AnswerJobHook(context.WithoutCancel(ctx), vm.Conn, msg); err != nil {
		log.Warn("the job's hook got no answer; it goes on after ten minutes", "err", err)
	}
}

// jobScope is the cache of the job the runner took, or nil and why not,
// for the job's log.
func (d *Dispatcher) jobScope(ctx context.Context, repo string, queued int64, runner, hint string, ahead *factsAhead, log hclog.Logger) (*cachedisk.Scope, string) {
	jobRepo, j, err := d.runnersJob(ctx, repo, queued, runner, hint)
	if err != nil {
		log.Warn("no cache disk", "err", err)
		return nil, "No cache disk: " + err.Error()
	}
	if jobRepo != repo || j.ID != queued {
		log.Info("the runner took another job than the one it was started for", "job_repo", jobRepo, "job_id", strconv.FormatInt(j.ID, 10))
	}
	f, ok := cacheFacts{}, false
	if jobRepo == repo && j.ID == queued {
		f, ok = ahead.get(ctx)
	}
	if !ok {
		if f, err = d.cacheFacts(ctx, jobRepo, j); err != nil {
			log.Warn("no cache disk", "err", err)
			return nil, "No cache disk: " + err.Error()
		}
	}
	// The runner's word on the ref counts for the run it names.
	hintRepo, hintRun, ref := parseHint(hint)
	if !strings.EqualFold(hintRepo, jobRepo) || hintRun != j.RunID {
		ref = refHint{}
	}
	if err := d.refFacts(ctx, &f, ref); err != nil {
		log.Warn("no cache disk", "err", err)
		return nil, "No cache disk: " + err.Error()
	}
	scope := cacheScope(d.cfg.CacheDisk, d.instance(), f)
	return &scope, ""
}

// attach attaches the job's disk and says what it got, for the job's log.
func (d *Dispatcher) attach(ctx context.Context, ca source.CacheAttacher, vm VM, scope cachedisk.Scope, log hclog.Logger) string {
	msg, err := source.AttachCache(ctx, ca, vm.ID, scope)
	if err != nil {
		log.Warn("no cache disk", "err", err)
		return "No cache disk: " + err.Error()
	}
	if msg == "" {
		return "No cache disk"
	}
	return msg
}

// runnersJob finds the job the runner took: the one it was started for,
// or else one of the run the hook names. GitHub's word decides; the hint
// only says where to look.
func (d *Dispatcher) runnersJob(ctx context.Context, repo string, queued int64, runner, hint string) (string, job, error) {
	hintRepo, runID, _ := parseHint(hint)
	// GitHub may name the runner on the job a moment after the runner has it.
	var why error
	for try := 0; ; try++ {
		j, err := d.gh.job(ctx, repo, queued)
		if err == nil && j.RunnerName == runner {
			return repo, j, nil
		}
		why = err
		switch {
		case hint == "":
			why = errors.Join(why, errors.New("the hook named no run"))
		case runID <= 0 || !d.serves(hintRepo):
			why = errors.Join(why, fmt.Errorf("the hook named %q, not a run fleetci serves", hint))
		default:
			jobs, err := d.gh.runJobs(ctx, hintRepo, runID)
			for _, j := range jobs {
				if j.RunnerName == runner {
					return hintRepo, j, nil
				}
			}
			why = errors.Join(why, err)
		}
		if try == jobLookupTries-1 {
			if why != nil {
				return "", job{}, fmt.Errorf("GitHub does not say which job runner %s took: %w", runner, why)
			}
			return "", job{}, fmt.Errorf("GitHub does not say which job runner %s took", runner)
		}
		select {
		case <-ctx.Done():
			return "", job{}, ctx.Err()
		case <-time.After(jobLookupWait << min(try, 2)):
		}
	}
}

// jobLookupWait is how long runnersJob first waits between tries, doubling
// to four times that, and jobLookupTries how many it makes: GitHub names
// the runner on the job within a second or so, and the job's steps wait
// for its disk.
var jobLookupWait = 250 * time.Millisecond

const jobLookupTries = 7

// How long what GitHub says is kept: a run's branch and event never
// change, a repo's default branch rarely, a branch's protection may.
const (
	runFor  = 30 * time.Minute
	repoFor = 10 * time.Minute
	refFor  = time.Minute
)

// memo keeps answers for a while; errors are not kept. The zero value is
// ready.
type memo[V any] struct {
	mu sync.Mutex
	m  map[string]memoized[V]
}

type memoized[V any] struct {
	v    V
	till time.Time
}

func (m *memo[V]) get(key string, keep time.Duration, fetch func() (V, error)) (V, error) {
	now := time.Now()
	m.mu.Lock()
	e, ok := m.m[key]
	m.mu.Unlock()
	if ok && now.Before(e.till) {
		return e.v, nil
	}
	v, err := fetch()
	if err != nil {
		return v, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.m == nil {
		m.m = map[string]memoized[V]{}
	}
	if len(m.m) >= 1024 {
		for k, e := range m.m {
			if !now.Before(e.till) {
				delete(m.m, k)
			}
		}
	}
	m.m[key] = memoized[V]{v, now.Add(keep)}
	return v, nil
}

// cacheFacts is what GitHub says of a job, for its cache disk.
type cacheFacts struct {
	repo          string
	repoID        int64
	defaultBranch string
	event         string
	branch        string // or tag
	headRepo      string // the repo the branch is in: a fork's for a pull request from one
	protected     bool
	tag           bool
	job           string // its name, a matrix job's values included
}

func (d *Dispatcher) cacheFacts(ctx context.Context, repo string, j job) (cacheFacts, error) {
	f := cacheFacts{repo: repo, job: j.Name}
	r, err := d.repoInfos.get(repo, repoFor, func() (repoInfo, error) { return d.gh.repo(ctx, repo) })
	if err != nil {
		return f, err
	}
	f.repoID, f.defaultBranch = r.ID, r.DefaultBranch
	run, err := d.runs.get(fmt.Sprintf("%s %d", repo, j.RunID), runFor, func() (workflowRun, error) { return d.gh.run(ctx, repo, j.RunID) })
	if err != nil {
		return f, err
	}
	f.event, f.branch, f.headRepo = run.Event, run.HeadBranch, run.HeadRepository.FullName
	if f.branch == "" {
		return f, errors.New("the job's run has no branch")
	}
	return f, nil
}

// refHint is what the job's runner says of its ref, from the job's hook
// (GITHUB_REF_TYPE, GITHUB_REF_PROTECTED): GitHub's own word, in the job
// it sent the runner, given before any of the job's steps run.
type refHint struct{ known, tag, protected bool }

// parseHint reads the hook's line: the job's repo and run, then its ref's
// type and whether it is protected, which an older hook leaves out.
func parseHint(hint string) (repo string, run int64, ref refHint) {
	f := strings.Fields(hint)
	if len(f) > 0 {
		repo = f[0]
	}
	if len(f) > 1 {
		run, _ = strconv.ParseInt(f[1], 10, 64)
	}
	if len(f) > 3 && (f[2] == "branch" || f[2] == "tag") && (f[3] == "true" || f[3] == "false") {
		ref = refHint{known: true, tag: f[2] == "tag", protected: f[3] == "true"}
	}
	return repo, run, ref
}

// refFacts says whether the job's ref is a tag or a protected branch, when
// the writers' rules need to know: as the job's runner says, so the app
// needs no permission for it, or else as GitHub's API says, which needs
// Contents: read.
func (d *Dispatcher) refFacts(ctx context.Context, f *cacheFacts, ref refHint) error {
	if isReview(f.event) || isFork(*f) || !needsRef(d.cfg.CacheDisk.Rules(f.repo)) {
		return nil
	}
	if ref.known {
		f.tag, f.protected = ref.tag, ref.protected && !ref.tag
		return nil
	}
	repo := f.repo
	var err error
	f.protected, err = d.refs.get(repo+" branch "+f.branch, refFor, func() (bool, error) { return d.gh.branchProtected(ctx, repo, f.branch) })
	if errors.Is(err, errNotFound) {
		// Not a branch: a tag's run names the tag.
		f.tag, err = d.refs.get(repo+" tag "+f.branch, refFor, func() (bool, error) { return d.gh.tagExists(ctx, repo, f.branch) })
	}
	if se := (statusError{}); errors.As(err, &se) && se.status == http.StatusForbidden {
		err = fmt.Errorf("the writers' rules need to know whether the branch is protected: the job's runner didn't say, and the app may not read the branch (Contents: read): %w", err)
	}
	return err
}

// needsRef says whether the rules tell branches from tags, or protected
// branches from others; "default" and "all" need neither.
func needsRef(rules []string) bool {
	for _, r := range rules {
		if r != "default" && r != "all" {
			return true
		}
	}
	return false
}

func isReview(event string) bool { return strings.HasPrefix(event, "pull_request") }

func isFork(f cacheFacts) bool { return f.headRepo != "" && !strings.EqualFold(f.headRepo, f.repo) }

// cacheScope is the job's cache disk: its repo's, under its branch.
func cacheScope(p *cachedisk.Policy, instance string, f cacheFacts) cachedisk.Scope {
	review := isReview(f.event)
	r := cachedisk.Request{
		Forge:    "github",
		Instance: instance,
		Project:  strconv.FormatInt(f.repoID, 10),
		Path:     f.repo,
		Job: cachedisk.Job{
			Ref:       f.branch,
			Tag:       f.tag,
			Protected: f.protected && !review, // a pull request runs the merge, not its branch
			Default:   !f.tag && f.branch == f.defaultBranch,
			Review:    review,
			Name:      f.job,
		},
	}
	if isFork(f) {
		r.Fork = f.headRepo
	}
	if f.defaultBranch != "" {
		r.From = []string{f.defaultBranch}
	}
	return p.Scope(r)
}

// instance is GitHub's host, for the disks' keys.
func (d *Dispatcher) instance() string {
	if u, err := url.Parse(d.cfg.APIURL); err == nil && u.Host != "" {
		return u.Host
	}
	return d.cfg.APIURL
}

// onVM runs a script on the VM as its user and returns its output.
func onVM(ctx context.Context, conn *proxy.Conn, script, stdin string) (string, error) {
	if conn == nil {
		return "", errors.New("no connection to the vm")
	}
	client, _, err := conn.Client(ctx)
	if err != nil {
		return "", err
	}
	s, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer s.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-done:
		}
	}()
	var out, errOut bytes.Buffer
	s.Stdin, s.Stdout, s.Stderr = strings.NewReader(stdin), &out, &errOut
	if err := s.Run(script); err != nil {
		if msg := strings.TrimSpace(errOut.String()); msg != "" {
			return out.String(), fmt.Errorf("%w: %s", err, msg)
		}
		return out.String(), err
	}
	return out.String(), nil
}

// The few calls the cache disk needs.

func (c *client) runJobs(ctx context.Context, repo string, run int64) ([]job, error) {
	var list struct {
		Jobs []job `json:"jobs"`
	}
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=latest&per_page=100", repo, run), nil, &list)
	return list.Jobs, err
}

type workflowRun struct {
	Event          string `json:"event"`
	HeadBranch     string `json:"head_branch"`
	HeadRepository struct {
		FullName string `json:"full_name"`
	} `json:"head_repository"`
}

func (c *client) run(ctx context.Context, repo string, id int64) (workflowRun, error) {
	var r workflowRun
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runs/%d", repo, id), nil, &r)
	return r, err
}

type repoInfo struct {
	ID            int64  `json:"id"`
	DefaultBranch string `json:"default_branch"`
}

func (c *client) repo(ctx context.Context, repo string) (repoInfo, error) {
	var r repoInfo
	err := c.do(ctx, http.MethodGet, "/repos/"+repo, nil, &r)
	return r, err
}

// branchProtected says whether GitHub protects the branch; errNotFound
// when there is no such branch.
func (c *client) branchProtected(ctx context.Context, repo, branch string) (bool, error) {
	var b struct {
		Protected bool `json:"protected"`
	}
	err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/branches/"+escapeRef(branch), nil, &b)
	return b.Protected, err
}

func (c *client) tagExists(ctx context.Context, repo, tag string) (bool, error) {
	err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/git/ref/tags/"+escapeRef(tag), nil, nil)
	if errors.Is(err, errNotFound) {
		return false, nil
	}
	return err == nil, err
}

// escapeRef escapes each part of a branch or tag's name for a URL path.
func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

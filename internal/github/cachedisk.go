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

// attachCache gives the VM's job its cache disk, and the hook its answer.
// queued is the job the runner was started for; runner is its name.
func (d *Dispatcher) attachCache(ctx context.Context, vm VM, repo string, queued int64, runner string, log hclog.Logger) {
	ca, ok := d.capacity.(source.CacheAttacher)
	if !ok {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	out, err := onVM(wctx, vm.Conn, waitJobScript, "")
	cancel()
	var exit interface{ ExitStatus() int }
	if errors.As(err, &exit) && exit.ExitStatus() == 4 {
		log.Warn("no cache disk: the job's hook stopped waiting")
		return
	}
	if err != nil {
		log.Warn("no cache disk: the job's hook did not say which job it is", "err", err)
		return
	}
	msg := d.cacheDisk(ctx, ca, vm, repo, queued, runner, strings.TrimSpace(out), log)
	if _, err := onVM(context.WithoutCancel(ctx), vm.Conn, answerScript, msg+"\n"); err != nil {
		log.Warn("the job's hook got no answer; it goes on after ten minutes", "err", err)
	}
}

// cacheDisk attaches the job's disk and says what it got, for the job's log.
func (d *Dispatcher) cacheDisk(ctx context.Context, ca source.CacheAttacher, vm VM, repo string, queued int64, runner, hint string, log hclog.Logger) string {
	repo, j, err := d.runnersJob(ctx, repo, queued, runner, hint)
	if err != nil {
		log.Warn("no cache disk", "err", err)
		return "No cache disk: " + err.Error()
	}
	log = log.With("job_id", strconv.FormatInt(j.ID, 10))
	f, err := d.cacheFacts(ctx, repo, j)
	if err != nil {
		log.Warn("no cache disk", "err", err)
		return "No cache disk: " + err.Error()
	}
	msg, err := source.AttachCache(ctx, ca, vm.ID, cacheScope(d.cfg.CacheDisk, d.instance(), f))
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
	hintRepo, run, _ := strings.Cut(hint, " ")
	runID, _ := strconv.ParseInt(run, 10, 64)
	// GitHub may name the runner on the job a moment after the runner has it.
	for try := 0; ; try++ {
		if j, err := d.gh.job(ctx, repo, queued); err == nil && j.RunnerName == runner {
			return repo, j, nil
		}
		if runID > 0 && d.serves(hintRepo) {
			jobs, err := d.gh.runJobs(ctx, hintRepo, runID)
			if err == nil {
				for _, j := range jobs {
					if j.RunnerName == runner {
						return hintRepo, j, nil
					}
				}
			}
		}
		if try == 4 {
			return "", job{}, errors.New("GitHub does not say which job the runner took")
		}
		select {
		case <-ctx.Done():
			return "", job{}, ctx.Err()
		case <-time.After(jobLookupWait):
		}
	}
}

// jobLookupWait is how long runnersJob waits between tries.
var jobLookupWait = time.Second

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
}

func (d *Dispatcher) cacheFacts(ctx context.Context, repo string, j job) (cacheFacts, error) {
	f := cacheFacts{repo: repo}
	r, err := d.gh.repo(ctx, repo)
	if err != nil {
		return f, err
	}
	f.repoID, f.defaultBranch = r.ID, r.DefaultBranch
	run, err := d.gh.run(ctx, repo, j.RunID)
	if err != nil {
		return f, err
	}
	f.event, f.branch, f.headRepo = run.Event, run.HeadBranch, run.HeadRepository.FullName
	if f.branch == "" {
		return f, errors.New("the job's run has no branch")
	}
	if isReview(f.event) || isFork(f) {
		return f, nil
	}
	f.protected, err = d.gh.branchProtected(ctx, repo, f.branch)
	if errors.Is(err, errNotFound) {
		// Not a branch: a tag's run names the tag.
		f.tag, err = d.gh.tagExists(ctx, repo, f.branch)
	}
	return f, err
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

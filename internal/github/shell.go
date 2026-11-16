package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"

	"github.com/nesty92/fleetci/internal/shell"
	"github.com/nesty92/fleetci/internal/source"
)

// ShellConfig lets a failed job's VM be debugged over SSH (package shell),
// by whoever re-ran the job with debug logging: GitHub's "Re-run jobs" →
// "Enable debug logging". The job's log, at its end, says how to connect;
// if the job then fails, its VM is kept for Hold. Only someone with write
// access to the repo is let in, with the SSH keys on their GitHub account,
// and never into a pull request from a fork's job.
type ShellConfig = shell.Config

// shellHook ends the job, in the job-completed hook, when the job was run
// with debug logging: it asks fleetci for a shell, and prints the answer,
// how to connect or why not, in the job's log. The hook can't tell
// whether the job failed; fleetci learns that after it.
const shellHook = `if [ "${RUNNER_DEBUG:-}" = 1 ]; then
  d=$HOME/.fleetci/shell
  mkdir -p "$d" && rm -f "$d/answer" &&
    printf '%s %s %s\n' "$GITHUB_REPOSITORY" "$GITHUB_RUN_ID" "${GITHUB_RUN_ATTEMPT:-1}" > "$d/ask.new" && mv "$d/ask.new" "$d/ask"
  i=0
  until [ -e "$d/answer" ] || [ $i -ge 600 ]; do i=$((i+1)); sleep 0.1; done
  if [ -e "$d/answer" ]; then cat "$d/answer"; else echo "fleetci: no shell: fleetci did not answer"; fi
fi
`

// shellWaitScript waits on the VM for the hook to ask, and prints what
// it asked; it gives up after 12 hours, should its VM outlive its job.
const shellWaitScript = `d=$HOME/.fleetci/shell
i=0
until [ -e "$d/ask" ]; do
  i=$((i+1)); [ $i -lt 86400 ] || exit 1; sleep 0.5
done
cat "$d/ask"`

// shellAnswerScript gives the hook its answer, from stdin.
const shellAnswerScript = `d=$HOME/.fleetci/shell
cat > "$d/answer.new" && mv "$d/answer.new" "$d/answer"`

// shellAsk is a job's shell: none until its hook asks, then the server,
// once fleetci has let the run's actor in.
type shellAsk struct {
	wg  sync.WaitGroup
	mu  sync.Mutex
	srv *shell.Server
	job job
	// repo is the job's, which may not be the queued job's.
	repo, actor string
}

// result is the job's shell server, nil when none, once the watch is
// over.
func (a *shellAsk) result() (*shell.Server, string, job, string) {
	if a == nil {
		return nil, "", job{}, ""
	}
	a.wg.Wait()
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.srv, a.repo, a.job, a.actor
}

// close closes the job's shell server, if it has one.
func (a *shellAsk) close() {
	if srv, _, _, _ := a.result(); srv != nil {
		_ = srv.Close()
	}
}

// watchShell answers the job's hook, if it asks for a shell, until ctx
// ends. The server it starts lives until held's ctx ends, or Close.
func (d *Dispatcher) watchShell(ctx, held context.Context, vm VM, repo string, queued int64, runner string, log hclog.Logger) *shellAsk {
	a := &shellAsk{}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		// Closing the session doesn't end the wait on the VM, so it's
		// left behind when ctx ends: it ends with the VM.
		type asked struct {
			hint string
			err  error
		}
		got := make(chan asked, 1)
		go func() {
			hint, err := onVM(ctx, vm.Conn, shellWaitScript, "")
			got <- asked{hint, err}
		}()
		var hint string
		select {
		case <-ctx.Done():
			return
		case r := <-got:
			if r.err != nil || ctx.Err() != nil {
				return
			}
			hint = r.hint
		}
		answer := func(msg string) {
			if _, err := onVM(ctx, vm.Conn, shellAnswerScript, msg+"\n"); err != nil {
				log.Warn("shell: could not answer the job", "err", err)
			}
		}
		jobRepo, j, actor, keys, err := d.shellFor(ctx, repo, queued, runner, strings.TrimSpace(hint))
		if err != nil {
			log.Info("shell: refused", "why", err)
			answer("fleetci: no shell: " + err.Error())
			return
		}
		srv, err := shell.Start(held, shell.Options{
			Keys:       keys,
			Who:        "@" + actor,
			VM:         func(ctx context.Context) (*ssh.Client, error) { c, _, err := vm.Conn.Client(ctx); return c, err },
			DERPMapURL: d.cfg.Shell.DERPMapURL,
			Log:        log,
		})
		if err != nil {
			log.Warn("shell: could not start", "err", err)
			answer("fleetci: no shell: " + err.Error())
			return
		}
		a.mu.Lock()
		a.srv, a.repo, a.job, a.actor = srv, jobRepo, j, actor
		a.mu.Unlock()
		log.Info("shell: offered", "actor", actor)
		answer(fmt.Sprintf(`fleetci: if this job fails, its VM is kept for %s, for @%s, with the SSH keys on their GitHub account. Connect with
  tailcat ssh %[3]s
or
  fleetci ssh %[3]s
(tailcat: https://github.com/tailscale/tailcat)`, d.cfg.Shell.HoldFor(), actor, srv.Addr()))
	}()
	return a
}

// errShell is a reason not to let anyone into a job's VM.
var errShell = errors.New("")

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w"+format, append([]any{errShell}, a...)...)
}

// shellFor checks, with GitHub, the run the hook names (its repo, run and
// attempt): that the runner ran a job of it, that it is not a fork's pull
// request, and that whoever started the attempt may write to the repo;
// and it reads their SSH keys.
func (d *Dispatcher) shellFor(ctx context.Context, repo string, queued int64, runner, hint string) (string, job, string, string, error) {
	f := strings.Fields(hint)
	if len(f) != 3 {
		return "", job{}, "", "", refuse("the job did not say which run it is")
	}
	jobRepo, j, err := d.runnersJob(ctx, repo, queued, runner, f[0]+" "+f[1])
	if err != nil {
		return "", job{}, "", "", err
	}
	if fmt.Sprint(j.RunAttempt) != f[2] {
		return "", job{}, "", "", refuse("the job is of attempt %d of its run, not %s", j.RunAttempt, f[2])
	}
	run, err := d.gh.runAttempt(ctx, jobRepo, j.RunID, j.RunAttempt)
	if err != nil {
		return "", job{}, "", "", err
	}
	if head := run.HeadRepository.FullName; head != "" && !strings.EqualFold(head, jobRepo) {
		return "", job{}, "", "", refuse("not for a pull request from a fork")
	}
	actor := run.TriggeringActor.Login
	if actor == "" {
		return "", job{}, "", "", refuse("GitHub does not say who started the run")
	}
	perm, err := d.gh.permission(ctx, jobRepo, actor)
	if err != nil {
		return "", job{}, "", "", err
	}
	if perm != "admin" && perm != "write" {
		return "", job{}, "", "", refuse("@%s may not write to %s", actor, jobRepo)
	}
	keys, err := d.gh.userKeys(ctx, ownerOf("/repos/"+jobRepo), actor)
	if err != nil {
		return "", job{}, "", "", err
	}
	if strings.TrimSpace(keys) == "" {
		return "", job{}, "", "", refuse("@%s has no SSH keys on GitHub", actor)
	}
	return jobRepo, j, actor, keys, nil
}

// holdForShell keeps a failed job's VM for its shell, then closes it; a
// job that didn't fail has its shell closed at once. The VM's cache disk
// is taken back first, as at any job's end, so nothing done in the shell
// reaches the caches. It returns when the VM may go.
func (d *Dispatcher) holdForShell(ctx context.Context, a *shellAsk, vm VM, log hclog.Logger) {
	srv, repo, j, actor := a.result()
	if srv == nil {
		return
	}
	defer srv.Close()
	conclusion, err := d.conclusion(ctx, repo, j.ID)
	if err != nil {
		log.Warn("shell: the job's VM isn't kept: its conclusion is unknown", "err", err)
		return
	}
	if conclusion != "failure" {
		log.Info("shell: the job didn't fail: its VM isn't kept", "conclusion", conclusion)
		return
	}
	if vm.Cache {
		dc, ok := d.capacity.(source.CacheDetacher)
		if !ok {
			log.Warn("shell: the job's VM isn't kept: its cache disk can't be taken back first")
			return
		}
		if err := dc.DetachCache(ctx, vm.ID); err != nil {
			log.Warn("shell: the job's VM isn't kept: its cache disk wasn't taken back", "err", err)
			return
		}
	}
	hold := d.cfg.Shell.HoldFor()
	srv.Open()
	log.Info("shell: the failed job's VM is kept", "actor", actor, "for", hold.String())
	select {
	case <-ctx.Done():
	case <-time.After(hold):
	}
	log.Info("shell: the VM's time is up")
}

// conclusion waits for the job to complete, as GitHub says, and says how.
func (d *Dispatcher) conclusion(ctx context.Context, repo string, id int64) (string, error) {
	var err error
	for try := 0; try < 20; try++ {
		var j job
		if j, err = d.gh.job(ctx, repo, id); err == nil && j.Status == "completed" {
			return j.Conclusion, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if err == nil {
		err = errors.New("GitHub has it not completed")
	}
	return "", err
}

// The calls the shell needs.

type runAttempt struct {
	TriggeringActor struct {
		Login string `json:"login"`
	} `json:"triggering_actor"`
	HeadRepository struct {
		FullName string `json:"full_name"`
	} `json:"head_repository"`
}

func (c *client) runAttempt(ctx context.Context, repo string, run, attempt int64) (runAttempt, error) {
	var r runAttempt
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runs/%d/attempts/%d", repo, run, attempt), nil, &r)
	return r, err
}

// permission is the user's on the repo: admin, write (maintain too), read
// or none. It needs Metadata: read.
func (c *client) permission(ctx context.Context, repo, user string) (string, error) {
	var p struct {
		Permission string `json:"permission"`
	}
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/collaborators/%s/permission", repo, user), nil, &p)
	if errors.Is(err, errNotFound) {
		return "none", nil
	}
	return p.Permission, err
}

// userKeys are the user's public SSH keys, in authorized_keys form.
func (c *client) userKeys(ctx context.Context, owner, user string) (string, error) {
	var keys []struct {
		Key string `json:"key"`
	}
	if err := c.doAs(ctx, owner, http.MethodGet, fmt.Sprintf("/users/%s/keys?per_page=100", user), nil, &keys); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k.Key + "\n")
	}
	return b.String(), nil
}

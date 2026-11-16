package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"

	"gitlab.com/gitlab-org/gitlab-runner/common"

	"github.com/nesty92/fleetci/internal/shell"
	"github.com/nesty92/fleetci/internal/source"
)

// sshVariable is the job variable that asks, should the job fail, for its
// VM to be kept for a shell (package shell), for whoever ran the job, with
// the SSH keys on their GitLab account. The runner offers it only with
// [gitlab.shell] set.
const sshVariable = "FLEETCI_SSH"

// heldShell is a failed job's shell, and what Release does with its VM.
type heldShell struct {
	srv shellServer
	// who ran the job, for the log.
	who  string
	hold time.Duration
}

// offerShell, at the end of a job that asked for a shell, while its log is
// still open: if the job failed, it serves the job's VM at a tailcat
// address, for whoever ran the job, says in the log how to connect, and
// leaves Release to keep the VM. Nobody gets onto the VM before then.
func (e *executor) offerShell(jobErr error) {
	b, log := e.build, e.logger
	if b == nil || !isOn(b.GetAllVariables().Value(sshVariable)) {
		return
	}
	ref, ok := b.ExecutorData.(*acquisitionRef)
	if !ok || ref.vm == nil {
		return
	}
	if !scriptFailed(jobErr) {
		log.Println("fleetci: the job didn't fail: its VM isn't kept for a shell")
		return
	}
	cfg := e.p.shell
	if cfg == nil {
		log.Warningln("fleetci: no shell: this runner doesn't offer one")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	user, keys, err := shellFor(ctx, http.DefaultClient, b)
	if err != nil {
		log.Warningln("fleetci: no shell: " + err.Error())
		return
	}
	vm, conn := *ref.vm, ref.conn
	srv, err := startShell(context.Background(), shell.Options{
		Keys: keys,
		Who:  "@" + user,
		VM: func(ctx context.Context) (*ssh.Client, error) {
			c, err := conn(vm.ID)
			if err != nil {
				return nil, err
			}
			client, _, err := c.Client(ctx)
			return client, err
		},
		DERPMapURL: cfg.DERPMapURL,
		Log:        e.p.shellLog(vm.ID),
	})
	if err != nil {
		log.Warningln("fleetci: no shell: " + err.Error())
		return
	}
	ref.shell = &heldShell{srv: srv, who: user, hold: cfg.HoldFor()}
	log.Println(fmt.Sprintf(`fleetci: the job failed: its VM is kept for %s, for @%s, with the SSH keys on their GitLab account. Once the job has ended, connect with
  tailcat ssh %[3]s
or
  fleetci ssh %[3]s
(tailcat: https://github.com/tailscale/tailcat). The job's containers are gone; its images and Docker volumes are on the VM.`, cfg.HoldFor(), user, srv.Addr()))
}

// startShell is shell.Start, which tests replace.
var startShell = func(ctx context.Context, o shell.Options) (shellServer, error) {
	s, err := shell.Start(ctx, o)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// shellServer is what a job's shell needs of shell.Server.
type shellServer interface {
	Addr() string
	Open()
	Close() error
}

// keepForShell keeps a failed job's VM for its shell, then deletes it: the
// VM's cache disk is taken back first, as at any job's end, so nothing done
// in the shell reaches the caches, and only then is the shell let through.
// The VM still counts against its class's max_instances while kept.
func (p *provider) keepForShell(vm string, h *heldShell) {
	log := logrus.WithField("vm", vm)
	defer func() {
		_ = h.srv.Close()
		_ = p.vms.Delete(context.Background(), vm)
	}()
	if c, ok := p.vms.(interface{ HasCache(string) bool }); ok && c.HasCache(vm) {
		d, ok := p.vms.(source.CacheDetacher)
		if !ok {
			log.Warn("shell: the job's VM isn't kept: its cache disk can't be taken back first")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		err := d.DetachCache(ctx, vm)
		cancel()
		if err != nil {
			log.WithError(err).Warn("shell: the job's VM isn't kept: its cache disk wasn't taken back")
			return
		}
	}
	h.srv.Open()
	log.WithFields(logrus.Fields{"for": h.hold.String(), "user": h.who}).Info("shell: the failed job's VM is kept")
	time.Sleep(h.hold)
	log.Info("shell: the VM's time is up")
}

// shellLog is the log of a VM's shell.
func (p *provider) shellLog(vm string) hclog.Logger {
	if p.log == nil {
		return nil
	}
	return p.log.With("vm", vm)
}

// isOn reads a yes/no job variable.
func isOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// scriptFailed says whether the job failed in its own script: not
// canceled, timed out, or failed by the runner.
func scriptFailed(err error) bool {
	var be *common.BuildError
	if !errors.As(err, &be) {
		return false
	}
	return be.FailureReason == "" || be.FailureReason == common.ScriptFailure
}

// shellFor asks GitLab, with the job's token, who ran the job, and reads
// the SSH keys on their account. Anyone who may run a project's pipelines
// is a developer of it; a merge request's pipeline from a fork is refused.
func shellFor(ctx context.Context, hc *http.Client, b *common.Build) (string, string, error) {
	vars := b.GetAllVariables()
	if src := vars.Value("CI_MERGE_REQUEST_SOURCE_PROJECT_ID"); src != "" && src != vars.Value("CI_PROJECT_ID") {
		return "", "", errors.New("not for a merge request from a fork")
	}
	api := strings.TrimRight(b.Runner.URL, "/") + "/api/v4"
	var job struct {
		User struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
		} `json:"user"`
	}
	if err := getJSON(ctx, hc, api+"/job", b.Job.Token, &job); err != nil {
		return "", "", fmt.Errorf("asking GitLab who ran the job: %w", err)
	}
	if job.User.ID == 0 || job.User.Username == "" {
		return "", "", errors.New("GitLab doesn't say who ran the job")
	}
	var keys []struct {
		Key       string `json:"key"`
		UsageType string `json:"usage_type"`
	}
	if err := getJSON(ctx, hc, api+"/users/"+url.PathEscape(fmt.Sprint(job.User.ID))+"/keys", "", &keys); err != nil {
		return "", "", fmt.Errorf("reading @%s's SSH keys: %w", job.User.Username, err)
	}
	var lines []string
	for _, k := range keys {
		if k.UsageType != "signing" && strings.TrimSpace(k.Key) != "" {
			lines = append(lines, k.Key)
		}
	}
	if len(lines) == 0 {
		return "", "", fmt.Errorf("@%s has no SSH keys on GitLab", job.User.Username)
	}
	return job.User.Username, strings.Join(lines, "\n"), nil
}

// getJSON GETs u, with the job token when one is given, into v.
func getJSON(ctx context.Context, hc *http.Client, u, jobToken string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if jobToken != "" {
		req.Header.Set("JOB-TOKEN", jobToken)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("%s", resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

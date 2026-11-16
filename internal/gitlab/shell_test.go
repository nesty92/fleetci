package gitlab

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/buildlogger"
	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/nesty92/fleetci/internal/fleet"
	"github.com/nesty92/fleetci/internal/proxy"
	"github.com/nesty92/fleetci/internal/shell"
	"github.com/nesty92/fleetci/internal/source"
)

// shellGitLab is GitLab's API as shellFor asks it: the job token "tok"
// is user 7's job, user 8's ("nokeys") has only a signing key.
func shellGitLab(t *testing.T, user int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/job":
			if r.Header.Get("JOB-TOKEN") != "tok" {
				http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
				return
			}
			name := map[int]string{7: "dev", 8: "nokeys"}[user]
			_, _ = w.Write([]byte(`{"id":1,"user":{"id":` + strconv.Itoa(user) + `,"username":"` + name + `"}}`))
		case "/api/v4/users/7/keys":
			_, _ = w.Write([]byte(`[{"key":"ssh-ed25519 AAAA dev@a","usage_type":"auth_and_signing"},{"key":"ssh-ed25519 BBBB dev@sign","usage_type":"signing"},{"key":"ssh-ed25519 CCCC dev@b"}]`))
		case "/api/v4/users/8/keys":
			_, _ = w.Write([]byte(`[{"key":"ssh-ed25519 BBBB n@sign","usage_type":"signing"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// shellJob is a job of project 5 on the runner of url, with token and
// variables kv.
func shellJob(url, token string, kv ...string) *common.Build {
	b := &common.Build{Runner: &common.RunnerConfig{RunnerCredentials: common.RunnerCredentials{URL: url + "/"}}}
	b.Job.Token = token
	b.Variables = spec.Variables{{Key: "CI_PROJECT_ID", Value: "5", Public: true}}
	for i := 0; i+1 < len(kv); i += 2 {
		b.Variables = append(b.Variables, spec.Variable{Key: kv[i], Value: kv[i+1], Public: true})
	}
	return b
}

func TestShellFor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		user     int
		token    string
		kv       []string
		who, err string
	}{
		{name: "developer", user: 7, token: "tok", who: "dev"},
		{name: "merge request in the project", user: 7, token: "tok", kv: []string{"CI_MERGE_REQUEST_SOURCE_PROJECT_ID", "5"}, who: "dev"},
		{name: "fork", user: 7, token: "tok", kv: []string{"CI_MERGE_REQUEST_SOURCE_PROJECT_ID", "6"}, err: "fork"},
		{name: "bad token", user: 7, token: "nope", err: "who ran the job: 401"},
		{name: "only signing keys", user: 8, token: "tok", err: "@nokeys has no SSH keys"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := shellGitLab(t, tc.user)
			who, keys, err := shellFor(context.Background(), srv.Client(), shellJob(srv.URL, tc.token, tc.kv...))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if who != tc.who || keys != "ssh-ed25519 AAAA dev@a\nssh-ed25519 CCCC dev@b" {
				t.Fatalf("who %q, keys %q", who, keys)
			}
		})
	}
}

func TestScriptFailed(t *testing.T) {
	for err, want := range map[error]bool{
		nil:                             false,
		errors.New("system"):            false,
		&common.BuildError{ExitCode: 1}: true,
		&common.BuildError{FailureReason: common.ScriptFailure}:       true,
		&common.BuildError{FailureReason: common.JobCanceled}:         false,
		&common.BuildError{FailureReason: common.JobExecutionTimeout}: false,
	} {
		if got := scriptFailed(err); got != want {
			t.Errorf("scriptFailed(%v) = %v", err, got)
		}
	}
}

// events records the order things happen in.
type events struct {
	mu   sync.Mutex
	list []string
}

func (e *events) note(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, s)
}

func (e *events) String() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.list, " ")
}

// fakeShell is a shell server that records what was done with it.
type fakeShell struct{ ev *events }

func (s *fakeShell) Addr() string { return "tc1.fake" }
func (s *fakeShell) Open()        { s.ev.note("open") }
func (s *fakeShell) Close() error { s.ev.note("close"); return nil }

// cacheHost is a host whose VMs have cache disks.
type cacheHost struct {
	fakeHost
	ev   *events
	gone chan string
}

func (h *cacheHost) HasCache(string) bool { return true }
func (h *cacheHost) DetachCache(context.Context, string) error {
	h.ev.note("detach")
	return nil
}
func (h *cacheHost) Delete(_ context.Context, vm string) error {
	h.ev.note("delete")
	h.gone <- vm
	return nil
}

var _ source.CacheDetacher = (*cacheHost)(nil)

type traceBuf struct{ bytes.Buffer }

func (*traceBuf) IsStdout() bool { return false }

// TestAFailedJobKeepsItsVM: a job that asked for a shell and failed is
// told how to connect; Release keeps its VM for the hold, its cache disk
// taken back before anyone is let through, then deletes it.
func TestAFailedJobKeepsItsVM(t *testing.T) {
	srv := shellGitLab(t, 7)
	ev := &events{}
	fs := &fakeShell{ev: ev}
	old := startShell
	startShell = func(context.Context, shell.Options) (shellServer, error) { return fs, nil }
	t.Cleanup(func() { startShell = old })

	h := &cacheHost{ev: ev, gone: make(chan string, 1)}
	p := newProvider(nil, h, func(*common.RunnerConfig) string { return "" }, nil, "")
	p.shell = &shell.Config{Hold: fleet.Duration(50 * time.Millisecond)}

	run := func(jobErr error, kv ...string) (string, *acquisitionRef) {
		b := shellJob(srv.URL, "tok", kv...)
		ref := &acquisitionRef{vm: &source.VM{ID: "vm-1"}, conn: func(string) (*proxy.Conn, error) { return nil, errors.New("fake") }}
		b.ExecutorData = ref
		var out traceBuf
		e := &executor{p: p, build: b, logger: buildlogger.New(&out, logrus.NewEntry(logrus.New()), buildlogger.Options{})}
		e.offerShell(jobErr)
		return out.String(), ref
	}

	if out, ref := run(&common.BuildError{ExitCode: 1}); ref.shell != nil || out != "" {
		t.Fatalf("a job that didn't ask got a shell: %q", out)
	}
	if out, ref := run(nil, sshVariable, "1"); ref.shell != nil || !strings.Contains(out, "didn't fail") {
		t.Fatalf("a job that passed: %q", out)
	}
	out, ref := run(&common.BuildError{ExitCode: 1}, sshVariable, "1")
	if ref.shell == nil || !strings.Contains(out, "fleetci ssh tc1.fake") || !strings.Contains(out, "@dev") || !strings.Contains(out, "Docker on the VM starts empty") {
		t.Fatalf("a failed job: %q", out)
	}
	if ev.String() != "" {
		t.Fatalf("the shell was opened before the job's end: %v", ev)
	}

	p.Release(nil, ref)
	select {
	case <-h.gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the kept VM was never deleted")
	}
	if got := ev.String(); got != "detach open close delete" {
		t.Fatalf("events %q", got)
	}
}

package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"

	"gitlab.com/gitlab-org/gitlab-runner/common"

	"github.com/nesty92/fleetci/internal/source"
)

// failServer records the jobs failed through it, and refuses job 3.
func failServer(t *testing.T) (*httptest.Server, func() map[string]map[string]any) {
	var mu sync.Mutex
	failed := map[string]map[string]any{} // path → body
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/api/v4/jobs/3" {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		failed[r.URL.Path] = body
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, func() map[string]map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return failed
	}
}

// A job noted as taken and never placed, by a controller killed meanwhile,
// is failed when no leader took it in time, as a runner system failure,
// with its own token, and its note dropped; a note made after the start
// read them, by the new leader, is left alone, and one GitLab can't be told
// of is kept.
func TestHeldFailAfter(t *testing.T) {
	srv, failed := failServer(t)
	dir := filepath.Join(t.TempDir(), "held")
	h, log := held(dir), hclog.NewNullLogger()
	h.put(heldJob{ID: 1, Token: "job-token-1", URL: srv.URL}, log)
	h.put(heldJob{ID: 3, Token: "job-token-3", URL: srv.URL}, log)
	hs := LoadHeld(dir, log)
	h.put(heldJob{ID: 2, Token: "job-token-2", URL: srv.URL}, log) // the new leader's
	hs.FailAfter(context.Background(), 0)

	f := failed()
	b, ok := f["/api/v4/jobs/1"]
	if !ok || b["token"] != "job-token-1" || b["state"] != "failed" || b["failure_reason"] != "runner_system_failure" {
		t.Errorf("job 1 not failed as a runner system failure: %v", f)
	}
	if _, ok := f["/api/v4/jobs/2"]; ok {
		t.Error("the new leader's job was failed")
	}
	for id, kept := range map[int64]bool{1: false, 2: true, 3: true} {
		if _, err := os.Stat(h.path(id)); (err == nil) != kept {
			t.Errorf("job %d: note kept %v, want %v", id, err == nil, kept)
		}
	}
	if fi, err := os.Stat(h.path(2)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("a note holds a job's token: mode %v, %v", fi.Mode().Perm(), err)
	}
}

// The jobs go to the leader or to FailAfter, never both; and a controller
// that exits before either leaves the notes for its next start.
func TestHeldTakenOnce(t *testing.T) {
	srv, failed := failServer(t)
	log := hclog.NewNullLogger()

	dir := filepath.Join(t.TempDir(), "held")
	held(dir).put(heldJob{ID: 1, Token: "t", URL: srv.URL}, log)
	hs := LoadHeld(dir, log)
	if got := hs.take(); len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("take: %v", got)
	}
	hs.FailAfter(context.Background(), 0)
	if f := failed(); len(f) != 0 {
		t.Errorf("a job the leader took was failed: %v", f)
	}

	dir = filepath.Join(t.TempDir(), "held")
	held(dir).put(heldJob{ID: 4, Token: "t", URL: srv.URL}, log)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	LoadHeld(dir, log).FailAfter(ctx, time.Hour)
	if _, err := os.Stat(held(dir).path(4)); err != nil || len(failed()) != 0 {
		t.Errorf("a controller exiting failed its jobs, or dropped the notes: %v, %v", failed(), err)
	}
}

type createFunc func(ctx context.Context, class string) (source.VM, error)

type fakeCapacity struct {
	source.Capacity
	create createFunc
}

func (c fakeCapacity) Create(ctx context.Context, class string) (source.VM, error) {
	return c.create(ctx, class)
}

// A job its last process left noted is placed by the next leader on a VM
// of its class, as the worker would have had it, and its note dropped.
func TestResumeHeldPlacesTheJob(t *testing.T) {
	log := hclog.NewNullLogger()
	dir := filepath.Join(t.TempDir(), "held")
	n := heldJob{ID: 7, Token: "t", URL: "https://gitlab.example", Class: "big",
		Job: json.RawMessage(`{"id":7}`), Runner: json.RawMessage(`{"url":"https://gitlab.example","token":"short"}`)}
	held(dir).put(n, log)

	ran := make(chan [3]string, 1)
	mr := &RunCommand{dispatch: &dispatcher{
		ctx:     context.Background(),
		log:     log,
		classes: []string{"small", "big"},
		held:    held(dir),
		resume:  LoadHeld(dir, log),
		capacity: fakeCapacity{create: func(_ context.Context, class string) (source.VM, error) {
			return source.VM{ID: "vm-1", Class: class}, nil
		}},
		remote: func(_ context.Context, vm source.VM, job, runner []byte) error {
			if _, err := os.Stat(held(dir).path(7)); err == nil {
				t.Error("the note outlived the job's placing")
			}
			ran <- [3]string{vm.Class, string(job), string(runner)}
			return nil
		},
	}}
	mr.resumeHeld()
	select {
	case got := <-ran:
		if got[0] != "big" || got[1] != string(n.Job) || got[2] != string(n.Runner) {
			t.Errorf("placed %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the job taken before the restart was not placed")
	}
	if got := mr.dispatch.resume.take(); got != nil {
		t.Errorf("the jobs are still there to fail: %v", got)
	}
}

// A controller that exits while a job waits for a VM leaves its note, and
// doesn't fail it: its next start places it.
func TestPlaceExitingKeepsTheNote(t *testing.T) {
	log := hclog.NewNullLogger()
	dir := filepath.Join(t.TempDir(), "held")
	n := heldJob{ID: 8, Token: "t", URL: "https://gitlab.example", Class: "small"}
	held(dir).put(n, log)
	ctx, cancel := context.WithCancel(context.Background())
	mr := &RunCommand{dispatch: &dispatcher{
		ctx:     ctx,
		log:     log,
		held:    held(dir),
		exiting: func() bool { return true },
		capacity: fakeCapacity{create: func(ctx context.Context, _ string) (source.VM, error) {
			cancel()
			<-ctx.Done()
			return source.VM{}, ctx.Err()
		}},
	}}
	if err := mr.place(common.RunnerConfig{}, n, log); err != nil {
		t.Errorf("place: %v", err)
	}
	if _, err := os.Stat(held(dir).path(8)); err != nil {
		t.Errorf("the note is gone: %v", err)
	}
}

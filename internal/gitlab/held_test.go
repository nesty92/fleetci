package gitlab

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hashicorp/go-hclog"
)

// A job noted as taken and never placed, by a controller killed meanwhile,
// is failed at the next start as a runner system failure, with its own
// token, and its note dropped; a note made after the listing, by the new
// leader, is left alone, and one GitLab can't be told of is kept.
func TestFailHeld(t *testing.T) {
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
	defer srv.Close()

	dir := filepath.Join(t.TempDir(), "held")
	h, log := held(dir), hclog.NewNullLogger()
	h.put(heldJob{ID: 1, Token: "job-token-1", URL: srv.URL}, log)
	h.put(heldJob{ID: 3, Token: "job-token-3", URL: srv.URL}, log)
	fail := FailHeld(dir, log)
	h.put(heldJob{ID: 2, Token: "job-token-2", URL: srv.URL}, log) // the new leader's
	fail()

	mu.Lock()
	defer mu.Unlock()
	b, ok := failed["/api/v4/jobs/1"]
	if !ok || b["token"] != "job-token-1" || b["state"] != "failed" || b["failure_reason"] != "runner_system_failure" {
		t.Errorf("job 1 not failed as a runner system failure: %v", failed)
	}
	if _, ok := failed["/api/v4/jobs/2"]; ok {
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

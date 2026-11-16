package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// shellGitHub answers the calls the shell makes, for repo o/r: job 2 of
// run 70, attempt 2, taken by runner-a and started by actor; perms are
// the users' permissions, keys their SSH keys.
func shellGitHub(t *testing.T, head string, perms map[string]string, keys map[string][]string, conclusion string) *Dispatcher {
	mux := http.NewServeMux()
	j := map[string]any{"id": 2, "run_id": 70, "run_attempt": 2, "runner_name": "runner-a", "status": "completed", "conclusion": conclusion}
	mux.HandleFunc("GET /repos/o/r/actions/jobs/2", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, j) })
	mux.HandleFunc("GET /repos/o/r/actions/jobs/1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": 1, "run_id": 60, "runner_name": "runner-b"})
	})
	mux.HandleFunc("GET /repos/o/r/actions/runs/70/jobs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"jobs": []map[string]any{j}})
	})
	mux.HandleFunc("GET /repos/o/r/actions/runs/70/attempts/2", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"triggering_actor": map[string]any{"login": "actor"}, "head_repository": map[string]any{"full_name": head}})
	})
	mux.HandleFunc("GET /repos/o/r/collaborators/{user}/permission", func(w http.ResponseWriter, r *http.Request) {
		p, ok := perms[r.PathValue("user")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{"permission": p})
	})
	mux.HandleFunc("GET /users/{user}/keys", func(w http.ResponseWriter, r *http.Request) {
		var out []map[string]any
		for _, k := range keys[r.PathValue("user")] {
			out = append(out, map[string]any{"id": 1, "key": k})
		}
		if out == nil {
			out = []map[string]any{}
		}
		writeJSON(w, out)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Dispatcher{
		cfg:   Config{APIURL: srv.URL, Shell: &ShellConfig{}},
		gh:    newClient(srv.URL, staticToken("t"), srv.Client()),
		repos: map[string]bool{"o/r": true},
		orgs:  map[string]bool{},
	}
}

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"

// TestShellFor: only whoever started the attempt, with write access and
// SSH keys, gets into a job's VM, and never a fork's pull request's.
func TestShellFor(t *testing.T) {
	jobLookupWait = time.Millisecond
	t.Cleanup(func() { jobLookupWait = 250 * time.Millisecond })
	ctx := context.Background()
	keys := map[string][]string{"actor": {testKey}}
	for _, tc := range []struct {
		name  string
		head  string
		perms map[string]string
		keys  map[string][]string
		hint  string
		want  string // "" for a shell, else part of the refusal
	}{
		{name: "writer", head: "o/r", perms: map[string]string{"actor": "write"}, keys: keys, hint: "o/r 70 2"},
		{name: "admin", head: "o/r", perms: map[string]string{"actor": "admin"}, keys: keys, hint: "o/r 70 2"},
		{name: "reader", head: "o/r", perms: map[string]string{"actor": "read"}, keys: keys, hint: "o/r 70 2", want: "may not write"},
		{name: "not a collaborator", head: "o/r", perms: nil, keys: keys, hint: "o/r 70 2", want: "may not write"},
		{name: "fork", head: "someone/r", perms: map[string]string{"actor": "admin"}, keys: keys, hint: "o/r 70 2", want: "fork"},
		{name: "no keys", head: "o/r", perms: map[string]string{"actor": "write"}, keys: nil, hint: "o/r 70 2", want: "no SSH keys"},
		{name: "other attempt", head: "o/r", perms: map[string]string{"actor": "write"}, keys: keys, hint: "o/r 70 1", want: "attempt"},
		{name: "no hint", head: "o/r", perms: map[string]string{"actor": "write"}, keys: keys, hint: "", want: "did not say"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := shellGitHub(t, tc.head, tc.perms, tc.keys, "failure")
			repo, j, actor, k, err := d.shellFor(ctx, "o/r", 1, "runner-a", tc.hint)
			if tc.want == "" {
				if err != nil || repo != "o/r" || j.ID != 2 || actor != "actor" || strings.TrimSpace(k) != testKey {
					t.Fatalf("shellFor: %q %+v %q %q %v", repo, j, actor, k, err)
				}
				return
			}
			if !errors.Is(err, errShell) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("shellFor: %v, want a refusal saying %q", err, tc.want)
			}
		})
	}
	// A run in a repo fleetci doesn't serve is not looked at.
	d := shellGitHub(t, "o/r", map[string]string{"actor": "write"}, keys, "failure")
	if _, _, _, _, err := d.shellFor(ctx, "o/r", 1, "runner-a", "evil/r 70 2"); err == nil {
		t.Fatal("a shell for a run fleetci doesn't serve")
	}
}

func TestConclusion(t *testing.T) {
	d := shellGitHub(t, "o/r", nil, nil, "failure")
	if c, err := d.conclusion(context.Background(), "o/r", 2); err != nil || c != "failure" {
		t.Fatalf("conclusion: %q %v", c, err)
	}
}

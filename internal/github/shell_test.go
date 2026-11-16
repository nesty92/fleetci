package github

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"
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

// stuckVM is a VM whose commands never end, even when their session
// closes, as a loop under sshd: the wait for a hook that never asks.
func stuckVM(t *testing.T) string {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(nil)
	signer, _ := ssh.NewSignerFromKey(key)
	cfg := &ssh.ServerConfig{NoClientAuth: true,
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					_, creqs, err := nc.Accept()
					if err != nil {
						continue
					}
					go func() {
						for r := range creqs {
							_ = r.Reply(r.Type == "exec", nil)
						}
					}()
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// TestWatchShellEndsWithItsJob: a job whose hook never asks for a shell
// lets its VM go when the job ends.
func TestWatchShellEndsWithItsJob(t *testing.T) {
	d := shellGitHub(t, "o/r", nil, nil, "failure")
	conn := vmConn(t, stuckVM(t))
	ctx, cancel := context.WithCancel(context.Background())
	a := d.watchShell(ctx, context.Background(), vmFor(conn), "o/r", 1, "runner-a", hclog.NewNullLogger())
	time.Sleep(200 * time.Millisecond)
	cancel()
	done := make(chan struct{})
	go func() { a.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the shell's watch outlived its job")
	}
}

func TestConclusion(t *testing.T) {
	d := shellGitHub(t, "o/r", nil, nil, "failure")
	if c, err := d.conclusion(context.Background(), "o/r", 2); err != nil || c != "failure" {
		t.Fatalf("conclusion: %q %v", c, err)
	}
}

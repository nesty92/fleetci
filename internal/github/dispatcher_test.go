package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/fleet"
)

type fakeBackend struct {
	mu      sync.Mutex
	created []string // classes
	deleted []string // vm ids
}

func (b *fakeBackend) Has(class string) bool { return class == "2vcpu-1gb" || class == "2vcpu-2gb" }

func (b *fakeBackend) Create(_ context.Context, class string) (fleet.VM, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.created = append(b.created, class)
	return fleet.VM{ID: fmt.Sprintf("fleetci-%s-%d", class, len(b.created)), Class: class}, nil
}

func (b *fakeBackend) Delete(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deleted = append(b.deleted, id)
	return nil
}

// fakeGitHub serves one queued run whose jobs ask for various labels.
type fakeGitHub struct {
	t        *testing.T
	polls    atomic.Int32
	mu       sync.Mutex
	jit      map[string][]string // runner name → labels
	removed  []int64
	nextID   int64
	jobsJSON string
}

func (g *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/actions/runs":
		g.polls.Add(1)
		if r.URL.Query().Get("status") == "queued" {
			fmt.Fprint(w, `{"workflow_runs":[{"id":7}]}`)
		} else {
			fmt.Fprint(w, `{"workflow_runs":[]}`)
		}
	case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/actions/runs/7/jobs":
		fmt.Fprint(w, g.jobsJSON)
	case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/actions/runners/generate-jitconfig":
		var body struct {
			Name   string   `json:"name"`
			Group  int64    `json:"runner_group_id"`
			Labels []string `json:"labels"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Group != 1 {
			g.t.Errorf("jitconfig body %+v, %v", body, err)
		}
		g.mu.Lock()
		g.nextID++
		id := g.nextID
		g.jit[body.Name] = body.Labels
		g.mu.Unlock()
		fmt.Fprintf(w, `{"runner":{"id":%d},"encoded_jit_config":"jit-%s"}`, id, body.Name)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/actions/runners/"):
		fmt.Fprint(w, `{"id":1,"busy":true}`)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/repos/o/r/actions/runners/"):
		var id int64
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/repos/o/r/actions/runners/"), "%d", &id)
		g.mu.Lock()
		g.removed = append(g.removed, id)
		g.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		g.t.Errorf("unexpected %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	}
}

func TestDispatcher(t *testing.T) {
	gh := &fakeGitHub{t: t, jit: map[string][]string{}, jobsJSON: `{"jobs":[
	  {"id":1,"name":"build","status":"queued","labels":["fleetci-2vcpu-2gb"]},
	  {"id":2,"name":"lint","status":"queued","labels":["self-hosted","fleetci"]},
	  {"id":3,"name":"hosted","status":"queued","labels":["ubuntu-latest"]},
	  {"id":4,"name":"typo","status":"queued","labels":["fleetci-huge"]},
	  {"id":5,"name":"gpu","status":"queued","labels":["fleetci-2vcpu-2gb","gpu"]},
	  {"id":6,"name":"done","status":"completed","labels":["fleetci"]}
	]}`}
	srv := httptest.NewServer(gh)
	defer srv.Close()

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	backend := &fakeBackend{}
	release := make(chan struct{})
	var mu sync.Mutex
	started := map[string]string{} // vm id → jit config
	start := func(ctx context.Context, vm fleet.VM, jit string) error {
		mu.Lock()
		started[vm.ID] = jit
		mu.Unlock()
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}

	d, err := New(Config{
		Repos:        []string{"o/r"},
		TokenFile:    tokenFile,
		PollInterval: fleet.Duration(20 * time.Millisecond),
		APIURL:       srv.URL,
	}, "2vcpu-1gb", backend, start, hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()

	// Several polls see the same queued jobs while their runners start.
	waitFor(t, func() bool { return gh.polls.Load() >= 10 })
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(started) == 2 })

	backend.mu.Lock()
	created := slices.Sorted(slices.Values(backend.created))
	backend.mu.Unlock()
	if !slices.Equal(created, []string{"2vcpu-1gb", "2vcpu-2gb"}) {
		t.Errorf("created %v; want one 2vcpu-1gb (runs-on: fleetci) and one 2vcpu-2gb", created)
	}

	gh.mu.Lock()
	for name, labels := range gh.jit {
		want := []string{"fleetci-2vcpu-2gb"}
		if strings.Contains(name, "2vcpu-1gb") {
			want = []string{"fleetci", "self-hosted"}
		}
		if !slices.Equal(labels, want) {
			t.Errorf("runner %s labels %v; want %v", name, labels, want)
		}
		if started[name] != "jit-"+name {
			t.Errorf("runner %s started with %q", name, started[name])
		}
	}
	gh.mu.Unlock()

	close(release)
	waitFor(t, func() bool { backend.mu.Lock(); defer backend.mu.Unlock(); return len(backend.deleted) == 2 })
	waitFor(t, func() bool { gh.mu.Lock(); defer gh.mu.Unlock(); return len(gh.removed) == 2 })

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestNewChecksConfig(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tokenFile, []byte("x"), 0o600)
	for name, c := range map[string]Config{
		"no repos":     {TokenFile: tokenFile},
		"bad repo":     {Repos: []string{"just-a-name"}, TokenFile: tokenFile},
		"no token":     {Repos: []string{"o/r"}},
		"missing file": {Repos: []string{"o/r"}, TokenFile: tokenFile + ".missing"},
	} {
		if _, err := New(c, "x", &fakeBackend{}, nil, hclog.NewNullLogger()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

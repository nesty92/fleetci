package github

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/nesty92/fleetci/internal/proxy"
	"github.com/nesty92/fleetci/internal/source"
)

// fakeBackend is a source.Capacity of two classes that creates VMs at once.
type fakeBackend struct {
	defaultClass string

	mu      sync.Mutex
	created []string // classes
	deleted []string // vm ids
	conns   map[string]*proxy.Conn

	createWait time.Duration // before Create answers
	createErr  error
}

func (b *fakeBackend) Classes(context.Context) ([]source.Class, string, error) {
	return []source.Class{{Name: "2vcpu-1gb", Slots: 1}, {Name: "2vcpu-2gb", Slots: 1}}, b.defaultClass, nil
}

func (b *fakeBackend) Reserve(context.Context, string) (source.Reservation, error) {
	return source.Reservation{}, source.ErrNoRoom
}

func (b *fakeBackend) Acquire(context.Context, source.Reservation) (source.VM, error) {
	return source.VM{}, errors.New("no reservations")
}

func (b *fakeBackend) Release(context.Context, source.Reservation) error {
	return errors.New("no reservations")
}

// Conn hands out one connection per vm, never dialed.
func (b *fakeBackend) Conn(id string) (*proxy.Conn, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conns == nil {
		b.conns = map[string]*proxy.Conn{}
	}
	if b.conns[id] == nil {
		b.conns[id] = proxy.NewConn(func(context.Context) (proxy.Target, error) {
			return proxy.Target{}, errors.New("fake vm")
		}, hclog.NewNullLogger())
	}
	return b.conns[id], nil
}

func (b *fakeBackend) Create(_ context.Context, class string) (source.VM, error) {
	time.Sleep(b.createWait)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.createErr != nil {
		return source.VM{}, b.createErr
	}
	b.created = append(b.created, class)
	return source.VM{ID: fmt.Sprintf("fleetci-%s-%d", class, len(b.created)), Class: class}, nil
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
	names    []string            // runner names, as registered
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
		g.names = append(g.names, body.Name)
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

	backend := &fakeBackend{defaultClass: "2vcpu-1gb"}
	release := make(chan struct{})
	var mu sync.Mutex
	started := map[string]string{} // jit config → vm id
	conns := map[string]*proxy.Conn{}
	start := func(ctx context.Context, vm VM, jit string, took func(string)) error {
		mu.Lock()
		started[jit] = vm.ID
		conns[vm.ID] = vm.Conn
		mu.Unlock()
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}

	d, err := New(t.Context(), Config{
		Repos:        []string{"o/r"},
		TokenFile:    tokenFile,
		PollInterval: fleet.Duration(20 * time.Millisecond),
		APIURL:       srv.URL,
	}, backend, start, hclog.NewNullLogger())
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
		vm := started["jit-"+name]
		if vm == "" {
			t.Errorf("runner %s never started", name)
			continue
		}
		if want, _ := backend.Conn(vm); conns[vm] == nil || conns[vm] != want {
			t.Errorf("runner %s started without its vm's connection", name)
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
		"cache scheme": {Repos: []string{"o/r"}, TokenFile: tokenFile, CacheURL: "ftp://cache/"},
		"cache quote":  {Repos: []string{"o/r"}, TokenFile: tokenFile, CacheURL: "http://cache/'$(id)"},
		"cache host":   {Repos: []string{"o/r"}, TokenFile: tokenFile, CacheURL: "http:///x"},
	} {
		if _, err := New(t.Context(), c, &fakeBackend{defaultClass: "x"}, nil, hclog.NewNullLogger()); err == nil {
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

// A runner that fails before taking its job is replaced, up to maxTries.
func TestDispatcherRetriesFailedRunner(t *testing.T) {
	defer func(p time.Duration) { retryPause = p }(retryPause)
	retryPause = 10 * time.Millisecond

	gh := &fakeGitHub{t: t, jit: map[string][]string{}, jobsJSON: `{"jobs":[
	  {"id":1,"name":"build","status":"queued","labels":["fleetci"]}
	]}`}
	srv := httptest.NewServer(gh)
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tokenFile, []byte("test-token"), 0o600)

	var starts atomic.Int32
	start := func(context.Context, VM, string, func(string)) error {
		starts.Add(1)
		return fmt.Errorf("download failed")
	}
	backend := &fakeBackend{defaultClass: "2vcpu-1gb"}
	d, err := New(t.Context(), Config{Repos: []string{"o/r"}, TokenFile: tokenFile, PollInterval: fleet.Duration(5 * time.Millisecond), APIURL: srv.URL},
		backend, start, hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()

	waitFor(t, func() bool { return starts.Load() == maxTries })
	waitFor(t, func() bool { return gh.polls.Load() > 40 })
	cancel()
	<-done
	if n := starts.Load(); n != maxTries {
		t.Errorf("%d runners started; want %d", n, maxTries)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.deleted) != maxTries {
		t.Errorf("%d vms removed; want %d", len(backend.deleted), maxTries)
	}
}

// runOne serves one queued job on backend until wait holds, and returns
// the fake GitHub it talked to.
func runOne(t *testing.T, backend *fakeBackend, start StartFunc, wait func(*fakeGitHub) bool) *fakeGitHub {
	t.Helper()
	gh := &fakeGitHub{t: t, jit: map[string][]string{}, jobsJSON: `{"jobs":[
	  {"id":1,"name":"build","status":"queued","labels":["fleetci"]}
	]}`}
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tokenFile, []byte("test-token"), 0o600)
	d, err := New(t.Context(), Config{Repos: []string{"o/r"}, TokenFile: tokenFile, PollInterval: fleet.Duration(5 * time.Millisecond), APIURL: srv.URL},
		backend, start, hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	waitFor(t, func() bool { gh.mu.Lock(); defer gh.mu.Unlock(); return wait(gh) })
	cancel()
	<-done
	return gh
}

// A runner registered while its VM was taken is removed when no VM came.
func TestDispatcherRemovesRunnerWithoutVM(t *testing.T) {
	backend := &fakeBackend{defaultClass: "2vcpu-1gb", createErr: errors.New("no room")}
	start := func(context.Context, VM, string, func(string)) error {
		t.Error("a runner started without a vm")
		return nil
	}
	gh := runOne(t, backend, start, func(g *fakeGitHub) bool { return g.nextID >= 2 && len(g.removed) >= 2 })
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if int64(len(gh.removed)) < gh.nextID-1 {
		t.Errorf("%d runners registered, %d removed", gh.nextID, len(gh.removed))
	}
}

// A registration older than registrationFresh by the time its VM comes is
// replaced by a new one, and the runner starts on that.
func TestDispatcherRegistersAgainAfterLongWait(t *testing.T) {
	defer func(f time.Duration) { registrationFresh = f }(registrationFresh)
	registrationFresh = 20 * time.Millisecond

	backend := &fakeBackend{defaultClass: "2vcpu-1gb", createWait: 100 * time.Millisecond}
	jits := make(chan string, 4)
	release := make(chan struct{})
	start := func(ctx context.Context, _ VM, jit string, _ func(string)) error {
		jits <- jit
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}
	gh := runOne(t, backend, start, func(g *fakeGitHub) bool { return len(jits) == 1 })
	close(release)
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if gh.nextID != 2 || len(gh.removed) < 1 || gh.removed[0] != 1 {
		t.Fatalf("registered %d, removed %v; want the first removed and a second registered", gh.nextID, gh.removed)
	}
	if jit := <-jits; jit != "jit-"+gh.names[1] {
		t.Errorf("runner started with %q; want the second registration's, %q", jit, "jit-"+gh.names[1])
	}
}

func TestNewCacheURLGetsTrailingSlash(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tokenFile, []byte("x"), 0o600)
	d, err := New(t.Context(), Config{Repos: []string{"o/r"}, TokenFile: tokenFile, CacheURL: "http://10.0.0.5:3000"}, &fakeBackend{defaultClass: "x"}, nil, hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	if d.cfg.CacheURL != "http://10.0.0.5:3000/" {
		t.Errorf("cache_url %q", d.cfg.CacheURL)
	}
}

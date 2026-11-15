package github

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/fleet"
	"github.com/nesty92/fleetci/internal/source"
)

// forge is a fake GitHub that keeps state the way GitHub does: queued jobs
// in runs, registered runners by id and name, and which jobs ran. A runner
// started with one of its jit configs takes the first queued job and is
// then removed, as an ephemeral runner is.
type forge struct {
	t *testing.T

	mu      sync.Mutex
	queued  []job            // jobs waiting, in the order runners get them
	runners map[int64]string // registered: id → name
	nextID  int64
	ran     map[int64]int // job id → runners that took it
	reqs    []request
	// keep leaves a runner registered after its job, as when the runner
	// exits before GitHub removes it.
	keep bool

	// fault, when set, may answer a request itself, and says so. It is
	// called for every request, without mu held.
	fault func(w http.ResponseWriter, r *http.Request) bool

	scopes   []string // where each runner was registered
	orgRepos []map[string]any
	// app, when set, is the GitHub App's public key: its JWT mints
	// installation tokens, which expire after tokenLife.
	app       *rsa.PublicKey
	tokenLife time.Duration
	mints     int
}

type request struct {
	at           time.Time
	method, path string
}

func newForge(t *testing.T, jobs ...job) *forge {
	return &forge{t: t, queued: jobs, runners: map[int64]string{}, ran: map[int64]int{}}
}

func (f *forge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.app != nil && (strings.HasPrefix(r.URL.Path, "/app/") || strings.HasSuffix(r.URL.Path, "/installation")) {
		f.appRoute(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, request{time.Now(), r.Method, r.URL.Path})
	fault := f.fault
	f.mu.Unlock()
	if fault != nil && fault(w, r) {
		return
	}
	f.route(w, r)
}

func (f *forge) route(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	runners := "/repos/o/r/actions/runners"
	path := r.URL.Path
	if strings.HasPrefix(path, "/orgs/o/actions/runners") {
		runners = "/orgs/o/actions/runners"
	}
	switch {
	case r.Method == http.MethodGet && path == "/orgs/o/repos":
		writeJSON(w, f.orgRepos)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/o/r/actions/jobs/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(path, "/repos/o/r/actions/jobs/"), 10, 64)
		for _, j := range f.queued {
			if j.ID == id {
				writeJSON(w, j)
				return
			}
		}
		writeJSON(w, job{ID: id, Status: "completed"})
	case r.Method == http.MethodGet && path == "/repos/o/r/actions/runs":
		// The newest runs, unfiltered: the queued ones and no others.
		var runs []map[string]any
		if r.URL.Query().Get("status") == "" {
			seen := map[int64]bool{}
			for _, j := range f.queued {
				if !seen[j.RunID] {
					seen[j.RunID] = true
					runs = append(runs, map[string]any{"id": j.RunID, "status": "queued"})
				}
			}
		}
		writeJSON(w, map[string]any{"workflow_runs": runs})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/o/r/actions/runs/") && strings.HasSuffix(path, "/jobs"):
		run, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(path, "/repos/o/r/actions/runs/"), "/jobs"), 10, 64)
		var jobs []job
		for _, j := range f.queued {
			if j.RunID == run {
				jobs = append(jobs, j)
			}
		}
		writeJSON(w, map[string]any{"jobs": jobs})
	case r.Method == http.MethodPost && path == runners+"/generate-jitconfig":
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			http.Error(w, "bad body", http.StatusUnprocessableEntity)
			return
		}
		for _, name := range f.runners {
			if name == body.Name {
				http.Error(w, `{"message":"Already exists - A runner with the name already exists."}`, http.StatusConflict)
				return
			}
		}
		f.nextID++
		f.runners[f.nextID] = body.Name
		f.scopes = append(f.scopes, strings.TrimSuffix(runners, "/actions/runners"))
		writeJSON(w, map[string]any{"runner": map[string]int64{"id": f.nextID}, "encoded_jit_config": fmt.Sprintf("jit-%d", f.nextID)})
	case r.Method == http.MethodGet && path == runners:
		var list []map[string]any
		for id, name := range f.runners {
			if q := r.URL.Query().Get("name"); q == "" || q == name {
				list = append(list, map[string]any{"id": id, "name": name, "status": "offline"})
			}
		}
		writeJSON(w, map[string]any{"total_count": len(list), "runners": list})
	case strings.HasPrefix(path, runners+"/"):
		id, err := strconv.ParseInt(strings.TrimPrefix(path, runners+"/"), 10, 64)
		if _, ok := f.runners[id]; err != nil || !ok {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, map[string]any{"id": id, "busy": false})
		case http.MethodDelete:
			delete(f.runners, id)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	}
}

// appRoute answers as GitHub does a GitHub App with a valid JWT: its
// installation on org o, and installation tokens.
func (f *forge) appRoute(w http.ResponseWriter, r *http.Request) {
	jwt, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if err := verifyJWT(jwt, f.app); err != nil {
		http.Error(w, `{"message":"`+err.Error()+`"}`, http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/orgs/o/installation":
		writeJSON(w, map[string]any{"id": 42})
	case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
		f.mu.Lock()
		f.mints++
		f.mu.Unlock()
		writeJSON(w, map[string]any{"token": "test-token", "expires_at": time.Now().Add(f.tokenLife).UTC().Format(time.RFC3339)})
	default:
		http.NotFound(w, r)
	}
}

// verifyJWT checks an App's RS256 JWT as GitHub would: signature, issuer,
// and a lifetime of at most ten minutes that covers now.
func verifyJWT(tok string, pub *rsa.PublicKey) error {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return errors.New("not a jwt")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return errors.New("bad signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	var c struct {
		Iat, Exp int64
		Iss      string
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return err
	}
	now := time.Now().Unix()
	if c.Iss != "7" || c.Iat > now || c.Exp < now || c.Exp-c.Iat > 600 {
		return fmt.Errorf("bad claims %+v", c)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// start is a runner: with the jit config of a registered runner it takes
// the first queued job.
func (f *forge) start(_ context.Context, _ VM, jit string, took func(string)) error {
	id, err := strconv.ParseInt(strings.TrimPrefix(jit, "jit-"), 10, 64)
	if err != nil {
		return fmt.Errorf("bad jit config %q", jit)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.runners[id]; !ok {
		return fmt.Errorf("runner %d is not registered", id)
	}
	if len(f.queued) == 0 {
		return errors.New("no job for the runner")
	}
	j := f.queued[0]
	f.queued = f.queued[1:]
	f.ran[j.ID]++
	if !f.keep {
		delete(f.runners, id)
	}
	took(j.Name)
	return nil
}

// lose acts on the request, then drops the connection before answering:
// GitHub did it, the dispatcher never hears.
func (f *forge) lose(w http.ResponseWriter, r *http.Request) {
	f.route(httptest.NewRecorder(), r)
	hangUp(w)
}

func hangUp(w http.ResponseWriter) {
	if c, _, err := http.NewResponseController(w).Hijack(); err == nil {
		c.Close()
	}
}

// settled says whether every job ran once and no runner is left.
func (f *forge) settled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queued) != 0 || len(f.runners) != 0 {
		return false
	}
	for _, n := range f.ran {
		if n != 1 {
			return false
		}
	}
	return true
}

func (f *forge) String() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fmt.Sprintf("queued %d, ran %v, runners left %v", len(f.queued), f.ran, f.runners)
}

// requestsBetween counts the requests that arrived in [from, to).
func (f *forge) requestsBetween(from, to time.Time) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.reqs {
		if !r.at.Before(from) && r.at.Before(to) {
			n++
		}
	}
	return n
}

// dispatch runs a dispatcher against the forge at url, quick to poll and
// retry, until the returned stop is called.
func dispatch(t *testing.T, f *forge, url string, backend source.Capacity) (d *Dispatcher, stop func()) {
	t.Helper()
	return dispatchWith(t, f, url, backend, func(*Config) {})
}

// dispatchWith is dispatch with the config changed by edit first.
func dispatchWith(t *testing.T, f *forge, url string, backend source.Capacity, edit func(*Config)) (d *Dispatcher, stop func()) {
	t.Helper()
	pause := retryPause
	retryPause = 10 * time.Millisecond
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("test-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Repos: []string{"o/r"}, TokenFile: tokenFile, PollInterval: fleet.Duration(5 * time.Millisecond), APIURL: url}
	edit(&cfg)
	d, err := New(t.Context(), cfg, backend, f.start, failureLog(t))
	if err != nil {
		t.Fatal(err)
	}
	d.gh.http.Timeout = 300 * time.Millisecond
	d.settle = 2 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("Run did not return")
			}
			retryPause = pause
		})
	}
	t.Cleanup(stop)
	return d, stop
}

// failureLog is a logger whose lines are shown only if t fails.
func failureLog(t *testing.T) hclog.Logger {
	var mu sync.Mutex
	var b strings.Builder
	t.Cleanup(func() {
		if t.Failed() {
			mu.Lock()
			defer mu.Unlock()
			t.Logf("the dispatcher's log:\n%s", b.String())
		}
	})
	return hclog.New(&hclog.LoggerOptions{Output: lockedWriter{&mu, &b}, Level: hclog.Debug, TimeFormat: "15:04:05.000"})
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *strings.Builder
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func queuedJob(id, run int64) job {
	return job{ID: id, RunID: run, Name: fmt.Sprintf("job-%d", id), Status: "queued", Labels: []string{"fleetci"}}
}

func eventuallySettled(t *testing.T, f *forge, within time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(within); !f.settled(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("never settled: %v", f)
		}
	}
}

// A rate-limited dispatcher sends nothing until GitHub said it may, then
// carries on; a 403 that is no rate limit stops nothing.
func TestARateLimitedDispatcherWaitsAsTold(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(w http.ResponseWriter)
		quiet  time.Duration // at least this long without a request
	}{
		{"429 with Retry-After", func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		}, 900 * time.Millisecond},
		{"403 with the limit spent", func(w http.ResponseWriter) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Unix()+2, 10))
			w.WriteHeader(http.StatusForbidden)
		}, 900 * time.Millisecond},
		{"403 secondary limit, no headers", func(w http.ResponseWriter) {
			http.Error(w, `{"message":"You have exceeded a secondary rate limit."}`, http.StatusForbidden)
		}, 900 * time.Millisecond}, // a minute, in truth
		{"403 forbidden", func(w http.ResponseWriter) {
			http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newForge(t, queuedJob(1, 7))
			var limited atomic.Pointer[time.Time]
			var once sync.Once
			f.fault = func(w http.ResponseWriter, r *http.Request) bool {
				hit := false
				once.Do(func() { hit = true })
				if hit {
					tc.answer(w)
					now := time.Now()
					limited.Store(&now)
				}
				return hit
			}
			srv := httptest.NewServer(f)
			defer srv.Close()
			dispatch(t, f, srv.URL, &fakeBackend{defaultClass: "2vcpu-1gb"})

			if tc.quiet == 0 {
				eventuallySettled(t, f, 2*time.Second)
				return
			}
			time.Sleep(tc.quiet)
			at := *limited.Load()
			if n := f.requestsBetween(at.Add(time.Millisecond), at.Add(tc.quiet)); n != 0 {
				t.Fatalf("%d requests within %v of being rate limited", n, tc.quiet)
			}
			if tc.name != "403 secondary limit, no headers" {
				eventuallySettled(t, f, 5*time.Second)
			}
		})
	}
}

// A rate limit met halfway through listing the jobs starts no runner for
// the jobs listed before it.
func TestARateLimitMidListStartsNothing(t *testing.T) {
	f := newForge(t, queuedJob(1, 8), queuedJob(2, 7))
	var once sync.Once
	f.fault = func(w http.ResponseWriter, r *http.Request) bool {
		hit := false
		if strings.HasSuffix(r.URL.Path, "/runs/7/jobs") {
			once.Do(func() { hit = true })
		}
		if hit {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		}
		return hit
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	backend := &fakeBackend{defaultClass: "2vcpu-1gb"}
	dispatch(t, f, srv.URL, backend)
	time.Sleep(700 * time.Millisecond)
	backend.mu.Lock()
	created := len(backend.created)
	backend.mu.Unlock()
	if created != 0 {
		t.Fatalf("%d vms created while rate limited", created)
	}
	eventuallySettled(t, f, 5*time.Second)
}

// A runner GitHub registered but whose answer was lost is removed, and the
// job still gets a runner.
func TestALostRegistrationLeavesNoRunnerBehind(t *testing.T) {
	f := newForge(t, queuedJob(1, 7))
	var once sync.Once
	f.fault = func(w http.ResponseWriter, r *http.Request) bool {
		hit := false
		if strings.HasSuffix(r.URL.Path, "/generate-jitconfig") {
			once.Do(func() { hit = true })
		}
		if hit {
			f.lose(w, r)
		}
		return hit
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	dispatch(t, f, srv.URL, &fakeBackend{defaultClass: "2vcpu-1gb"})
	eventuallySettled(t, f, 5*time.Second)
}

// A runner whose removal failed is removed later.
func TestARunnerWhoseRemovalFailedIsRemovedLater(t *testing.T) {
	f := newForge(t, queuedJob(1, 7))
	f.keep = true
	var once sync.Once
	f.fault = func(w http.ResponseWriter, r *http.Request) bool {
		hit := false
		if r.Method == http.MethodDelete {
			once.Do(func() { hit = true })
		}
		if hit {
			http.Error(w, "", http.StatusBadGateway)
		}
		return hit
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	dispatch(t, f, srv.URL, &fakeBackend{defaultClass: "2vcpu-1gb"})
	eventuallySettled(t, f, 5*time.Second)
}

// GitHub failing to register runners for a while does not use up a job's
// tries; GitHub refusing to does.
func TestRegistrationFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		fails   int
		settles bool
	}{
		{"github errors for a while", http.StatusInternalServerError, 2 * maxTries, true},
		{"github refuses", http.StatusForbidden, 1000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newForge(t, queuedJob(1, 7))
			var mu sync.Mutex
			fails := 0
			f.fault = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasSuffix(r.URL.Path, "/generate-jitconfig") {
					return false
				}
				mu.Lock()
				defer mu.Unlock()
				if fails == tc.fails {
					return false
				}
				fails++
				http.Error(w, `{"message":"no"}`, tc.status)
				return true
			}
			srv := httptest.NewServer(f)
			defer srv.Close()
			backend := &fakeBackend{defaultClass: "2vcpu-1gb"}
			dispatch(t, f, srv.URL, backend)
			if tc.settles {
				eventuallySettled(t, f, 5*time.Second)
				return
			}
			time.Sleep(500 * time.Millisecond)
			backend.mu.Lock()
			created := len(backend.created)
			backend.mu.Unlock()
			if created != maxTries {
				t.Fatalf("%d vms created for a job github refuses runners for; want %d", created, maxTries)
			}
		})
	}
}

// A run whose jobs cannot be listed does not hold up the other runs' jobs.
func TestABrokenRunDoesNotHoldUpTheOthers(t *testing.T) {
	f := newForge(t, queuedJob(1, 8), queuedJob(2, 7))
	f.fault = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/runs/8/jobs") {
			http.Error(w, "", http.StatusInternalServerError)
			return true
		}
		return false
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	dispatch(t, f, srv.URL, &fakeBackend{defaultClass: "2vcpu-1gb"})
	// job 1 sits first in the queue, so the one runner takes it: GitHub
	// hands a runner any job with its labels.
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		f.mu.Lock()
		ran := len(f.ran)
		f.mu.Unlock()
		if ran == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no job ran while one run was broken: %v", f)
		}
	}
}

// GitHub hands a runner any queued job with its labels: a job whose runner
// took another one still gets a runner.
func TestAJobWhoseRunnerTookAnotherStillGetsOne(t *testing.T) {
	f := newForge(t, queuedJob(1, 8), queuedJob(2, 7))
	var once sync.Once
	f.fault = func(w http.ResponseWriter, r *http.Request) bool {
		hit := false
		if strings.HasSuffix(r.URL.Path, "/runs/8/jobs") {
			once.Do(func() { hit = true }) // the first poll sees job 2 only
		}
		if hit {
			http.Error(w, "", http.StatusInternalServerError)
		}
		return hit
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	dispatch(t, f, srv.URL, &fakeBackend{defaultClass: "2vcpu-1gb"})
	eventuallySettled(t, f, 5*time.Second)
}

// A registration GitHub carries out only after fleetci gave up on it,
// and after a first look found no such runner, is still removed.
func TestARegistrationDoneLateIsRemovedToo(t *testing.T) {
	f := newForge(t, queuedJob(1, 7))
	var once sync.Once
	late := make(chan struct{})
	f.fault = func(w http.ResponseWriter, r *http.Request) bool {
		hit := false
		if strings.HasSuffix(r.URL.Path, "/generate-jitconfig") {
			once.Do(func() { hit = true })
		}
		if hit {
			time.Sleep(700 * time.Millisecond) // past the client's 300ms
			f.route(w, r)
			close(late)
		}
		return hit
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	dispatch(t, f, srv.URL, &fakeBackend{defaultClass: "2vcpu-1gb"})
	select {
	case <-late:
	case <-time.After(5 * time.Second):
		t.Fatal("the late registration never came")
	}
	eventuallySettled(t, f, 5*time.Second)
}

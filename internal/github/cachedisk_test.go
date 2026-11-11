package github

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nesty92/fleetci/internal/cachedisk"
)

func TestCacheScope(t *testing.T) {
	push := func(branch string, protected bool) cacheFacts {
		return cacheFacts{repo: "o/r", repoID: 9, defaultBranch: "main", event: "push", branch: branch, headRepo: "o/r", protected: protected}
	}
	pr := func(branch, from string) cacheFacts {
		return cacheFacts{repo: "o/r", repoID: 9, defaultBranch: "main", event: "pull_request", branch: branch, headRepo: from}
	}
	tag := cacheFacts{repo: "o/r", repoID: 9, defaultBranch: "main", event: "push", branch: "v1.2.0", headRepo: "o/r", tag: true}
	jobs := map[string]cacheFacts{
		"protected main":   push("main", true),
		"unprotected main": push("main", false),
		"feature":          push("feature", false),
		"pr":               pr("feature", "o/r"),
		// A pull request from a protected branch runs the merge, not it.
		"pr from protected": {repo: "o/r", repoID: 9, defaultBranch: "main", event: "pull_request", branch: "release", headRepo: "o/r", protected: true},
		"fork pr":           pr("main", "someone/r"),
		"tag":               tag,
	}
	for _, tc := range []struct {
		writers []string
		want    []string
	}{
		{nil, []string{"protected main"}}, // the default
		{[]string{"default"}, []string{"protected main", "unprotected main"}},
		{[]string{"tag:v*"}, []string{"tag"}},
		// Never a fork's.
		{[]string{"all"}, []string{"protected main", "unprotected main", "feature", "pr", "pr from protected", "tag"}},
	} {
		var p *cachedisk.Policy
		if tc.writers != nil {
			p = &cachedisk.Policy{Writers: &tc.writers}
		}
		var got []string
		for name, f := range jobs {
			if cacheScope(p, "api.github.com", f).Writer {
				got = append(got, name)
			}
		}
		slices.Sort(got)
		if want := slices.Sorted(slices.Values(tc.want)); !slices.Equal(got, want) {
			t.Errorf("writers %q: %q, want %q", tc.writers, got, want)
		}
	}
	s := cacheScope(nil, "api.github.com", jobs["fork pr"])
	if s.Ref != "someone/r:main" || s.Project != "9" || s.Forge != "github" || !slices.Equal(s.From, []string{"main"}) {
		t.Errorf("fork's scope %+v", s)
	}
}

// cacheGitHub answers the calls the cache disk makes, for repo o/r: job 1,
// queued for runner-a but taken by runner-b, and job 2 of run 70, taken
// by runner-a, on branch "feature/x".
func cacheGitHub(t *testing.T, protected map[string]bool) *Dispatcher {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/actions/jobs/1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": 1, "run_id": 60, "runner_name": "runner-b"})
	})
	mux.HandleFunc("GET /repos/o/r/actions/runs/70/jobs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"jobs": []map[string]any{
			{"id": 3, "run_id": 70, "runner_name": "runner-c"},
			{"id": 2, "run_id": 70, "runner_name": "runner-a"},
		}})
	})
	mux.HandleFunc("GET /repos/o/r/actions/runs/70", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"event": "push", "head_branch": "feature/x", "head_repository": map[string]any{"full_name": "o/r"}})
	})
	mux.HandleFunc("GET /repos/o/r", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": 9, "default_branch": "main"})
	})
	mux.HandleFunc("GET /repos/o/r/branches/", func(w http.ResponseWriter, r *http.Request) {
		b := strings.TrimPrefix(r.URL.Path, "/repos/o/r/branches/")
		p, ok := protected[b]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{"protected": p})
	})
	mux.HandleFunc("GET /repos/o/r/git/ref/tags/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Dispatcher{
		cfg:   Config{APIURL: srv.URL},
		gh:    newClient(srv.URL, staticToken("t"), srv.Client()),
		repos: map[string]bool{"o/r": true},
		orgs:  map[string]bool{},
	}
}

func TestRunnersJob(t *testing.T) {
	d := cacheGitHub(t, nil)
	ctx := context.Background()
	repo, j, err := d.runnersJob(ctx, "o/r", 1, "runner-a", "o/r 70")
	if err != nil || repo != "o/r" || j.ID != 2 || j.RunID != 70 {
		t.Fatalf("runnersJob: %q %+v %v", repo, j, err)
	}
	// The job it was started for, when it took that one.
	if _, j, err := d.runnersJob(ctx, "o/r", 1, "runner-b", ""); err != nil || j.ID != 1 {
		t.Fatalf("its own job: %+v %v", j, err)
	}
	// A hint names only where to look: a repo fleetci doesn't serve isn't.
	jobLookupWait = time.Millisecond
	t.Cleanup(func() { jobLookupWait = time.Second })
	if _, _, err := d.runnersJob(ctx, "o/r", 1, "runner-a", "evil/r 70"); err == nil {
		t.Fatal("found a job in a repo fleetci doesn't serve")
	}
}

func TestCacheFacts(t *testing.T) {
	d := cacheGitHub(t, map[string]bool{"feature/x": true})
	f, err := d.cacheFacts(context.Background(), "o/r", job{ID: 2, RunID: 70})
	if err != nil {
		t.Fatal(err)
	}
	if f.repoID != 9 || f.defaultBranch != "main" || f.branch != "feature/x" || !f.protected || f.tag {
		t.Fatalf("%+v", f)
	}
	// Neither a branch nor a tag any more: not protected.
	d = cacheGitHub(t, nil)
	if f, err = d.cacheFacts(context.Background(), "o/r", job{ID: 2, RunID: 70}); err != nil || f.protected || f.tag {
		t.Fatalf("%+v %v", f, err)
	}
}

// Rules that need no branch facts ask GitHub for none: an app that may not
// read the branch still gets the default branch's disk.
func TestCacheFactsAsksOnlyWhatTheRulesNeed(t *testing.T) {
	d := cacheGitHub(t, nil)
	branches := 0
	d.gh = newClient(d.cfg.APIURL, staticToken("t"), &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/branches/") {
			branches++
			return &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader(`{"message":"Resource not accessible by integration"}`)), Header: http.Header{}, Request: r}, nil
		}
		return http.DefaultTransport.RoundTrip(r)
	})})
	d.cfg.CacheDisk = &cachedisk.Policy{Writers: &[]string{"default"}}
	if _, err := d.cacheFacts(context.Background(), "o/r", job{ID: 2, RunID: 70}); err != nil || branches != 0 {
		t.Fatalf("default: %d branch calls, %v", branches, err)
	}
	d.cfg.CacheDisk = nil // protected
	_, err := d.cacheFacts(context.Background(), "o/r", job{ID: 2, RunID: 70})
	if err == nil || !strings.Contains(err.Error(), "Contents: read") {
		t.Fatalf("protected, refused: %v", err)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRunnersJobSaysWhy(t *testing.T) {
	d := cacheGitHub(t, nil)
	jobLookupWait = time.Millisecond
	t.Cleanup(func() { jobLookupWait = time.Second })
	_, _, err := d.runnersJob(context.Background(), "o/r", 1, "runner-z", "")
	if err == nil || !strings.Contains(err.Error(), "runner-z") || !strings.Contains(err.Error(), "named no run") {
		t.Fatalf("%v", err)
	}
}

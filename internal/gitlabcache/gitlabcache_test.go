package gitlabcache

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"gitlab.com/gitlab-org/gitlab-runner/cache"
	"gitlab.com/gitlab-org/gitlab-runner/cache/cacheconfig"
	"gitlab.com/gitlab-org/gitlab-runner/commands/helpers/archive"
	"gitlab.com/gitlab-org/gitlab-runner/commands/helpers/archive/tarzstd"

	"github.com/nesty92/fleetci/internal/cachebucket"
	"github.com/nesty92/fleetci/internal/gitlabcache/vmcache"
)

// memBucket is a bucket whose links are to its own server, as a real
// bucket's signed links are, ranges and all.
type memBucket struct {
	srv  *httptest.Server
	mu   sync.Mutex
	objs map[string][]byte
}

func newMemBucket(t *testing.T) *memBucket {
	b := &memBucket{objs: map[string][]byte{}}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/o/")
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			b.mu.Lock()
			b.objs[name] = body
			b.mu.Unlock()
		case http.MethodGet:
			b.mu.Lock()
			body, ok := b.objs[name]
			b.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
		}
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *memBucket) List(_ context.Context, prefix string) ([]cachebucket.Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []cachebucket.Object
	for n := range b.objs {
		if strings.HasPrefix(n, prefix) {
			out = append(out, cachebucket.Object{Name: n})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (b *memBucket) Download(_ context.Context, name string, _ time.Duration) (cachebucket.Link, error) {
	return cachebucket.Link{URL: b.srv.URL + "/o/" + name}, nil
}

func (b *memBucket) Upload(ctx context.Context, name string, ttl time.Duration) (cachebucket.Link, error) {
	return b.Download(ctx, name, ttl)
}

// jobVM is one job: its config for the runner, the node's signer, and the
// VM's service in front of it.
type jobVM struct {
	cfg  *cacheconfig.Config
	vm   *httptest.Server
	node *httptest.Server
}

func newJob(t *testing.T, b *memBucket, runnerCfg *cacheconfig.Config) *jobVM {
	j := &jobVM{}
	// The VM's service's address, before it listens: the runner's config
	// names it.
	vm := httptest.NewUnstartedServer(nil)
	cfg, grants, done := ForJob(runnerCfg, "http://"+vm.Listener.Addr().String())
	t.Cleanup(done)
	j.cfg = cfg
	j.node = httptest.NewServer(&Signer{B: b, J: grants, Log: hclog.NewNullLogger()})
	t.Cleanup(j.node.Close)
	vm.Config.Handler = &vmcache.VM{Node: j.node.URL, Log: log.New(os.Stderr, "vm ", 0)}
	vm.Start()
	t.Cleanup(vm.Close)
	j.vm = vm
	return j
}

// links are the runner's links for the job's key, as it puts them in the
// job's script.
func (j *jobVM) links(key string) (down, up string) {
	a := cache.GetAdapter(j.cfg, time.Hour, "short", "42", key, false)
	return a.GetDownloadURL(context.Background()).URL.String(), a.GetUploadURL(context.Background()).URL.String()
}

func (j *jobVM) downOnly(key string) string {
	a := cache.GetAdapter(j.cfg, time.Hour, "short", "42", key, false)
	return a.GetDownloadURL(context.Background()).URL.String()
}

func archiveOf(t *testing.T, dir string, paths ...string) []byte {
	t.Helper()
	files := map[string]os.FileInfo{}
	for _, p := range paths {
		_ = filepath.Walk(filepath.Join(dir, p), func(f string, fi os.FileInfo, err error) error {
			if err == nil {
				files[f] = fi
			}
			return err
		})
	}
	var buf bytes.Buffer
	a, _ := tarzstd.NewArchiver(&buf, dir, archive.FastestCompression)
	if err := a.Archive(context.Background(), files); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func put(t *testing.T, url string, body []byte) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func get(t *testing.T, method, url string) (int, []byte, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

func TestAJobsCacheGoesThroughTheStoreAndBack(t *testing.T) {
	b := newMemBucket(t)
	runner := &cacheconfig.Config{Type: "s3", Path: "ci/caches"}
	project := t.TempDir()
	writeTree(t, project, map[string]string{
		"node_modules/a/index.js": strings.Repeat("a", 5000),
		"node_modules/b/index.js": "b",
		"vendor/x.go":             "package x",
	})

	writer := newJob(t, b, runner)
	down, up := writer.links("deps-protected")
	if code, _, _ := get(t, http.MethodGet, down); code != http.StatusNotFound {
		t.Fatalf("a key never saved: %d, want 404", code)
	}
	if code := put(t, up, archiveOf(t, project, "node_modules", "vendor")); code != http.StatusOK {
		t.Fatalf("save: %d", code)
	}
	b.mu.Lock()
	for n := range b.objs {
		if !strings.HasPrefix(n, "runner/short/project/42/fleetci-files/protected/") {
			t.Errorf("object %q outside the project's protected store", n)
		}
	}
	b.mu.Unlock()

	// A new job, on a new VM, reads it.
	reader := newJob(t, b, runner)
	code, _, h := get(t, http.MethodHead, reader.downOnly("deps-protected"))
	if code != http.StatusOK || h.Get("Last-Modified") == "" {
		t.Fatalf("head: %d %q", code, h.Get("Last-Modified"))
	}
	code, body, _ := get(t, http.MethodGet, reader.downOnly("deps-protected"))
	if code != http.StatusOK {
		t.Fatalf("restore: %d %s", code, body)
	}
	f := filepath.Join(t.TempDir(), "cache.zip") // the helper's name for it
	_ = os.WriteFile(f, body, 0o644)
	fh, _ := os.Open(f)
	defer fh.Close()
	dst := t.TempDir()
	x, _ := tarzstd.NewExtractor(fh, int64(len(body)), dst)
	if err := x.Extract(context.Background()); err != nil {
		t.Fatal(err)
	}
	want, got := readTree(t, project), readTree(t, dst)
	if fmt.Sprint(want) != fmt.Sprint(got) {
		t.Fatalf("restored %v, want %v", got, want)
	}
}

func TestAJobWritesOnlyTheKeysItWasGiven(t *testing.T) {
	b := newMemBucket(t)
	runner := &cacheconfig.Config{Type: "s3"}
	project := t.TempDir()
	writeTree(t, project, map[string]string{"c/f": "x"})
	tarzst := archiveOf(t, project, "c")

	// A job given only a download link for the protected key, as a
	// pull policy or an unprotected ref gets.
	j := newJob(t, b, runner)
	down := j.downOnly("deps-protected")
	if code := put(t, down, tarzst); code == http.StatusOK {
		t.Fatal("saved a key the job may only read")
	}
	// Its own key, in the other store, it may write; and not, through it,
	// another key's snapshot.
	_, up := j.links("deps-non_protected")
	if code := put(t, up, tarzst); code != http.StatusOK {
		t.Fatalf("its own key: %d", code)
	}
	signer := &Signer{B: b, J: jobOf(t, j), Log: hclog.NewNullLogger()}
	for _, name := range []string{
		"runner/short/project/42/fleetci-files/other/snaps/main-non_protected",
		"runner/short/project/42/fleetci-files/protected/packs/x",
		"runner/short/project/43/fleetci-files/other/index/x",
		"runner/short/project/42/fleetci-files/other/../protected/index/x",
	} {
		rec := httptest.NewRecorder()
		signer.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sign", strings.NewReader(`{"op":"put","name":"`+name+`"}`)))
		if rec.Code != http.StatusForbidden {
			t.Errorf("put %s: %d, want 403", name, rec.Code)
		}
	}
	// A zip, as the runner's default format, is refused, not misread.
	if code := put(t, up, []byte("PK\x03\x04zip")); code != http.StatusUnsupportedMediaType {
		t.Fatalf("a zip: %d", code)
	}
}

// jobOf is the grants behind j's config.
func jobOf(t *testing.T, j *jobVM) *Job {
	first, _, _ := strings.Cut(j.cfg.Path, "/")
	v, ok := jobs.Load(strings.TrimPrefix(first, jobMark))
	if !ok {
		t.Fatal("no job")
	}
	return v.(*Job)
}

func TestStoreOfANameIsItsProjectsAndClass(t *testing.T) {
	for name, want := range map[string]string{
		"runner/s/project/1/k-protected":      "runner/s/project/1/fleetci-files/protected k-protected",
		"project/1/ab/k-non_protected":        "project/1/fleetci-files/other k-non_protected",
		"runner/s/project/1/k":                "runner/s/project/1/fleetci-files/other k",
		"runner/s/project/1/../2/k-protected": "",
		"runner/s/k":                          "",
	} {
		p, k, err := store(name)
		got := p + " " + k
		if err != nil {
			got = ""
		}
		if got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(root, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(root, p)
			out[rel] = string(b)
		}
		return nil
	})
	return out
}

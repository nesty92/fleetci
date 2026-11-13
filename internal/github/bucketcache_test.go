package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/cachebucket"
)

// fakeBucket keeps objects in memory and serves its own signed links.
type fakeBucket struct {
	srv *httptest.Server

	mu      sync.Mutex
	objects map[string][]byte
	times   map[string]time.Time
	lists   int
	gets    int
	// refusePuts is how many uploads to refuse next, as a bucket that
	// fails does.
	refusePuts int
}

func newFakeBucket(t *testing.T) *fakeBucket {
	b := &fakeBucket{objects: map[string][]byte{}, times: map[string]time.Time{}}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.EscapedPath(), "/o/")
		if r.URL.Query().Get("sig") != "ok" || r.Header.Get("Authorization") != "" {
			http.Error(w, "bad signature", http.StatusForbidden)
			return
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			data, ok := b.objects[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			b.gets++
			http.ServeContent(w, r, name, b.times[name], bytes.NewReader(data))
		case http.MethodPut:
			if b.refusePuts > 0 {
				b.refusePuts--
				http.Error(w, "slow down", http.StatusServiceUnavailable)
				return
			}
			if r.Header.Get("Content-Type") != "application/octet-stream" {
				http.Error(w, "unsigned content type", http.StatusForbidden)
				return
			}
			data, _ := io.ReadAll(r.Body)
			b.objects[name] = data
			b.times[name] = time.Now()
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *fakeBucket) List(_ context.Context, prefix string) ([]cachebucket.Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lists++
	var out []cachebucket.Object
	for n, d := range b.objects {
		if strings.HasPrefix(n, prefix) {
			out = append(out, cachebucket.Object{Name: n, Size: int64(len(d)), Modified: b.times[n]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (b *fakeBucket) Download(_ context.Context, name string, _ time.Duration) (cachebucket.Link, error) {
	return cachebucket.Link{URL: b.srv.URL + "/o/" + name + "?sig=ok"}, nil
}

func (b *fakeBucket) Upload(_ context.Context, name string, _ time.Duration) (cachebucket.Link, error) {
	return cachebucket.Link{URL: b.srv.URL + "/o/" + name + "?sig=ok", Headers: map[string]string{"Content-Type": "application/octet-stream"}}, nil
}

func (b *fakeBucket) names() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var n []string
	for k := range b.objects {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

func (b *fakeBucket) put(name string, data []byte, at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[name] = data
	b.times[name] = at
}

// bucketNode is the node's cache service, as a VM reaches it.
func bucketNode(t *testing.T, b cachebucket.Bucket, issuer string) string {
	c := &bucketCache{bucket: b, tokens: newTokenVerifier(issuer), log: hclog.NewNullLogger()}
	srv := httptest.NewServer(c.Handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

func flush(t *testing.T, l1 string) map[string]float64 {
	t.Helper()
	code, out := do(t, http.MethodPost, l1+"/fleetci/flush", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("flush: %d %s", code, out)
	}
	var m map[string]float64
	_ = json.Unmarshal(out, &m)
	return m
}

func restoreData(t *testing.T, base, token string, keys ...string) (string, []byte, string) {
	t.Helper()
	code, m := call(t, base, token, "GetCacheEntryDownloadURL", map[string]any{"key": keys[0], "restore_keys": keys[1:], "version": "v1"})
	if code != http.StatusOK || m["ok"] != true {
		return "", nil, ""
	}
	url := m["signed_download_url"].(string)
	code, data := do(t, http.MethodGet, url, nil, map[string]string{"Authorization": ""})
	if code != http.StatusOK {
		t.Fatalf("download %s: %d %s", url, code, data)
	}
	return m["matched_key"].(string), data, url
}

// TestL1CacheBucket: saves go to the disk and, after the job, to the
// bucket; another VM's disk restores them from the bucket and then from
// itself; the node gives a job only its own token's scopes.
func TestL1CacheBucket(t *testing.T) {
	is := newTestIssuer(t, true)
	b := newFakeBucket(t)
	node := bucketNode(t, b, is.srv.URL)
	l2 := newFakeL2(t)
	bare := func(tok string) string { return strings.TrimPrefix(tok, "Bearer ") }
	onMain := bare(is.token(t, nil))
	onFeature := bare(is.token(t, map[string]any{"ac": `[{"Scope":"refs/heads/feature","Permission":3},{"Scope":"refs/heads/main","Permission":1}]`}))
	big := bytes.Repeat([]byte("fleetci-"), 2_000_000) // 16 MB, fetched in ranges

	// The first VM, on main: the save waits for no upload.
	vm1 := startL1(t, l2.srv.URL+"/", filepath.Join(t.TempDir(), "actions"), true, "remote=bucket", "node="+node)
	if code := saveAs(t, vm1, onMain, "go-linux-aaa", big); code != http.StatusOK {
		t.Fatalf("save: %d", code)
	}
	if len(b.names()) != 0 {
		t.Fatal("the save went to the bucket during the job")
	}
	if code, out := do(t, http.MethodGet, vm1+"/fleetci/stats", nil, nil); code != 200 || !bytes.Contains(out, []byte("1 to upload to the cache bucket after the job")) {
		t.Errorf("stats: %s", out)
	}
	if got := flush(t, vm1); got["uploaded"] != 1 || got["bytes"] != float64(len(big)) {
		t.Fatalf("flush: %v", got)
	}
	want := "github/1/refs%2Fheads%2Fmain/v1/go-linux-aaa"
	if n := b.names(); len(n) != 1 || n[0] != want {
		t.Fatalf("the bucket has %v, want %s", n, want)
	}
	if got := flush(t, vm1); got["uploaded"] != 0 {
		t.Errorf("a second flush uploaded again: %v", got)
	}

	// Another VM's disk, on a feature branch, which reads main's caches.
	vm2 := startL1(t, l2.srv.URL+"/", filepath.Join(t.TempDir(), "actions"), true, "remote=bucket", "node="+node)
	key, data, url := restoreData(t, vm2, onFeature, "go-linux-bbb", "go-linux-")
	if key != "go-linux-aaa" || !bytes.Equal(data, big) || !strings.Contains(url, "/fleetci/fill/") {
		t.Fatalf("restore from the bucket: %q, %d bytes, %s", key, len(data), url)
	}
	gets := b.gets
	key, data, url = restoreData(t, vm2, onFeature, "go-linux-aaa")
	if key != "go-linux-aaa" || !bytes.Equal(data, big) || !strings.Contains(url, "/fleetci/l1/") || b.gets != gets {
		t.Fatalf("restore again: %q, %d bytes, %s, %d more bucket reads", key, len(data), url, b.gets-gets)
	}

	// The feature branch saves the same key in its own scope; main's stays.
	if code := saveAs(t, vm2, onFeature, "go-linux-aaa", []byte("feature's")); code != http.StatusOK {
		t.Fatalf("feature's save: %d", code)
	}
	if got := flush(t, vm2); got["uploaded"] != 1 {
		t.Fatalf("feature's flush: %v", got)
	}
	if n := b.names(); len(n) != 2 || n[0] != "github/1/refs%2Fheads%2Ffeature/v1/go-linux-aaa" {
		t.Fatalf("the bucket has %v", n)
	}

	// A save already in the bucket isn't uploaded again.
	vm3 := startL1(t, l2.srv.URL+"/", filepath.Join(t.TempDir(), "actions"), true, "remote=bucket", "node="+node)
	if code := saveAs(t, vm3, onMain, "go-linux-aaa", []byte("again")); code != http.StatusOK {
		t.Fatalf("save on another disk: %d", code)
	}
	if got := flush(t, vm3); got["exists"] != 1 || got["uploaded"] != 0 {
		t.Errorf("flush of a saved entry: %v", got)
	}

	// A token the issuer didn't sign gets nothing from the bucket.
	forged := newTestIssuer(t, true)
	tok := bare(forged.token(t, map[string]any{"iss": is.srv.URL}))
	vm4 := startL1(t, l2.srv.URL+"/", filepath.Join(t.TempDir(), "actions"), true, "remote=bucket", "node="+node)
	if key, _, _ := restoreData(t, vm4, tok, "go-linux-aaa"); key != "" {
		t.Errorf("a forged token restored %q", key)
	}
	if l2.gets.Load() != 0 || len(l2.entries) != 0 || len(l2.pending) != 0 {
		t.Error("GitHub's cache got cache calls")
	}
}

// TestBucketMatching: GitHub's choice among a bucket's entries, after
// falcondev-oss/github-actions-cache-server's matching tests.
func TestBucketMatching(t *testing.T) {
	is := newTestIssuer(t, true)
	b := newFakeBucket(t)
	node := bucketNode(t, b, is.srv.URL)
	main := entryDir("1", "refs/heads/main", "v1")
	feature := entryDir("1", "refs/heads/feature", "v1")
	t0 := time.Now()
	for i, n := range []string{
		main + "primary-old", main + "primary-new", main + "restore", main + "restore-newer",
		feature + "primary",
		main + escapeName("CaseFoo-1"), main + escapeName("Café-1"), main + escapeName("Pad-1"),
		main + escapeName(`wild-%!_\-1`), main + escapeName(`wild-%!_\-2`), main + "wild-xy-1",
		entryDir("1", "refs/heads/main2", "v1") + "only-in-main2",
		entryDir("2", "refs/heads/main", "v1") + "other-repo",
		entryDir("1", "refs/heads/main", "v2") + "other-version",
	} {
		b.put(n, []byte(n), t0.Add(time.Duration(i)*time.Second))
	}
	lookup := func(tok string, keys ...string) string {
		body, _ := json.Marshal(lookupReq{Key: keys[0], RestoreKeys: keys[1:], Version: "v1"})
		req, _ := http.NewRequest(http.MethodPost, node+"/lookup", bytes.NewReader(body))
		req.Header.Set("Authorization", tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		if m["ok"] != true {
			return ""
		}
		return m["matched_key"].(string)
	}
	onMain := is.token(t, nil)
	onFeature := is.token(t, map[string]any{"ac": `[{"Scope":"refs/heads/feature","Permission":3},{"Scope":"refs/heads/main","Permission":1}]`})
	featureLast := is.token(t, map[string]any{"ac": `[{"Scope":"refs/heads/main","Permission":3},{"Scope":"refs/heads/feature","Permission":1}]`})
	for _, c := range []struct {
		name, token string
		keys        []string
		want        string
	}{
		{"the first scope wins", onFeature, []string{"primary", "restore"}, "primary"},
		{"the newest prefix of the key beats an exact restore key", featureLast, []string{"primary", "restore"}, "primary-new"},
		{"an exact restore key beats a newer entry it prefixes", onMain, []string{"missing", "restore"}, "restore"},
		{"a later restore key by prefix", onMain, []string{"missing", "absent", "restore-"}, "restore-newer"},
		{"nothing matches", featureLast, []string{"missing", "absent"}, ""},
		{"case", onMain, []string{"casefoo-1"}, ""},
		{"case in a restore key", onMain, []string{"nomatch", "casefoo-"}, ""},
		{"accents", onMain, []string{"Cafe-1"}, ""},
		{"a trailing space", onMain, []string{"Pad-1 "}, ""},
		{"an exact key with accents", onMain, []string{"Café-1"}, "Café-1"},
		{"% is literal", onMain, []string{"missing", "wild-%y"}, ""},
		{"% matches itself", onMain, []string{"missing", "wild-%"}, `wild-%!_\-2`},
		{"_ is literal", onMain, []string{"missing", "wild-_y"}, ""},
		{`\ is literal`, onMain, []string{"missing", `wild-x\`}, ""},
		{"a literal prefix", onMain, []string{"missing", `wild-%!_\-`}, `wild-%!_\-2`},
		{"a sibling scope isn't read", onMain, []string{"only-in-main2"}, ""},
		{"another repo isn't read", onMain, []string{"other-repo"}, ""},
		{"another version isn't read", onMain, []string{"other-version"}, ""},
	} {
		if got := lookup(c.token, c.keys...); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}

	// A token that may only read gets no upload link.
	readOnly := is.token(t, map[string]any{"ac": `[{"Scope":"refs/heads/main","Permission":1}]`})
	body, _ := json.Marshal(lookupReq{Key: "k", Version: "v1"})
	req, _ := http.NewRequest(http.MethodPost, node+"/upload", bytes.NewReader(body))
	req.Header.Set("Authorization", readOnly)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a read-only token's upload: %d", resp.StatusCode)
	}
}

func TestEscapeName(t *testing.T) {
	for _, s := range []string{"", "a/b", "refs/heads/main", "Café %25 \x00 ~._-", `wild-%!_\-`} {
		e := escapeName(s)
		if strings.Contains(e, "/") {
			t.Errorf("%q: %q has a slash", s, e)
		}
		if back, ok := unescapeName(e); !ok || back != s {
			t.Errorf("%q: back %q %v", s, back, ok)
		}
		if p := escapeName(s + "x"); !strings.HasPrefix(p, e) {
			t.Errorf("%q: escaping isn't prefix-preserving", s)
		}
	}
	if _, ok := unescapeName("a%2"); ok {
		t.Error("a short escape decoded")
	}
	_ = fmt.Sprint()
}

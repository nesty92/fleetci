package github

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeL2 is a cache server as actions/cache sees GitHub's: the twirp calls
// and Azure-style blob uploads and downloads.
type fakeL2 struct {
	srv     *httptest.Server
	mu      sync.Mutex
	entries map[string][]byte // key + "\n" + version
	saved   map[string]time.Time
	pending map[string][]byte // blob name
	blocks  map[string][]byte // blob name + "\n" + block id
	gets    atomic.Int32
	ranges  atomic.Int32 // ranged GETs past the first byte's
	down    atomic.Bool
}

func newFakeL2(t *testing.T) *fakeL2 {
	f := &fakeL2{entries: map[string][]byte{}, saved: map[string]time.Time{}, pending: map[string][]byte{}, blocks: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeL2) put(key, version string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[key+"\n"+version] = data
	f.saved[key+"\n"+version] = time.Now()
}

func (f *fakeL2) serve(w http.ResponseWriter, r *http.Request) {
	if f.down.Load() {
		hj, _ := w.(http.Hijacker)
		c, _, _ := hj.Hijack()
		c.Close()
		return
	}
	// Blob URLs are signed, as Azure's: only the twirp calls take the token.
	if !strings.HasPrefix(r.URL.Path, "/blob/") && r.Header.Get("Authorization") != "Bearer tok" {
		http.Error(w, "no token", http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.URL.Path == svcPath+"GetCacheEntryDownloadURL":
		var req struct {
			Key, Version string
			RestoreKeys  []string `json:"restore_keys"`
		}
		_ = json.Unmarshal(body, &req)
		match := ""
		if _, ok := f.entries[req.Key+"\n"+req.Version]; ok {
			match = req.Key
		}
		for _, rk := range req.RestoreKeys {
			if match != "" {
				break
			}
			var best time.Time
			for k := range f.entries {
				key, v, _ := strings.Cut(k, "\n")
				if v == req.Version && strings.HasPrefix(key, rk) && f.saved[k].After(best) {
					match, best = key, f.saved[k]
				}
			}
		}
		if match == "" {
			reply(map[string]any{"ok": false})
			return
		}
		reply(map[string]any{"ok": true, "matched_key": match, "signed_download_url": f.srv.URL + "/blob/" + base64.RawURLEncoding.EncodeToString([]byte(match+"\n"+req.Version)) + "?sig=dl"})
	case r.URL.Path == svcPath+"CreateCacheEntry":
		var req struct{ Key, Version string }
		_ = json.Unmarshal(body, &req)
		if _, ok := f.entries[req.Key+"\n"+req.Version]; ok {
			reply(map[string]any{"ok": false, "message": "exists"})
			return
		}
		reply(map[string]any{"ok": true, "signed_upload_url": f.srv.URL + "/blob/" + base64.RawURLEncoding.EncodeToString([]byte(req.Key+"\n"+req.Version)) + "?sig=ul"})
	case r.URL.Path == svcPath+"FinalizeCacheEntryUpload":
		var req struct {
			Key, Version string
			Size         string `json:"size_bytes"`
		}
		_ = json.Unmarshal(body, &req)
		k := req.Key + "\n" + req.Version
		data, ok := f.pending[k]
		if !ok || fmt.Sprint(len(data)) != req.Size {
			reply(map[string]any{"ok": false, "message": "no upload"})
			return
		}
		f.entries[k], f.saved[k] = data, time.Now()
		reply(map[string]any{"ok": true, "entry_id": "7"})
	case strings.HasPrefix(r.URL.Path, "/blob/"):
		name, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(r.URL.Path, "/blob/"))
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodGet && q.Get("sig") == "dl":
			// One per download: a whole GET, or the first of its ranges.
			if rg := r.Header.Get("Range"); rg == "" || rg == "bytes=0-0" {
				f.gets.Add(1)
			} else {
				f.ranges.Add(1)
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(f.entries[string(name)]))
		case r.Method == http.MethodPut && q.Get("sig") == "ul" && q.Get("comp") == "block":
			f.blocks[string(name)+"\n"+q.Get("blockid")] = body
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPut && q.Get("sig") == "ul" && q.Get("comp") == "blocklist":
			var all []byte
			for _, m := range regexp.MustCompile(`<Latest>([^<]*)</Latest>`).FindAllStringSubmatch(string(body), -1) {
				all = append(all, f.blocks[string(name)+"\n"+m[1]]...)
			}
			f.pending[string(name)] = all
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPut && q.Get("sig") == "ul" && r.Header.Get("x-ms-blob-type") == "BlockBlob":
			f.pending[string(name)] = body
			w.WriteHeader(http.StatusCreated)
		default:
			http.Error(w, "bad blob request", http.StatusBadRequest)
		}
	case r.URL.Path == "/twirp/github.actions.results.api.v1.ArtifactService/Other":
		reply(map[string]any{"artifact": string(body)})
	default:
		http.NotFound(w, r)
	}
}

const svcPath = "/twirp/github.actions.results.api.v1.CacheService/"

// startL1 runs l1cache.py in front of l2, keeping archives in store.
func startL1(t *testing.T, l2, store string, anyStore bool) string {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "l1cache.py")
	if err := os.WriteFile(script, []byte(l1CacheScript), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	args := []string{"-I", script, fmt.Sprint(port), l2, store}
	if anyStore {
		args = append(args, "any")
	}
	cmd := exec.Command(py, args...)
	var logs bytes.Buffer
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("l1cache.py:\n%s", logs.String())
		}
	})
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	for deadline := time.Now().Add(10 * time.Second); ; {
		if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			c.Close()
			return url
		}
		if time.Now().After(deadline) {
			t.Fatalf("l1cache.py did not start: %s", logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// do sends a request as the job's actions/cache would.
func do(t *testing.T, method, url string, body []byte, hdr map[string]string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func twirp(t *testing.T, base, method string, req any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(req)
	code, out := do(t, http.MethodPost, base+svcPath+method, b, map[string]string{"Content-Type": "application/json"})
	if code != http.StatusOK {
		t.Fatalf("%s: %d %s", method, code, out)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("%s: %s", method, out)
	}
	return m
}

// restore is actions/cache's restore: the URL, then the archive from it.
func restore(t *testing.T, base, key string, rks []string) (string, []byte, string) {
	t.Helper()
	m := twirp(t, base, "GetCacheEntryDownloadURL", map[string]any{"key": key, "restore_keys": rks, "version": "v1"})
	if m["ok"] != true {
		return "", nil, ""
	}
	url := m["signed_download_url"].(string)
	code, data := do(t, http.MethodGet, url, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("download %s: %d %s", url, code, data)
	}
	return m["matched_key"].(string), data, url
}

// save is actions/cache's save; blocks > 0 uploads in that many blocks.
func save(t *testing.T, base, key string, data []byte, blocks int) {
	t.Helper()
	m := twirp(t, base, "CreateCacheEntry", map[string]any{"key": key, "version": "v1"})
	if m["ok"] != true {
		t.Fatalf("create: %v", m)
	}
	url := m["signed_upload_url"].(string)
	if blocks == 0 {
		if code, out := do(t, http.MethodPut, url, data, map[string]string{"x-ms-blob-type": "BlockBlob"}); code != http.StatusCreated {
			t.Fatalf("put: %d %s", code, out)
		}
	} else {
		var list strings.Builder
		list.WriteString(`<?xml version="1.0" encoding="utf-8"?><BlockList>`)
		n := (len(data) + blocks - 1) / blocks
		for i := 0; i*n < len(data); i++ {
			id := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("block-%06d", i)))
			part := data[i*n : min(len(data), (i+1)*n)]
			if code, out := do(t, http.MethodPut, withQuery(url, "comp=block&blockid="+strings.ReplaceAll(id, "=", "%3D")), part, nil); code != http.StatusCreated {
				t.Fatalf("block: %d %s", code, out)
			}
			list.WriteString("<Latest>" + id + "</Latest>")
		}
		list.WriteString("</BlockList>")
		if code, out := do(t, http.MethodPut, withQuery(url, "comp=blocklist"), []byte(list.String()), nil); code != http.StatusCreated {
			t.Fatalf("blocklist: %d %s", code, out)
		}
	}
	m = twirp(t, base, "FinalizeCacheEntryUpload", map[string]any{"key": key, "version": "v1", "size_bytes": fmt.Sprint(len(data))})
	if m["ok"] != true {
		t.Fatalf("finalize: %v", m)
	}
}

func TestL1Cache(t *testing.T) {
	l2 := newFakeL2(t)
	store := filepath.Join(t.TempDir(), "actions")
	l1 := startL1(t, l2.srv.URL+"/", store, true)
	big := bytes.Repeat([]byte("0123456789abcdef"), 300_000) // 4.8 MB

	// Saved through the L1: in the L2 and on the disk.
	save(t, l1, "go-linux-aaa", big, 0)
	if !bytes.Equal(l2.entries["go-linux-aaa\nv1"], big) {
		t.Fatal("the L2 lacks the saved archive")
	}
	key, data, url := restore(t, l1, "go-linux-aaa", nil)
	if key != "go-linux-aaa" || !bytes.Equal(data, big) || !strings.Contains(url, "/fleetci/l1/") || l2.gets.Load() != 0 {
		t.Fatalf("restore after save: key %q, %d bytes, url %s, %d L2 downloads", key, len(data), url, l2.gets.Load())
	}

	// Saved in blocks, as Azure's SDK does past 128 MiB.
	save(t, l1, "go-linux-bbb", big[:1_000_001], 3)
	if _, data, url := restore(t, l1, "go-linux-bbb", nil); !bytes.Equal(data, big[:1_000_001]) || !strings.Contains(url, "/fleetci/l1/") {
		t.Fatalf("blocks: %d bytes from %s", len(data), url)
	}

	// Saved by another node: downloaded once, from the disk after.
	other := []byte("from another node")
	l2.put("node-ccc", "v1", other)
	if key, data, url := restore(t, l1, "node-x", []string{"node-"}); key != "node-ccc" || !bytes.Equal(data, other) || !strings.Contains(url, "/fleetci/fill/") {
		t.Fatalf("first restore: %q %q %s", key, data, url)
	}
	if _, data, url := restore(t, l1, "node-ccc", nil); !bytes.Equal(data, other) || !strings.Contains(url, "/fleetci/l1/") || l2.gets.Load() != 1 {
		t.Fatalf("second restore: %q %s, %d L2 downloads", data, url, l2.gets.Load())
	}

	// A miss stays a miss; a second save of a key is refused by the L2.
	if key, _, _ := restore(t, l1, "nothing", nil); key != "" {
		t.Fatalf("miss restored %q", key)
	}
	if m := twirp(t, l1, "CreateCacheEntry", map[string]any{"key": "go-linux-aaa", "version": "v1"}); m["ok"] != false {
		t.Fatalf("a second save was allowed: %v", m)
	}

	// Other calls go to the L2 as they were.
	if code, out := do(t, http.MethodPost, l1+"/twirp/github.actions.results.api.v1.ArtifactService/Other", []byte("x"), nil); code != 200 || !strings.Contains(string(out), `"artifact":"x"`) {
		t.Fatalf("passthrough: %d %s", code, out)
	}
	// Without the job's token the L2 says no, through the L1 too.
	if code, _ := do(t, http.MethodPost, l1+svcPath+"GetCacheEntryDownloadURL", []byte(`{"key":"go-linux-aaa","version":"v1"}`), map[string]string{"Authorization": "Bearer other"}); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}

	// The L2 out of reach: the disk restores what it has.
	l2.down.Store(true)
	if key, data, _ := restore(t, l1, "go-linux-zzz", []string{"go-linux-"}); key != "go-linux-bbb" || !bytes.Equal(data, big[:1_000_001]) {
		t.Fatalf("L2 down: restored %q (%d bytes)", key, len(data))
	}
	code, _ := do(t, http.MethodPost, l1+svcPath+"GetCacheEntryDownloadURL", []byte(`{"key":"none","version":"v1"}`), nil)
	if code != http.StatusBadGateway {
		t.Fatalf("L2 down, nothing on the disk: %d", code)
	}
}

func TestL1CacheWithoutDisk(t *testing.T) {
	l2 := newFakeL2(t)
	// The store's parent is the root itself: no disk, so no copies, and
	// nothing is written there.
	store := "/fleetci-l1cache-test"
	l1 := startL1(t, l2.srv.URL+"/", store, false)
	save(t, l1, "k", []byte("data"), 0)
	key, data, url := restore(t, l1, "k", nil)
	if key != "k" || string(data) != "data" || !strings.HasPrefix(url, l2.srv.URL) {
		t.Fatalf("without a disk: %q %q %s", key, data, url)
	}
	if _, err := os.Stat(store); err == nil {
		entries, _ := os.ReadDir(store)
		if len(entries) > 0 {
			t.Fatalf("stored without a disk: %v", entries)
		}
	}
}

func TestL1CacheDiskFails(t *testing.T) {
	l2 := newFakeL2(t)
	store := filepath.Join(t.TempDir(), "actions")
	l1 := startL1(t, l2.srv.URL+"/", store, true)
	// The disk refuses writes: jobs restore and save all the same.
	if err := os.MkdirAll(filepath.Join(store, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(store, "tmp"), 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(store, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(store, 0o755); _ = os.Chmod(filepath.Join(store, "tmp"), 0o755) })
	if os.Geteuid() == 0 {
		t.Skip("root writes anyway")
	}
	save(t, l1, "k", []byte("data"), 0)
	l2.put("other", "v1", []byte("theirs"))
	for range 2 {
		if key, data, _ := restore(t, l1, "other", nil); key != "other" || string(data) != "theirs" {
			t.Fatalf("restore: %q %q", key, data)
		}
	}
	if key, data, _ := restore(t, l1, "k", nil); key != "k" || string(data) != "data" {
		t.Fatalf("restore of the save: %q %q", key, data)
	}
}

// withQuery adds q to url, as Azure's SDK adds its parameters.
func withQuery(url, q string) string {
	if strings.Contains(url, "?") {
		return url + "&" + q
	}
	return url + "?" + q
}

// A miss is fetched to the disk in parallel ranges, as actions/cache would
// fetch it from Azure, then served from the disk.
// The job's log ends with what its cache steps got: the L1's summary.
func TestL1CacheSummary(t *testing.T) {
	l2 := newFakeL2(t)
	l1 := startL1(t, l2.srv.URL+"/", filepath.Join(t.TempDir(), "actions"), true)
	stats := func() string {
		code, out := do(t, http.MethodGet, l1+"/fleetci/stats", nil, nil)
		if code != http.StatusOK {
			t.Fatalf("stats: %d %s", code, out)
		}
		return strings.TrimSpace(string(out))
	}
	if s := stats(); s != "fleetci cache: no actions/cache calls" {
		t.Fatalf("before: %q", s)
	}
	l2.put("deps", "v1", bytes.Repeat([]byte("d"), 3_000_000))
	restore(t, l1, "deps", nil)
	restore(t, l1, "deps", nil)
	restore(t, l1, "nope", nil)
	save(t, l1, "build", bytes.Repeat([]byte("b"), 2_000_000), 0)
	s := stats()
	for _, want := range []string{"1 restored from the cache disk (3 MB)", "1 downloaded to it (3 MB in ", "1 not in the cache", "1 saved, kept on the disk (2 MB)"} {
		if !strings.Contains(s, want) {
			t.Errorf("%q lacks %q", s, want)
		}
	}
}

func TestL1CacheFetchesInRanges(t *testing.T) {
	l2 := newFakeL2(t)
	store := filepath.Join(t.TempDir(), "actions")
	l1 := startL1(t, l2.srv.URL+"/", store, true)
	big := make([]byte, 20<<20+123) // three 8 MiB ranges
	for i := range big {
		big[i] = byte(i * 7)
	}
	l2.put("big", "v1", big)
	key, data, url := restore(t, l1, "big", nil)
	if key != "big" || !bytes.Equal(data, big) || !strings.HasPrefix(url, l1) {
		t.Fatalf("first restore: %q, %d bytes, equal %v, %s", key, len(data), bytes.Equal(data, big), url)
	}
	if g, r := l2.gets.Load(), l2.ranges.Load(); g != 1 || r != 3 {
		t.Fatalf("%d downloads in %d more ranges; want 1 in 3", g, r)
	}
	if _, data, _ = restore(t, l1, "big", nil); !bytes.Equal(data, big) || l2.gets.Load() != 1 {
		t.Fatalf("second restore: equal %v, %d downloads", bytes.Equal(data, big), l2.gets.Load())
	}
}

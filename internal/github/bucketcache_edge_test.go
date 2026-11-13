package github

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Cases falcondev's suite has no counterpart for: they come from fleetci's
// own design, a node between the VM and the bucket and saves uploaded
// after the job, and from what a real bucket does.

func bucketL1(t *testing.T, node string) string {
	t.Helper()
	l2 := newFakeL2(t)
	return startL1(t, l2.srv.URL+"/", filepath.Join(t.TempDir(), "actions"), true, "remote=bucket", "node="+node)
}

func bareToken(tok string) string { return strings.TrimPrefix(tok, "Bearer ") }

// The node is best effort: when the VM can't reach it, a restore is
// answered from the disk, a save stays there, and the flush says what it
// couldn't upload without failing the job.
func TestL1CacheBucketNodeUnreachable(t *testing.T) {
	is := newTestIssuer(t, true)
	tok := bareToken(is.token(t, nil))
	vm := bucketL1(t, "http://127.0.0.1:"+itoa(freePort(t)))
	if code := saveAs(t, vm, tok, "go-linux-aaa", []byte("saved")); code != http.StatusOK {
		t.Fatalf("save: %d", code)
	}
	key, data, url := restoreData(t, vm, tok, "go-linux-bbb", "go-linux-")
	if key != "go-linux-aaa" || string(data) != "saved" || !strings.Contains(url, "/fleetci/l1/") {
		t.Fatalf("restore without the node: %q %q %s", key, data, url)
	}
	if got := flush(t, vm); got["failed"] != 1 || got["uploaded"] != 0 {
		t.Fatalf("flush without the node: %v", got)
	}
}

// An upload the bucket refuses stays pending, and the next flush uploads it.
func TestL1CacheBucketFlushRetries(t *testing.T) {
	is := newTestIssuer(t, true)
	b := newFakeBucket(t)
	vm := bucketL1(t, bucketNode(t, b, is.srv.URL))
	tok := bareToken(is.token(t, nil))
	saveAs(t, vm, tok, "a", []byte("one"))
	saveAs(t, vm, tok, "b", []byte("two"))
	b.mu.Lock()
	b.refusePuts = 1
	b.mu.Unlock()
	if got := flush(t, vm); got["failed"] != 1 || got["uploaded"] != 1 {
		t.Fatalf("first flush: %v", got)
	}
	if got := flush(t, vm); got["failed"] != 0 || got["uploaded"] != 1 {
		t.Fatalf("second flush: %v", got)
	}
	if n := b.names(); len(n) != 2 {
		t.Fatalf("the bucket has %v", n)
	}
}

// A token the node refuses uploads nothing, and the save stays on the disk.
func TestL1CacheBucketFlushRefusedToken(t *testing.T) {
	is := newTestIssuer(t, true)
	b := newFakeBucket(t)
	vm := bucketL1(t, bucketNode(t, b, is.srv.URL))
	forged := bareToken(newTestIssuer(t, true).token(t, map[string]any{"iss": is.srv.URL}))
	saveAs(t, vm, forged, "a", []byte("one"))
	if got := flush(t, vm); got["failed"] != 1 || len(b.names()) != 0 {
		t.Fatalf("flush with a forged token: %v, bucket %v", got, b.names())
	}
	if key, data, _ := restoreData(t, vm, forged, "a"); key != "a" || string(data) != "one" {
		t.Fatalf("the job's own save from the disk: %q %q", key, data)
	}
}

// A key too long for an object name is kept on the disk and not uploaded.
func TestL1CacheBucketKeyTooLong(t *testing.T) {
	is := newTestIssuer(t, true)
	b := newFakeBucket(t)
	vm := bucketL1(t, bucketNode(t, b, is.srv.URL))
	tok := bareToken(is.token(t, nil))
	long := strings.Repeat("é", 400) // 800 bytes, 2400 escaped
	saveAs(t, vm, tok, long, []byte("x"))
	if got := flush(t, vm); got["failed"] != 1 || len(b.names()) != 0 {
		t.Fatalf("flush of a long key: %v", got)
	}
}

// An object the bucket lost after the node listed it is a miss, as a
// dangling entry is on GitHub; nothing is kept of it.
func TestL1CacheBucketVanishedObject(t *testing.T) {
	is := newTestIssuer(t, true)
	b := newFakeBucket(t)
	vm := bucketL1(t, bucketNode(t, b, is.srv.URL))
	tok := bareToken(is.token(t, nil))
	name := entryDir("1", "refs/heads/main", "v1") + "gone"
	b.put(name, []byte("soon gone"), time.Now())
	code, m := call(t, vm, tok, "GetCacheEntryDownloadURL", map[string]any{"key": "gone", "version": "v1"})
	if code != http.StatusOK || m["ok"] != true {
		t.Fatalf("lookup: %d %v", code, m)
	}
	b.mu.Lock()
	delete(b.objects, name)
	b.mu.Unlock()
	if code, _ := do(t, http.MethodGet, m["signed_download_url"].(string), nil, nil); code == http.StatusOK {
		t.Fatal("a vanished object downloaded")
	}
	if code, m := call(t, vm, tok, "GetCacheEntryDownloadURL", map[string]any{"key": "gone", "version": "v1"}); code != http.StatusOK || m["ok"] == true {
		t.Fatalf("restore after: %d %v", code, m)
	}
}

// The first read of an entry may be a range (BuildKit reads at offsets);
// the disk still gets the whole entry.
func TestL1CacheBucketRangedFirstRead(t *testing.T) {
	is := newTestIssuer(t, true)
	b := newFakeBucket(t)
	vm := bucketL1(t, bucketNode(t, b, is.srv.URL))
	tok := bareToken(is.token(t, nil))
	data := bytes.Repeat([]byte("0123456789"), 300_000)
	b.put(entryDir("1", "refs/heads/main", "v1")+"layers", data, time.Now())
	_, m := call(t, vm, tok, "GetCacheEntryDownloadURL", map[string]any{"key": "layers", "version": "v1"})
	code, part := do(t, http.MethodGet, m["signed_download_url"].(string), nil, map[string]string{"x-ms-range": "bytes=1000-1099"})
	if code != http.StatusPartialContent || !bytes.Equal(part, data[1000:1100]) {
		t.Fatalf("ranged first read: %d, %d bytes", code, len(part))
	}
	gets := b.gets
	key, all, url := restoreData(t, vm, tok, "layers")
	if key != "layers" || !bytes.Equal(all, data) || !strings.Contains(url, "/fleetci/l1/") || b.gets != gets {
		t.Fatalf("after a ranged read: %q, %d bytes, %s, %d more bucket reads", key, len(all), url, b.gets-gets)
	}
}

// Jobs' parallel restores of one entry on one disk each get all of it.
func TestL1CacheBucketParallelRestores(t *testing.T) {
	is := newTestIssuer(t, true)
	b := newFakeBucket(t)
	vm := bucketL1(t, bucketNode(t, b, is.srv.URL))
	tok := bareToken(is.token(t, nil))
	data := bytes.Repeat([]byte("abcdefgh"), 1_000_000)
	b.put(entryDir("1", "refs/heads/main", "v1")+"deps", data, time.Now())
	var wg sync.WaitGroup
	errs := make(chan string, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, m := call(t, vm, tok, "GetCacheEntryDownloadURL", map[string]any{"key": "deps", "version": "v1"})
			url, _ := m["signed_download_url"].(string)
			code, got := do(t, http.MethodGet, url, nil, nil)
			if code != http.StatusOK || !bytes.Equal(got, data) {
				errs <- url
			}
		}()
	}
	wg.Wait()
	close(errs)
	for u := range errs {
		t.Errorf("a parallel restore got the wrong data: %s", u)
	}
}

// Objects in the bucket that fleetci didn't write, or wrote under another
// layout, are never an entry; GitHub's scope order beats an exact key in a
// later scope.
func TestBucketMatchingOurs(t *testing.T) {
	is := newTestIssuer(t, true)
	b := newFakeBucket(t)
	node := bucketNode(t, b, is.srv.URL)
	main := entryDir("1", "refs/heads/main", "v1")
	feature := entryDir("1", "refs/heads/feature", "v1")
	t0 := time.Now()
	for i, n := range []string{
		main + escapeName("npm/linux/abc"), // a key with slashes
		main + "deeper/object",             // not ours: a / inside the key
		main + "bad%zzescape",              // not ours: a broken escape
		main + "only-main-exact",
		feature + "only-main-prefix-of-feature",
	} {
		b.put(n, []byte(n), t0.Add(time.Duration(i)*time.Second))
	}
	onFeature := is.token(t, map[string]any{"ac": `[{"Scope":"refs/heads/feature","Permission":3},{"Scope":"refs/heads/main","Permission":1}]`})
	lookup := func(keys ...string) string {
		body, _ := json.Marshal(lookupReq{Key: keys[0], RestoreKeys: keys[1:], Version: "v1"})
		req, _ := http.NewRequest(http.MethodPost, node+"/lookup", bytes.NewReader(body))
		req.Header.Set("Authorization", onFeature)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		k, _ := m["matched_key"].(string)
		return k
	}
	for _, c := range []struct {
		name string
		keys []string
		want string
	}{
		{"a key with slashes", []string{"npm/linux/abc"}, "npm/linux/abc"},
		{"a restore key ending in a slash", []string{"x", "npm/linux/"}, "npm/linux/abc"},
		{"an object below the key's level", []string{"deeper/object", "deeper"}, ""},
		{"an object with a broken escape", []string{"x", "bad"}, ""},
		{"the first scope's prefix beats a later scope's exact key", []string{"only-main-exact", "only-main-"}, "only-main-prefix-of-feature"},
	} {
		if got := lookup(c.keys...); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// A lookup without a token, or without a key, gets nothing.
	for _, h := range []map[string]string{{}, {"Authorization": "Bearer "}} {
		req, _ := http.NewRequest(http.MethodPost, node+"/lookup", strings.NewReader(`{"key":"x","version":"v1"}`))
		for k, v := range h {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("no token %v: %d", h, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, node+"/lookup", strings.NewReader(`{"version":"v1"}`))
	req.Header.Set("Authorization", onFeature)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("no key: %d", resp.StatusCode)
	}
}

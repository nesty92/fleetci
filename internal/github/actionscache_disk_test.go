package github

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// jobToken is a job's ACTIONS_RUNTIME_TOKEN as GitHub's cache server reads
// it: the "ac" claim's scopes, each with its permission bits (1 read,
// 2 write). The actions cache doesn't check the signature.
func jobToken(scopes ...any) string {
	var ac []map[string]any
	for i := 0; i < len(scopes); i += 2 {
		ac = append(ac, map[string]any{"Scope": scopes[i], "Permission": scopes[i+1]})
	}
	b, _ := json.Marshal(ac)
	claims, _ := json.Marshal(map[string]any{"ac": string(b), "repository_id": "1"})
	return "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

// call is one of the cache service's calls with the job's token.
func call(t *testing.T, base, token, method string, req any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(req)
	code, out := do(t, http.MethodPost, base+svcPath+method, b,
		map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + token})
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	return code, m
}

// saveAs saves key with the job's token; it returns CreateCacheEntry's
// status when that refuses.
func saveAs(t *testing.T, base, token, key string, data []byte) int {
	t.Helper()
	code, m := call(t, base, token, "CreateCacheEntry", map[string]any{"key": key, "version": "v1"})
	if code != http.StatusOK || m["ok"] != true {
		return code
	}
	code, _ = do(t, http.MethodPut, m["signed_upload_url"].(string), data, map[string]string{"x-ms-blob-type": "BlockBlob"})
	if code != http.StatusCreated {
		t.Fatalf("put %s: %d", key, code)
	}
	code, m = call(t, base, token, "FinalizeCacheEntryUpload", map[string]any{"key": key, "version": "v1", "size_bytes": fmt.Sprint(len(data))})
	if code != http.StatusOK || m["ok"] != true {
		t.Fatalf("finalize %s: %d %v", key, code, m)
	}
	time.Sleep(20 * time.Millisecond) // a newer save is newer
	return http.StatusOK
}

// restoreAs is the key a restore matches with the job's token, "" for none.
func restoreAs(t *testing.T, base, token string, keys ...string) string {
	t.Helper()
	code, m := call(t, base, token, "GetCacheEntryDownloadURL", map[string]any{"key": keys[0], "restore_keys": keys[1:], "version": "v1"})
	if code != http.StatusOK {
		t.Fatalf("restore %v: %d %v", keys, code, m)
	}
	if m["ok"] != true {
		return ""
	}
	return m["matched_key"].(string)
}

// TestActionsCacheDiskOnly: with remote=off, restores and saves are the disk's
// alone, matched in GitHub's order, and the cache server sees no cache
// call. The cases are falcondev-oss/github-actions-cache-server's
// matches_scopes_then_keys_then_exact_before_prefix.
func TestActionsCacheDiskOnly(t *testing.T) {
	l2 := newFakeL2(t)
	ac := startActionsCache(t, l2.srv.URL+"/", filepath.Join(t.TempDir(), "actions"), true, "remote=off")
	const main, feature = "refs/heads/main", "refs/heads/feature"
	onMain := jobToken(main, 3)
	onFeature := jobToken(feature, 3, main, 1)
	featureLast := jobToken(main, 3, feature, 1) // reads main first

	for _, key := range []string{"primary-old", "primary-new", "restore", "restore-newer"} {
		if code := saveAs(t, ac, onMain, key, []byte(key)); code != http.StatusOK {
			t.Fatalf("save %s: %d", key, code)
		}
	}
	if code := saveAs(t, ac, onFeature, "primary", []byte("primary")); code != http.StatusOK {
		t.Fatalf("save primary: %d", code)
	}

	for _, c := range []struct {
		name, token string
		keys        []string
		want        string
	}{
		{"the first scope wins", onFeature, []string{"primary", "restore"}, "primary"},
		{"the newest prefix of the key beats an exact restore key", featureLast, []string{"primary", "restore"}, "primary-new"},
		{"an exact restore key beats a newer entry it prefixes", onMain, []string{"missing", "restore"}, "restore"},
		{"a later restore key matches by prefix", onMain, []string{"missing", "absent", "restore-"}, "restore-newer"},
		{"nothing matches", featureLast, []string{"missing", "absent"}, ""},
		{"case counts", onMain, []string{"Primary-new"}, ""},
		{"another scope's entry is not read", jobToken("refs/heads/other", 3), []string{"restore"}, ""},
	} {
		if got := restoreAs(t, ac, c.token, c.keys...); got != c.want {
			t.Errorf("%s: restored %q, want %q", c.name, got, c.want)
		}
	}

	// A saved entry never changes: GitHub's 409. Another scope may save the
	// same key; its own job then restores its own.
	if code := saveAs(t, ac, onMain, "restore", []byte("again")); code != http.StatusConflict {
		t.Errorf("saving a key again: %d, want 409", code)
	}
	if code := saveAs(t, ac, onFeature, "restore", []byte("feature's")); code != http.StatusOK {
		t.Errorf("saving another scope's key: %d", code)
	}
	if got := restoreAs(t, ac, onFeature, "restore"); got != "restore" {
		t.Errorf("feature's own restore: %q", got)
	}

	if len(l2.entries) != 0 || len(l2.pending) != 0 || l2.gets.Load() != 0 {
		t.Errorf("the cache server got cache calls: %d entries, %d uploads, %d downloads", len(l2.entries), len(l2.pending), l2.gets.Load())
	}
	// Other calls, artifacts', still go to it.
	if code, out := do(t, http.MethodPost, ac+"/twirp/github.actions.results.api.v1.ArtifactService/Other", []byte("a"), nil); code != http.StatusOK || !bytes.Contains(out, []byte(`"a"`)) {
		t.Errorf("passthrough: %d %s", code, out)
	}
}

// TestActionsCacheUploadsAndRanges: uploads answer with a request id, which
// BuildKit's client needs, and the disk's copy is served in ranges, which
// BuildKit reads it by.
func TestActionsCacheUploadsAndRanges(t *testing.T) {
	for _, mode := range [][]string{nil, {"remote=off"}} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			l2 := newFakeL2(t)
			ac := startActionsCache(t, l2.srv.URL+"/", filepath.Join(t.TempDir(), "actions"), true, mode...)
			data := []byte("0123456789abcdefghij")
			_, m := call(t, ac, "tok", "CreateCacheEntry", map[string]any{"key": "k", "version": "v1"})
			if m == nil || m["ok"] != true {
				// The fake cache server takes only its own token.
				_, m = call(t, ac, jobToken("refs/heads/main", 3), "CreateCacheEntry", map[string]any{"key": "k", "version": "v1"})
			}
			req, _ := http.NewRequest(http.MethodPut, m["signed_upload_url"].(string), bytes.NewReader(data))
			req.Header.Set("x-ms-blob-type", "BlockBlob")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusCreated || resp.Header.Get("x-ms-request-id") == "" {
				t.Fatalf("upload: %d, request id %q", resp.StatusCode, resp.Header.Get("x-ms-request-id"))
			}
			tok := "tok"
			if mode != nil {
				tok = jobToken("refs/heads/main", 3)
			}
			if code, f := call(t, ac, tok, "FinalizeCacheEntryUpload", map[string]any{"key": "k", "version": "v1", "size_bytes": fmt.Sprint(len(data))}); code != http.StatusOK || f["ok"] != true {
				t.Fatalf("finalize: %d %v", code, f)
			}
			_, r := call(t, ac, tok, "GetCacheEntryDownloadURL", map[string]any{"key": "k", "version": "v1"})
			url, _ := r["signed_download_url"].(string)
			for _, c := range []struct {
				header, value string
				code          int
				want          string
			}{
				{"", "", 200, string(data)},
				{"x-ms-range", "bytes=5-", 206, string(data[5:])},
				{"Range", "bytes=2-4", 206, "234"},
				{"Range", "bytes=-3", 206, "hij"},
				{"x-ms-range", "bytes=50-", 416, ""},
			} {
				req, _ := http.NewRequest(http.MethodGet, url, nil)
				if c.header != "" {
					req.Header.Set(c.header, c.value)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				got, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != c.code || string(got) != c.want {
					t.Errorf("%s %s: %d %q, want %d %q", c.header, c.value, resp.StatusCode, got, c.code, c.want)
				}
			}
		})
	}
}

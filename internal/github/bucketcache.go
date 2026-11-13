package github

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/cachebucket"
)

// bucketCache is actions/cache's store in the user's own bucket, served
// by the node to its VM's L1 (l1cache.py, remote=bucket). The VM asks
// which entry a restore gets and where to upload a save; the node checks
// the job's token, lists and signs, and the VM only ever gets links.
// Saves are uploaded after the job, from the job's cache disk.
//
// An entry is the object
//
//	github/<repository_id>/<scope>/<version>/<key>
//
// each part escaped byte by byte outside [A-Za-z0-9._~-], so a key's
// prefix is its name's prefix, and one scope's name never prefixes
// another's. Entries never change once saved; the bucket's own lifecycle
// rules expire them.
type bucketCache struct {
	bucket cachebucket.Bucket
	tokens *tokenVerifier
	log    hclog.Logger
}

// linkTTL is how long a link the VM gets works.
const linkTTL = time.Hour

// maxObjectName is S3's and GCS's limit on a name, in bytes.
const maxObjectName = 1024

func escapeName(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '~' || c == '-' {
			b.WriteByte(c)
		} else {
			b.WriteString("%" + strings.ToUpper(hexByte(c)))
		}
	}
	return b.String()
}

func hexByte(c byte) string {
	const h = "0123456789abcdef"
	return string([]byte{h[c>>4], h[c&15]})
}

func unescapeName(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) || unhex(s[i+1]) < 0 || unhex(s[i+2]) < 0 {
			return "", false
		}
		b.WriteByte(byte(unhex(s[i+1])<<4 | unhex(s[i+2])))
		i += 2
	}
	return b.String(), true
}

func unhex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// entryDir is where a scope's entries of one version are.
func entryDir(repo, scope, version string) string {
	return "github/" + escapeName(repo) + "/" + escapeName(scope) + "/" + escapeName(version) + "/"
}

type entry struct {
	key, name string
	modified  time.Time
}

// match is GitHub's choice among one scope's entries: the key and then
// each restore key, each first exactly and else as the prefix of the
// newest entry. Keys compare byte for byte.
func match(entries []entry, keys []string) (entry, bool) {
	for _, k := range keys {
		var newest *entry
		for i := range entries {
			e := &entries[i]
			if e.key == k {
				return *e, true
			}
			if strings.HasPrefix(e.key, k) && (newest == nil || e.modified.After(newest.modified)) {
				newest = e
			}
		}
		if newest != nil {
			return *newest, true
		}
	}
	return entry{}, false
}

type lookupReq struct {
	Key         string   `json:"key"`
	RestoreKeys []string `json:"restore_keys"`
	Version     string   `json:"version"`
}

func (c *bucketCache) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /lookup", c.lookup)
	mux.HandleFunc("POST /upload", c.upload)
	return mux
}

func (c *bucketCache) request(w http.ResponseWriter, r *http.Request) (cacheGrant, lookupReq, bool) {
	var req lookupReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || req.Key == "" || req.Version == "" {
		http.Error(w, "want a key and a version", http.StatusBadRequest)
		return cacheGrant{}, req, false
	}
	g, err := c.tokens.verify(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		c.log.Warn("cache bucket: refused a job's token", "err", err)
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return cacheGrant{}, req, false
	}
	return g, req, true
}

// lookup answers which entry a restore gets, and a link to it.
func (c *bucketCache) lookup(w http.ResponseWriter, r *http.Request) {
	g, req, ok := c.request(w, r)
	if !ok {
		return
	}
	keys := append([]string{req.Key}, req.RestoreKeys...)
	for _, scope := range g.Reads {
		dir := entryDir(g.Repo, scope, req.Version)
		// Each key lists only the names it prefixes, which escaping keeps:
		// a scope's other entries, however many, aren't read.
		var e entry
		found := false
		for _, k := range keys {
			objs, err := c.bucket.List(r.Context(), dir+escapeName(k))
			if err != nil {
				c.log.Warn("cache bucket: listing failed", "err", err)
				http.Error(w, "listing the bucket failed", http.StatusBadGateway)
				return
			}
			var entries []entry
			for _, o := range objs {
				rest := strings.TrimPrefix(o.Name, dir)
				if k, ok := unescapeName(rest); ok && !strings.Contains(rest, "/") {
					entries = append(entries, entry{key: k, name: o.Name, modified: o.Modified})
				}
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
			if e, found = match(entries, []string{k}); found {
				break
			}
		}
		if !found {
			continue
		}
		l, err := c.bucket.Download(r.Context(), e.name, linkTTL)
		if err != nil {
			c.log.Warn("cache bucket: signing failed", "err", err)
			http.Error(w, "signing a link failed", http.StatusBadGateway)
			return
		}
		replyJSON(w, http.StatusOK, map[string]any{"ok": true, "matched_key": e.key, "scope": scope, "name": e.name, "url": l.URL, "headers": l.Headers})
		return
	}
	replyJSON(w, http.StatusOK, map[string]any{"ok": false})
}

// upload answers where a save goes: its scope's entry, if not saved yet.
func (c *bucketCache) upload(w http.ResponseWriter, r *http.Request) {
	g, req, ok := c.request(w, r)
	if !ok {
		return
	}
	if g.Write == "" {
		http.Error(w, "the token may not save", http.StatusForbidden)
		return
	}
	name := entryDir(g.Repo, g.Write, req.Version) + escapeName(req.Key)
	if len(name) > maxObjectName {
		http.Error(w, "the key is too long", http.StatusBadRequest)
		return
	}
	objs, err := c.bucket.List(r.Context(), name)
	if err != nil {
		c.log.Warn("cache bucket: listing failed", "err", err)
		http.Error(w, "listing the bucket failed", http.StatusBadGateway)
		return
	}
	for _, o := range objs {
		if o.Name == name {
			replyJSON(w, http.StatusConflict, map[string]any{"code": "already_exists"})
			return
		}
	}
	l, err := c.bucket.Upload(r.Context(), name, linkTTL)
	if err != nil {
		c.log.Warn("cache bucket: signing failed", "err", err)
		http.Error(w, "signing a link failed", http.StatusBadGateway)
		return
	}
	replyJSON(w, http.StatusOK, map[string]any{"ok": true, "scope": g.Write, "name": name, "url": l.URL, "headers": l.Headers})
}

func replyJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

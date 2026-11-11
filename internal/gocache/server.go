// Package gocache is fleetci's shared Go build cache: a server that keeps
// the go command's cache entries in S3, and a GOCACHEPROG client that jobs
// run, which holds no credentials but its job's token (see jobtoken).
//
// The server keeps each project's entries apart and decides from the
// token what a job may read and write. Jobs on protected refs write the
// project's shared entries, which all its jobs read; any other job writes
// entries of its own ref and reads those and the shared ones. A job on an
// unprotected ref, such as a merge request's, can't plant an entry that a
// protected ref's build then uses.
package gocache

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/jobtoken"
)

// Headers of an entry, next to its body.
const (
	headerOutputID = "Fleetci-Output-Id"
	headerTime     = "Fleetci-Time"
)

// maxEntry caps an entry's size: the go command's largest outputs are
// linked binaries and test results.
const maxEntry = 1 << 30

// errMiss is an entry the store doesn't have.
var errMiss = errors.New("no such entry")

// Entry is a stored cache entry's metadata.
type Entry struct {
	OutputID string // hex
	Size     int64
	Time     time.Time
}

// Store keeps entries by key.
type Store interface {
	Get(ctx context.Context, key string) (io.ReadCloser, Entry, error)
	Put(ctx context.Context, key string, e Entry, body io.Reader) error
}

// Server serves the cache to jobs with a valid token.
type Server struct {
	Store Store
	Keys  []ed25519.PublicKey
	Log   hclog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

var hexID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// scopes are where c's job reads, in order, and where it writes.
func scopes(c jobtoken.Claims) (read []string, write string) {
	base := fmt.Sprintf("%s/%s/%s/", c.Forge, c.Instance, c.Project)
	shared := base + "protected"
	if c.Protected {
		return []string{shared}, shared
	}
	sum := sha256.Sum256([]byte(c.Ref))
	own := base + "ref/" + hex.EncodeToString(sum[:8])
	return []string{own, shared}, own
}

var safe = regexp.MustCompile(`^[A-Za-z0-9.:_-]+$`)

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutPrefix(r.URL.Path, "/v1/cache/")
	if !ok || !hexID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	c, err := jobtoken.Verify(s.Keys, tok, now())
	if err != nil || !safe.MatchString(c.Forge) || !safe.MatchString(c.Instance) || !safe.MatchString(c.Project) {
		http.Error(w, "invalid job token", http.StatusUnauthorized)
		return
	}
	read, write := scopes(c)
	switch r.Method {
	case http.MethodGet:
		for _, sc := range read {
			body, e, err := s.Store.Get(r.Context(), sc+"/"+id)
			if errors.Is(err, errMiss) {
				continue
			}
			if err != nil {
				s.Log.Warn("get", "key", sc+"/"+id, "err", err)
				http.Error(w, "store", http.StatusBadGateway)
				return
			}
			defer body.Close()
			w.Header().Set(headerOutputID, e.OutputID)
			w.Header().Set(headerTime, e.Time.UTC().Format(time.RFC3339Nano))
			w.Header().Set("Content-Length", strconv.FormatInt(e.Size, 10))
			_, _ = io.Copy(w, body)
			return
		}
		http.Error(w, "miss", http.StatusNotFound)
	case http.MethodPut:
		out := r.Header.Get(headerOutputID)
		if !hexID.MatchString(out) || r.ContentLength < 0 || r.ContentLength > maxEntry {
			http.Error(w, "want an output ID and a length", http.StatusBadRequest)
			return
		}
		e := Entry{OutputID: out, Size: r.ContentLength, Time: now()}
		if err := s.Store.Put(r.Context(), write+"/"+id, e, io.LimitReader(r.Body, r.ContentLength)); err != nil {
			s.Log.Warn("put", "key", write+"/"+id, "err", err)
			http.Error(w, "store", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

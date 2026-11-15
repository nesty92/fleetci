package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"

	"github.com/nesty92/fleetci/internal/cachebucket"
)

// Where actions/cache keeps its caches, besides the job's cache disk.
const (
	// StoreGitHub is GitHub's cache, or the cache server at cache_url:
	// the disk is its actions cache.
	StoreGitHub = "github"
	// StoreBucket is the user's own bucket, [github.cache_bucket]: the
	// node lists and signs for the VM, and saves upload after the job.
	StoreBucket = "bucket"
	// StoreOff is the cache disk alone.
	StoreOff = "off"
)

// CacheStore is where a VM's actions/cache keeps its caches.
type CacheStore struct {
	Store string
	// URL is a cache server in GitHub's place (StoreGitHub only).
	URL    string
	bucket *bucketCache
}

// NewCacheStore checks a [github] table's cache settings: store is
// cache_store, cacheURL cache_url, bucket cache_bucket and issuer
// cache_token_issuer, whose keys sign the jobs' tokens.
func NewCacheStore(store, cacheURL string, bucket *cachebucket.Config, issuer string, log hclog.Logger) (CacheStore, error) {
	if store == "" {
		store = StoreGitHub
	}
	cs := CacheStore{Store: store}
	if cacheURL != "" {
		if store != StoreGitHub {
			return cs, fmt.Errorf("github cache_url with cache_store = %q: the cache server is the store only with \"github\"", store)
		}
		u, err := url.Parse(cacheURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.ContainsAny(cacheURL, "'\"\\ $`") {
			return cs, fmt.Errorf("github cache_url %q: want an http(s) URL", cacheURL)
		}
		// The cache client appends paths to it.
		if !strings.HasSuffix(cacheURL, "/") {
			cacheURL += "/"
		}
		cs.URL = cacheURL
	}
	if bucket != nil && store != StoreBucket {
		return cs, fmt.Errorf("github cache_bucket with cache_store = %q: want \"bucket\"", store)
	}
	switch store {
	case StoreGitHub, StoreOff:
	case StoreBucket:
		if bucket == nil {
			return cs, errors.New(`github cache_store = "bucket" needs a [github.cache_bucket] table`)
		}
		b, err := cachebucket.New(bucket)
		if err != nil {
			return cs, fmt.Errorf("github cache_bucket: %w", err)
		}
		if log == nil {
			log = hclog.NewNullLogger()
		}
		cs.bucket = &bucketCache{bucket: b, tokens: newTokenVerifier(issuer), log: log}
	default:
		return cs, fmt.Errorf(`github cache_store %q: want "github", "bucket" or "off"`, store)
	}
	return cs, nil
}

// actionsCacheArgs is the actions cache's arguments after its store.
func (cs CacheStore) actionsCacheArgs() string {
	switch cs.Store {
	case StoreOff:
		return "remote=off "
	case StoreBucket:
		return fmt.Sprintf("remote=bucket node=http://127.0.0.1:%d ", nodeCachePort)
	}
	return ""
}

// nodeCachePort is where the VM reaches its node's cache service: a port on
// the VM's loopback that the node's SSH connection forwards to it.
var nodeCachePort = 31850

// flushURL is the VM's actions cache's, which uploads the job's saves.
var flushURL = fmt.Sprintf("http://127.0.0.1:%d/fleetci/flush", actionsCachePort)

// flushTimeout bounds the uploads of a job's saves after the job.
const flushTimeout = 15 * time.Minute

// serveNode makes the node's cache service reachable on the VM, through
// the SSH connection; stop ends it. Without the forward, the VM's actions cache
// answers from its disk and keeps its saves there.
func serveNode(client *ssh.Client, h http.Handler, log hclog.Logger) (stop func()) {
	ln, err := client.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", nodeCachePort))
	if err != nil {
		log.Warn("cache bucket: the VM can't reach the node's cache; jobs use their disk alone", "err", err)
		return func() {}
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return func() { _ = srv.Close() }
}

// flushSaves uploads the job's saves from its disk to the bucket, after
// the job. It is best effort: a save it misses stays on the disk.
func flushSaves(ctx context.Context, client *ssh.Client, log hclog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, flushTimeout)
	defer cancel()
	s, err := client.NewSession()
	if err != nil {
		log.Warn("cache bucket: the job's saves weren't uploaded", "err", err)
		return
	}
	defer s.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-done:
		}
	}()
	out, err := s.Output(fmt.Sprintf("curl -fsS -m %d -X POST %s", int(flushTimeout.Seconds()), flushURL))
	if err != nil {
		log.Warn("cache bucket: the job's saves weren't uploaded", "err", err)
		return
	}
	log.Info("cache bucket: uploaded the job's saves", "result", strings.TrimSpace(string(out)))
}

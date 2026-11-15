package gitlab

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"golang.org/x/crypto/ssh"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/cachebucket"
	"github.com/nesty92/fleetci/internal/github"
	"github.com/nesty92/fleetci/internal/gitlabcache"
	"github.com/nesty92/fleetci/internal/gitlabcache/vmcache"
	"github.com/nesty92/fleetci/internal/source"
)

// cacheStoreEnv, in a runner's environment, says how its jobs keep their
// cache: in the bucket: "archive" keeps GitLab's own, one archive per key.
// [gitlab] cache_store sets it for fleetci's runners; a config.toml's
// runner may set it itself.
const cacheStoreEnv = "FLEETCI_CACHE_STORE"

// Cache stores, for [gitlab] cache_store.
const (
	// StoreFiles keeps a job's cache: file by file, each content once in
	// the project's store, so a save uploads what changed only.
	StoreFiles = "files"
	// StoreArchive keeps GitLab's archive per key, as gitlab-runner does.
	StoreArchive = "archive"
)

// bucketTypes are the runner's cache types the per-file store is for.
var bucketTypes = []string{"s3", "s3v2", "gcs", "gcsv2", "azure"}

// fileCacheReady bounds the wait for the VM's service to answer.
const fileCacheReady = 10 * time.Second

// withCacheStore notes store in a runner's environment, unless it says
// one already.
func withCacheStore(env []string, store string) []string {
	if store == "" || slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, cacheStoreEnv+"=") }) {
		return env
	}
	return append(env, cacheStoreEnv+"="+store)
}

// usesFileCache says the job's cache: goes to the per-file store: its
// runner keeps the cache in a bucket and doesn't ask for archives.
func usesFileCache(rc *common.RunnerConfig) bool {
	if rc == nil || rc.Cache == nil || !slices.Contains(bucketTypes, rc.Cache.Type) {
		return false
	}
	for _, e := range rc.Environment {
		if v, ok := strings.CutPrefix(e, cacheStoreEnv+"="); ok {
			return v != StoreArchive
		}
	}
	return true
}

// fileCache puts the job's cache: in the per-file store: it starts the
// service on the VM GitLab's helper then sends its cache to and takes it
// from, serves the VM the node's links for the job's own cache names, and
// points the job's cache config at the VM. Without it, as when something
// fails, the job keeps its cache as gitlab-runner does. stop ends it,
// after the job.
func (e *executor) fileCache(options common.ExecutorPrepareOptions, client *ssh.Client, log hclog.Logger) (stop func()) {
	build := options.Build
	rc := build.Runner
	bucket, err := cachebucket.New(rc.Cache)
	if err != nil {
		options.BuildLogger.Warningln("Cache in archives: " + err.Error())
		return func() {}
	}
	closeBucket := func() {
		if c, ok := bucket.(interface{ Close() }); ok {
			c.Close()
		}
	}
	at, err := startFileCache(options.Context, client)
	if err != nil {
		closeBucket()
		options.BuildLogger.Warningln("Cache in archives: " + err.Error())
		return func() {}
	}
	cfg, job, done := gitlabcache.ForJob(rc.Cache, fmt.Sprintf("http://%s:%d", at, vmcache.VMPort))
	ln, err := client.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", vmcache.NodePort))
	if err != nil {
		done()
		closeBucket()
		options.BuildLogger.Warningln("Cache in archives: the VM can't reach the node: " + err.Error())
		return func() {}
	}
	srv := &http.Server{Handler: &gitlabcache.Signer{B: bucket, J: job, Log: log}, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(ln) }()

	// The build's config is its own copy, which its cache's links come
	// from; the helper writes the archive the VM's service reads.
	rc.Cache = cfg
	rc.Environment = withTarZstd(rc.Environment)
	build.RefreshAllVariables()
	options.BuildLogger.Println("Cache in the bucket file by file, through the VM")
	return func() {
		_ = srv.Close()
		done()
		closeBucket()
	}
}

// withTarZstd is env with the helper's archives in tar.zst, the format the
// VM's service reads, in place of the runner's own.
func withTarZstd(env []string) []string {
	out := slices.DeleteFunc(slices.Clone(env), func(e string) bool { return strings.HasPrefix(e, "CACHE_COMPRESSION_FORMAT=") })
	return append(out, "CACHE_COMPRESSION_FORMAT=tarzstd")
}

// startJobCache is fileCache on the job's VM.
func (e *executor) startJobCache(options common.ExecutorPrepareOptions, acqRef *acquisitionRef, vm source.VM) func() {
	conn, err := acqRef.conn(vm.ID)
	if err != nil {
		options.BuildLogger.Warningln("Cache in archives: " + err.Error())
		return nil
	}
	client, _, err := conn.Client(options.Context)
	if err != nil {
		options.BuildLogger.Warningln("Cache in archives: " + err.Error())
		return nil
	}
	return e.fileCache(options, client, hclog.Default().Named("gitlab-cache"))
}

// startFileCache starts the cache's service on the VM and is the address
// the VM's containers reach it at: Docker's bridge.
func startFileCache(ctx context.Context, client *ssh.Client) (string, error) {
	bin, err := github.InstallCacheBinary(ctx, client)
	if err != nil {
		return "", err
	}
	// A firewall that drops what comes in lets the containers through to
	// it, and only it.
	cmd := fmt.Sprintf(`f="$HOME/%[1]s"; mkdir -p "$HOME/.fleetci"
if command -v iptables >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
  for i in docker0 br-+; do sudo -n iptables -I INPUT -i "$i" -p tcp --dport %[2]d -j ACCEPT 2>/dev/null || true; done
fi
nohup setsid "$f" gitlab %[2]d http://127.0.0.1:%[3]d >> "$HOME/.fleetci/gitlab-cache.log" 2>&1 < /dev/null &
ip -4 -o addr show docker0 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -n 1`, bin, vmcache.VMPort, vmcache.NodePort)
	s, err := client.NewSession()
	if err != nil {
		return "", err
	}
	out, err := s.Output(cmd)
	s.Close()
	if err != nil {
		return "", fmt.Errorf("starting the VM's cache: %w", err)
	}
	at := string(bytes.TrimSpace(out))
	if net.ParseIP(at) == nil {
		return "", fmt.Errorf("the VM has no Docker bridge (%q)", at)
	}
	// It answers, over the VM's loopback.
	hc := &http.Client{Timeout: time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return client.DialContext(ctx, "tcp", addr)
		},
	}}
	defer hc.CloseIdleConnections()
	deadline := time.Now().Add(fileCacheReady)
	for {
		resp, err := hc.Get(fmt.Sprintf("http://127.0.0.1:%d/fleetci/ping", vmcache.VMPort))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return at, nil
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return "", fmt.Errorf("the VM's cache didn't start: %v", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
}

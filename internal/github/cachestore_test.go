package github

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/vmjob/vmjobtest"
)

// freePort is a port nothing listens on now.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// The VM reaches its node's cache service through the VM's connection,
// and after the runner the node asks the VM's actions cache to upload the job's
// saves, before the job's disk is taken back.
func TestTheNodesCacheOverTheVMsConnection(t *testing.T) {
	port := freePort(t)
	old, oldFlush := nodeCachePort, flushURL
	t.Cleanup(func() { nodeCachePort, flushURL = old, oldFlush })
	nodeCachePort = port

	var lookups, flushes atomic.Int32
	node := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookups.Add(1)
		_, _ = w.Write([]byte("node"))
	})
	ac := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/fleetci/flush" {
			flushes.Add(1)
			_, _ = w.Write([]byte(`{"uploaded":1}`))
		}
	}))
	t.Cleanup(ac.Close)
	flushURL = ac.URL + "/fleetci/flush"

	vm := vmjobtest.New(t)
	runner := `read -r jit; read -r hook; curl -fsS -X POST http://127.0.0.1:` + itoa(port) + `/lookup > "$HOME/answer" 2> "$HOME/err"; cat "$HOME/err"; echo "Running job: build"`
	v := vmFor(vmConn(t, vm.Addr))
	v.Cache = true
	if err := startWith(runner, node, hclog.NewNullLogger())(context.Background(), v, "jit", nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(vm.Path("answer")); string(b) != "node" || lookups.Load() != 1 {
		e, _ := os.ReadFile(vm.Path("err"))
		t.Log(string(e))
		t.Fatalf("the VM got %q from the node, %d calls", b, lookups.Load())
	}
	if flushes.Load() != 1 {
		t.Fatalf("flushed %d times, want once", flushes.Load())
	}
	// A VM without a cache disk, with the user's bucket, goes to it
	// through its actions cache too: it flushes the job's saves.
	v.Cache = false
	if err := startWith(`read -r jit; echo "Running job: build"`, node, hclog.NewNullLogger())(context.Background(), v, "jit", nil); err != nil {
		t.Fatal(err)
	}
	if flushes.Load() != 2 {
		t.Errorf("a VM without a cache disk, with the bucket, flushed %d times in all, want 2", flushes.Load())
	}
	// Without the bucket there's no node service, and nothing to flush.
	if err := startWith(`read -r jit; echo "Running job: build"`, nil, hclog.NewNullLogger())(context.Background(), v, "jit", nil); err != nil {
		t.Fatal(err)
	}
	if flushes.Load() != 2 {
		t.Errorf("a VM without a cache disk or a bucket flushed")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// A [github] table's bucket is gitlab-runner's [runners.cache], keys and all.
func TestCacheStoreConfig(t *testing.T) {
	var cfg struct{ GitHub Config }
	_, err := toml.Decode(`
[github]
cache_store = "bucket"
[github.cache_bucket]
Type = "s3"
Path = "ci"
[github.cache_bucket.s3]
ServerAddress = "127.0.0.1:9000"
BucketName = "caches"
AccessKey = "test-access"
SecretKey = "test-secret"
Insecure = true
`, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.GitHub
	cs, err := NewCacheStore(g.CacheStore, g.CacheURL, g.CacheBucket, g.CacheTokenIssuer, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cs.bucket == nil || cs.actionsCacheArgs() != "remote=bucket node=http://127.0.0.1:"+itoa(nodeCachePort)+" " {
		t.Fatalf("store %+v, args %q", cs, cs.actionsCacheArgs())
	}
	l, err := cs.bucket.bucket.Upload(context.Background(), "github/1/x/v1/k", linkTTL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(l.URL, "http://127.0.0.1:9000/caches/ci/github/1/x/v1/k?") || !strings.Contains(l.URL, "X-Amz-Signature=") {
		t.Errorf("upload link %s", l.URL)
	}

	for name, c := range map[string][3]string{
		"an unknown store":         {"s3", "", ""},
		"a bucket without a table": {"bucket", "", ""},
		"cache_url and the disk":   {"off", "https://cache.example/", ""},
		"cache_url isn't an URL":   {"github", "cache.example", ""},
		"cache_url with shell":     {"github", "https://x/'$(id)'", ""},
	} {
		if _, err := NewCacheStore(c[0], c[1], nil, c[2], nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := NewCacheStore("github", "", g.CacheBucket, "", nil); err == nil {
		t.Error("a bucket with the github store: accepted")
	}
	cs, err = NewCacheStore("", "https://cache.example", nil, "", nil)
	if err != nil || cs.Store != StoreGitHub || cs.URL != "https://cache.example/" || cs.actionsCacheArgs() != "" {
		t.Errorf("the default: %+v %v", cs, err)
	}
	if cs, _ := NewCacheStore("off", "", nil, "", nil); cs.actionsCacheArgs() != "remote=off " {
		t.Errorf("off: %q", cs.actionsCacheArgs())
	}
}

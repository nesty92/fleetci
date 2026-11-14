package github

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/nesty92/fleetci/internal/cachebucket"
	"gitlab.com/gitlab-org/gitlab-runner/cache/cacheconfig"
)

// TestActionsCacheBucketS3 is TestActionsCacheBucket against a real S3 server, with
// its signed links: FLEETCI_TEST_S3=host:port, and the keys in
// AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY. FLEETCI_TEST_S3_BUCKET
// names the bucket, made if missing; each run writes under a prefix of
// its own and removes it.
func TestActionsCacheBucketS3(t *testing.T) {
	addr := os.Getenv("FLEETCI_TEST_S3")
	if addr == "" {
		t.Skip("FLEETCI_TEST_S3 is unset")
	}
	name := os.Getenv("FLEETCI_TEST_S3_BUCKET")
	if name == "" {
		name = "fleetci-test"
	}
	ctx := context.Background()
	raw := rawS3(t, addr)
	if _, err := raw.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &name}); err != nil {
		if _, err := raw.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &name}); err != nil {
			t.Fatal(err)
		}
	}
	run := make([]byte, 6)
	_, _ = rand.Read(run)
	path := "fleetci-test-" + hex.EncodeToString(run)
	t.Cleanup(func() { emptyPrefix(t, raw, name, path+"/") })

	b, err := cachebucket.New(&cachebucket.Config{Type: "s3", Path: path, S3: &cacheconfig.CacheS3Config{
		ServerAddress: addr, BucketName: name, Insecure: true,
		AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"), SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.(interface{ Close() }).Close)
	is := newTestIssuer(t, true)
	node := bucketNode(t, b, is.srv.URL)
	l2 := newFakeL2(t)
	bare := func(tok string) string { return strings.TrimPrefix(tok, "Bearer ") }
	onMain := bare(is.token(t, nil))
	onFeature := bare(is.token(t, map[string]any{"ac": `[{"Scope":"refs/heads/feature","Permission":3},{"Scope":"refs/heads/main","Permission":1}]`}))
	big := bytes.Repeat([]byte("fleetci-"), 2_000_000)

	vm1 := startActionsCache(t, l2.srv.URL+"/", filepath.Join(t.TempDir(), "actions"), true, "remote=bucket", "node="+node)
	if code := saveAs(t, vm1, onMain, "go-linux-aaa", big); code != http.StatusOK {
		t.Fatalf("save: %d", code)
	}
	if got := flush(t, vm1); got["uploaded"] != 1 {
		t.Fatalf("flush: %v", got)
	}
	objs, err := b.List(ctx, "github/")
	if err != nil || len(objs) != 1 || objs[0].Name != "github/1/refs%2Fheads%2Fmain/v1/go-linux-aaa" || objs[0].Size != int64(len(big)) {
		t.Fatalf("the bucket has %+v %v", objs, err)
	}

	vm2 := startActionsCache(t, l2.srv.URL+"/", filepath.Join(t.TempDir(), "actions"), true, "remote=bucket", "node="+node)
	key, data, url := restoreData(t, vm2, onFeature, "go-linux-bbb", "go-linux-")
	if key != "go-linux-aaa" || !bytes.Equal(data, big) || !strings.Contains(url, "/fleetci/fill/") {
		t.Fatalf("restore from the bucket: %q, %d bytes, %s", key, len(data), url)
	}
	if key, _, url = restoreData(t, vm2, onFeature, "go-linux-aaa"); key != "go-linux-aaa" || !strings.Contains(url, "/fleetci/blob/") {
		t.Fatalf("restore again: %q %s", key, url)
	}
	vm3 := startActionsCache(t, l2.srv.URL+"/", filepath.Join(t.TempDir(), "actions"), true, "remote=bucket", "node="+node)
	if code := saveAs(t, vm3, onMain, "go-linux-aaa", []byte("again")); code != http.StatusOK {
		t.Fatalf("save again: %d", code)
	}
	if got := flush(t, vm3); got["exists"] != 1 {
		t.Errorf("flush of a saved entry: %v", got)
	}
	if l2.gets.Load() != 0 || len(l2.entries) != 0 {
		t.Error("GitHub's cache got cache calls")
	}

	// A scope with more entries than one page of a listing (1000): the
	// entry is on the second page.
	dir := path + "/" + entryDir("1", "refs/heads/many", "v1")
	sem := make(chan struct{}, 32)
	var wg sync.WaitGroup
	for i := range 1100 {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			k := fmt.Sprintf("%sk-%04d", dir, i)
			if _, err := raw.PutObject(ctx, &s3.PutObjectInput{Bucket: &name, Key: &k, Body: strings.NewReader("x")}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	many := bare(is.token(t, map[string]any{"ac": `[{"Scope":"refs/heads/many","Permission":3}]`}))
	if key, data, _ := restoreData(t, vm2, many, "k-1099"); key != "k-1099" || string(data) != "x" {
		t.Fatalf("an entry on the listing's second page: %q", key)
	}
	if key, _, _ := restoreData(t, vm2, many, "nope", "k-10"); !strings.HasPrefix(key, "k-10") {
		t.Fatalf("a prefix across pages: %q", key)
	}
}

func rawS3(t *testing.T, addr string) *s3.Client {
	t.Helper()
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), "")))
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String("http://" + addr)
		o.UsePathStyle = true
		o.HTTPClient = &http.Client{Transport: tr}
	})
}

func emptyPrefix(t *testing.T, c *s3.Client, bucket, prefix string) {
	ctx := context.Background()
	p := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{Bucket: &bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			t.Logf("cleaning up: %v", err)
			return
		}
		for _, o := range page.Contents {
			if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: o.Key}); err != nil {
				var nf *types.NoSuchKey
				if !errors.As(err, &nf) {
					t.Logf("cleaning up %s: %v", aws.ToString(o.Key), err)
				}
			}
		}
	}
}

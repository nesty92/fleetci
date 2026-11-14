package filestore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/nesty92/fleetci/internal/cachebucket"
)

// Linked is a Remote over a cache bucket's signed links: what a VM can be
// given, never the bucket's credentials.
type Linked struct {
	B cachebucket.Bucket
	C *http.Client
}

const linkTTL = 10 * time.Minute

func (l Linked) client() *http.Client {
	if l.C != nil {
		return l.C
	}
	return http.DefaultClient
}

func (l Linked) List(ctx context.Context, prefix string) ([]string, error) {
	objs, err := l.B.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(objs))
	for i, o := range objs {
		out[i] = o.Name
	}
	return out, nil
}

func (l Linked) Put(ctx context.Context, name string, body []byte) error {
	k, err := l.B.Upload(ctx, name, linkTTL)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, k.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for h, v := range k.Headers {
		req.Header.Set(h, v)
	}
	req.ContentLength = int64(len(body))
	return l.do(req, nil)
}

func (l Linked) Get(ctx context.Context, name string, off, n int64) ([]byte, error) {
	k, err := l.B.Download(ctx, name, linkTTL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.URL, nil)
	if err != nil {
		return nil, err
	}
	for h, v := range k.Headers {
		req.Header.Set(h, v)
	}
	if n >= 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1))
	}
	var out []byte
	err = l.do(req, &out)
	if err == nil && n >= 0 && int64(len(out)) != n {
		err = fmt.Errorf("GET %s: %d bytes for a range of %d", name, len(out), n)
	}
	return out, err
}

func (l Linked) do(req *http.Request, body *[]byte) error {
	resp, err := l.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %s %s", req.Method, req.URL.Path, resp.Status, b)
	}
	if body == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	*body, err = io.ReadAll(resp.Body)
	return err
}

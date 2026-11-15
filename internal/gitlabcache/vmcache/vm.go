package vmcache

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/nesty92/fleetci/internal/cachelink"
	"github.com/nesty92/fleetci/internal/filestore"
)

// VM is the service on the job's VM that GitLab's helper sends its cache
// to and takes it from, at /gitlab/<the cache's name>:
//
//   - GET restores the snapshot as one tar.zst, its files read straight
//     from their packs; HEAD says when it was saved, as the helper asks;
//   - PUT takes the helper's tar.zst and saves it file by file, uploading
//     only the contents the store lacks.
//
// Its links to the bucket are the node's, at Node.
type VM struct {
	Node string // the node's signer, http://127.0.0.1:NodePort
	C    *http.Client
	Log  *log.Logger
}

func (v *VM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutPrefix(r.URL.Path, "/gitlab/")
	prefix, key, err := Store(name)
	if !ok || err != nil {
		http.Error(w, "want /gitlab/<cache name>", http.StatusNotFound)
		return
	}
	s := filestore.New(filestore.Linked{B: nodeBucket{v}, C: v.C}, prefix)
	var st filestore.Stats
	start := time.Now()
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		snap, err := s.Snap(r.Context(), key, &st)
		if errors.Is(err, filestore.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			v.logf("restore %s: %v", name, err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Last-Modified", time.Unix(0, snap.Taken).UTC().Format(http.TimeFormat))
		if r.Method == http.MethodHead {
			return
		}
		// A range, as the helper asks to see whether it may download in
		// parts, gets the whole: a stream has no length before its end.
		if err := s.Stream(r.Context(), snap, w, &st); err != nil {
			v.logf("restore %s: %v", name, err)
			return
		}
		v.logf("restored %s: %d files, %.1f MB from %d gets in %s", name, len(snap.Entries), float64(st.GetBytes)/1e6, st.Get, time.Since(start).Round(time.Millisecond))
	case http.MethodPut:
		br := bufio.NewReader(r.Body)
		if magic, _ := br.Peek(4); !bytes.Equal(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}) {
			http.Error(w, "want a tar.zst (CACHE_COMPRESSION_FORMAT=tarzstd)", http.StatusUnsupportedMediaType)
			return
		}
		zr, err := zstd.NewReader(br)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer zr.Close()
		if err := s.Load(r.Context(), &st); err != nil && !errors.Is(err, filestore.ErrNotFound) {
			v.logf("save %s: %v", name, err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		_, sst, err := s.SaveTar(r.Context(), zr, key)
		if err != nil {
			v.logf("save %s: %v", name, err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		v.logf("saved %s: %d files, %d new (%.1f MB), %.1f MB up in %s", name, sst.Files, sst.NewFiles, float64(sst.NewBytes)/1e6, float64(sst.PutBytes)/1e6, time.Since(start).Round(time.Millisecond))
	default:
		http.Error(w, "GET, HEAD or PUT", http.StatusMethodNotAllowed)
	}
}

func (v *VM) logf(format string, args ...any) {
	if v.Log != nil {
		v.Log.Printf(format, args...)
	}
}

// nodeBucket is the bucket through the node's signer.
type nodeBucket struct{ v *VM }

func (b nodeBucket) call(ctx context.Context, op, name string) (SignResp, error) {
	var out SignResp
	body, _ := json.Marshal(SignReq{Op: op, Name: name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.v.Node+"/sign", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	c := b.v.C
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return out, fmt.Errorf("the node: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return out, fmt.Errorf("the node: %s %s: %s %s", op, name, resp.Status, bytes.TrimSpace(msg))
	}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

func (b nodeBucket) List(ctx context.Context, prefix string) ([]cachelink.Object, error) {
	out, err := b.call(ctx, "list", prefix)
	objs := make([]cachelink.Object, len(out.Names))
	for i, n := range out.Names {
		objs[i].Name = n
	}
	return objs, err
}

func (b nodeBucket) Download(ctx context.Context, name string, _ time.Duration) (cachelink.Link, error) {
	out, err := b.call(ctx, "get", name)
	if err == nil && out.Link == nil {
		err = errors.New("the node: no link")
	}
	if err != nil {
		return cachelink.Link{}, err
	}
	return *out.Link, nil
}

func (b nodeBucket) Upload(ctx context.Context, name string, _ time.Duration) (cachelink.Link, error) {
	out, err := b.call(ctx, "put", name)
	if err == nil && out.Link == nil {
		err = errors.New("the node: no link")
	}
	if err != nil {
		return cachelink.Link{}, err
	}
	return *out.Link, nil
}

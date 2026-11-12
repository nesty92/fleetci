package cluster

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/klauspost/compress/s2"

	"github.com/nesty92/fleetci/internal/cachedisk"
)

// Workers copy each other's shared caches (see package cachedisk): each
// offers its own to the neighbours it reaches (peers.go), and every
// shareEvery copies their newer copies of the caches it has, and those it
// lacks while it has room to spare. A layer comes over the peers' mutual
// TLS with its SHA-256 after it, and is kept only when they agree.

// shareEvery is how often a worker looks at its neighbours' caches.
const shareEvery = 30 * time.Second

// layerSum is the trailer that carries a layer's SHA-256, of the layer
// as it is on disk.
const layerSum = "Fleetci-Sha256"

// A layer goes compressed with S2 when the worker asking takes it: cache
// layers hold unpacked files, and come to about half. S2 keeps up with
// 10 Gbit on two cores, so it never slows a copy much, and leaves the
// rest to the VMs.
const (
	encS2         = "s2"
	s2Concurrency = 2
)

// compressing says whether this worker asks for layers compressed.
func (w *Worker) compressing() bool { return w.cfg.ShareCompress == nil || *w.cfg.ShareCompress }

// sharing says whether this worker copies caches to and from others.
func (w *Worker) sharing() bool { return w.cfg.ShareCaches == nil || *w.cfg.ShareCaches }

func (w *Worker) cacheStores() []*cachedisk.Store {
	w.mu.Lock()
	b := w.backend
	w.mu.Unlock()
	if b == nil || !w.sharing() {
		return nil
	}
	return b.CacheStores()
}

// offers are the shared caches of stores, the newest copy of each.
func offers(stores []*cachedisk.Store) []cachedisk.Offer {
	byKey := map[string]cachedisk.Offer{}
	for _, s := range stores {
		for _, o := range s.Shared() {
			if cur, ok := byKey[o.Key]; !ok || o.Committed.After(cur.Committed) {
				byKey[o.Key] = o
			}
		}
	}
	out := make([]cachedisk.Offer, 0, len(byKey))
	for _, o := range byKey {
		out = append(out, o)
	}
	slices.SortFunc(out, func(a, b cachedisk.Offer) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// shareHandlers answer neighbours' requests for caches.
func shareHandlers(mux *http.ServeMux, stores func() []*cachedisk.Store) {
	mux.HandleFunc("GET /peer/v1/caches", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, offers(stores()))
	})
	mux.HandleFunc("GET /peer/v1/layers/{key}/{name}", func(w http.ResponseWriter, r *http.Request) {
		path := r.PathValue("key") + "/" + r.PathValue("name")
		for _, s := range stores() {
			f, err := s.OpenLayer(path)
			if err != nil {
				continue
			}
			defer f.Close()
			// No Content-Length: over HTTP/1.1 a trailer needs a chunked
			// body. The offer has the size, and Pull checks it.
			w.Header().Set("Trailer", layerSum)
			w.Header().Set("Content-Type", "application/octet-stream")
			var body io.Writer = w
			var zw *s2.Writer
			if slices.Contains(strings.Split(r.Header.Get("Accept-Encoding"), ","), encS2) {
				w.Header().Set("Content-Encoding", encS2)
				zw = s2.NewWriter(w, s2.WriterConcurrency(s2Concurrency))
				body = zw
			}
			h := sha256.New()
			if _, err := io.Copy(io.MultiWriter(body, h), f); err != nil {
				return // the neighbour sees a short body
			}
			if zw != nil && zw.Close() != nil {
				return
			}
			w.Header().Set(layerSum, hex.EncodeToString(h.Sum(nil)))
			return
		}
		http.Error(w, "no such layer", http.StatusNotFound)
	})
}

// peerClient reaches neighbour h, for requests that may take as long as
// a layer does.
func peerClient(w *Worker, pool *x509.CertPool, h PeerHint) *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig:       peerClientTLS(w.pki, pool, h),
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       time.Minute,
	}}
}

// neighbourOffers asks a neighbour for its shared caches.
func neighbourOffers(ctx context.Context, hc *http.Client, addr string) ([]cachedisk.Offer, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+"/peer/v1/caches", nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", addr, resp.Status)
	}
	var out []cachedisk.Offer
	return out, json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&out)
}

// countReader counts what it reads.
type countReader struct {
	r io.Reader
	n *int64
}

func (c countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.n += int64(n)
	return n, err
}

// fetchLayer copies a neighbour's layer to dst, and checks it against the
// SHA-256 the neighbour sends after it. It asks for it compressed when
// compress is set, and adds what came over the network to wire.
func fetchLayer(hc *http.Client, addr string, compress bool, wire *int64) cachedisk.Fetch {
	return func(ctx context.Context, path string, dst io.Writer) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+"/peer/v1/layers/"+path, nil)
		if err != nil {
			return err
		}
		if compress {
			req.Header.Set("Accept-Encoding", encS2)
		}
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s: %s", path, resp.Status)
		}
		var body io.Reader = countReader{resp.Body, wire}
		switch enc := resp.Header.Get("Content-Encoding"); enc {
		case "":
		case encS2:
			body = s2.NewReader(body)
		default:
			return fmt.Errorf("%s: encoding %q", path, enc)
		}
		h := sha256.New()
		if _, err := io.Copy(io.MultiWriter(dst, h), body); err != nil {
			return err
		}
		want := resp.Trailer.Get(layerSum)
		if got := hex.EncodeToString(h.Sum(nil)); want == "" || got != want {
			return errors.New("its SHA-256 doesn't match the neighbour's")
		}
		return nil
	}
}

// shareLoop copies the neighbours' caches every shareEvery, and when the
// neighbours change.
func (w *Worker) shareLoop(ctx context.Context, pool *x509.CertPool) {
	t := time.NewTicker(shareEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.shareKick:
		}
		w.shareOnce(ctx, pool)
	}
}

// candidate is a neighbour's copy of a cache.
type candidate struct {
	o    cachedisk.Offer
	near Neighbour
	hint PeerHint
}

// shareOnce copies what the neighbours have newer: first the caches this
// worker has, then those it lacks, the most recently used first.
func (w *Worker) shareOnce(ctx context.Context, pool *x509.CertPool) {
	stores := w.cacheStores()
	near := w.peers.list()
	if len(stores) == 0 || len(near) == 0 {
		return
	}
	w.peers.mu.Lock()
	hints := map[string]PeerHint{}
	for _, h := range w.peers.unionLocked() {
		hints[h.ID] = h
	}
	w.peers.mu.Unlock()
	slices.SortFunc(near, func(a, b Neighbour) int { return int(a.RTT - b.RTT) })

	best := map[string]candidate{}
	clients := map[string]*http.Client{}
	defer func() {
		for _, hc := range clients {
			hc.CloseIdleConnections()
		}
	}()
	for _, n := range near {
		h, ok := hints[n.ID]
		if !ok {
			continue
		}
		hc := peerClient(w, pool, h)
		clients[n.ID] = hc
		offs, err := neighbourOffers(ctx, hc, n.Addr)
		if err != nil {
			w.log.Debug("no caches from a neighbour", "peer", n.ID, "err", err)
			continue
		}
		for _, o := range offs {
			// The nearest of the newest.
			if c, ok := best[o.Key]; !ok || o.Committed.After(c.o.Committed) {
				best[o.Key] = candidate{o, n, h}
			}
		}
	}

	type pull struct {
		c     candidate
		store *cachedisk.Store
		has   bool
	}
	var pulls []pull
	for _, c := range best {
		var into *cachedisk.Store
		has := false
		for _, s := range stores {
			if committed, ok := s.Has(c.o.Key); ok {
				into, has = s, true
				if !committed.Before(c.o.Committed) {
					into = nil
				}
				break
			}
		}
		if !has {
			into = stores[0]
		}
		if into != nil {
			pulls = append(pulls, pull{c, into, has})
		}
	}
	slices.SortFunc(pulls, func(a, b pull) int {
		if a.has != b.has {
			if a.has {
				return -1
			}
			return 1
		}
		return b.c.o.Used.Compare(a.c.o.Used)
	})

	copied := false
	for _, p := range pulls {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		var wire int64
		n, err := p.store.Pull(ctx, p.c.o, fetchLayer(clients[p.c.near.ID], p.c.near.Addr, w.compressing(), &wire))
		name := p.c.o.Scope.Key
		if name == "" {
			name = p.c.o.Scope.Ref
		}
		switch {
		case errors.Is(err, cachedisk.ErrNoRoom):
			w.log.Debug("no room for a neighbour's cache", "cache", name, "peer", p.c.near.ID)
		case err != nil:
			w.log.Warn("cache not copied from a neighbour", "cache", name, "project", p.c.o.Scope.Project, "peer", p.c.near.ID, "err", err)
		case n > 0:
			copied = true
			took := time.Since(start)
			w.log.Info("cache copied from a neighbour", "cache", name, "project", p.c.o.Scope.Project, "peer", p.c.near.ID,
				"refresh", p.has, "size", fmt.Sprintf("%.1fMB", float64(n)/(1<<20)),
				"wire", fmt.Sprintf("%.1fMB", float64(wire)/(1<<20)), "took", took.Round(time.Millisecond).String(),
				"rate", fmt.Sprintf("%.0fMB/s", float64(n)/(1<<20)/max(took.Seconds(), 0.001)))
		}
	}
	if copied {
		w.poke() // the report's caches change
	}
}

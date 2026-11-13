package cachedisk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// Nodes on one network copy each other's shared caches, the ones writers
// make (by default, jobs of protected refs): never a ref's own. A node
// offers its shared caches, layer by layer (Shared, OpenLayer), and copies
// the layers it lacks of a neighbour's newer copy (Pull). Layers never
// change once kept, and their names are random, so one a node has is the
// same as the neighbour's of that name: a newer copy costs its new layers.

// Offer is a shared cache, as a node offers it to its neighbours.
type Offer struct {
	// Key is its directory, the same on every node (Scope.Dirs).
	Key   string `json:"key"`
	Scope Scope  `json:"scope"`
	// Used and Committed are as in its head.
	Used      time.Time `json:"used"`
	Committed time.Time `json:"committed"`
	// Layers are its chain, oldest first.
	Layers []OfferLayer `json:"layers"`
}

// OfferLayer is a layer of an offer: "<key>/<name>.qcow2" in the store,
// and its size.
type OfferLayer struct {
	Path string `json:"path"`
	// Size is the layer's length, what a copy of it checks.
	Size int64 `json:"size"`
	// Stored is what it takes on the disk: less than Size where a job's
	// trim left holes. Zero from a node before it was sent.
	Stored int64 `json:"stored,omitempty"`
}

// stored is what the layer takes on the disk here once copied.
func (l OfferLayer) stored() int64 {
	if l.Stored > 0 {
		return l.Stored
	}
	return l.Size
}

// Size is what keeping all of it takes.
func (o Offer) Size() int64 {
	var n int64
	for _, l := range o.Layers {
		n += l.stored()
	}
	return n
}

var layerPath = regexp.MustCompile(`^[0-9a-f]{24}/[0-9a-f]{16}\.qcow2$`)

// Shared are the store's shared caches, newest first. A cache kept before
// nodes noted when (no Committed) isn't offered until its next job.
func (s *Store) Shared() []Offer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sharedLocked()
}

func (s *Store) sharedLocked() []Offer {
	var out []Offer
	scopes := filepath.Join(s.dir, "scopes")
	for _, key := range s.keys() {
		h, err := s.readHead(key)
		if err != nil || !h.Scope.Writer || h.Scope.key() != key || h.Committed.IsZero() {
			continue
		}
		o := Offer{Key: key, Scope: h.Scope, Used: h.Used, Committed: h.Committed}
		for _, f := range h.Chain {
			rel, err := filepath.Rel(scopes, f)
			st, serr := os.Stat(f)
			if err != nil || serr != nil || !layerPath.MatchString(rel) {
				o.Layers = nil
				break
			}
			o.Layers = append(o.Layers, OfferLayer{Path: rel, Size: st.Size(), Stored: allocated(fs.FileInfoToDirEntry(st))})
		}
		if len(o.Layers) > 0 {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Committed.After(out[j].Committed) })
	return out
}

// OpenLayer opens a layer of a shared cache, by its offer's path; none
// other.
func (s *Store) OpenLayer(path string) (*os.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.sharedLocked() {
		for _, l := range o.Layers {
			if l.Path == path {
				return os.Open(filepath.Join(s.dir, "scopes", path))
			}
		}
	}
	return nil, fmt.Errorf("no shared layer %q", path)
}

// Has is when the store's copy of a cache was committed, if it has one.
func (s *Store) Has(key string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := s.readHead(key)
	if err != nil {
		return time.Time{}, false
	}
	return h.Committed, true
}

// Fetch writes a neighbour's layer, by its offer's path, to w. It must
// check what it copies, as Pull trusts it.
type Fetch func(ctx context.Context, path string, w io.Writer) error

// ErrNoRoom: a cache the store lacks doesn't fit beside what it has.
var ErrNoRoom = errors.New("no room for it")

// Pull copies a neighbour's newer copy of a shared cache: the layers the
// store lacks, by fetch. A cache the store has replaces its copy, and may
// evict others to fit; one it lacks is copied only into room it has
// spare, and takes the neighbour's last use, so it goes before the caches
// jobs here use. It says how much it copied; nothing when the store's
// copy is as new.
func (s *Store) Pull(ctx context.Context, o Offer, fetch Fetch) (int64, error) {
	if !o.Scope.Writer || o.Scope.key() != o.Key || len(o.Layers) == 0 || o.Committed.IsZero() {
		return 0, errors.New("not a shared cache")
	}
	scopes := filepath.Join(s.dir, "scopes")
	chain := make([]string, len(o.Layers))
	for i, l := range o.Layers {
		if !layerPath.MatchString(l.Path) {
			return 0, fmt.Errorf("layer %q", l.Path)
		}
		chain[i] = filepath.Join(scopes, l.Path)
	}
	if filepath.Base(filepath.Dir(chain[len(chain)-1])) != o.Key {
		return 0, errors.New("its newest layer isn't its own")
	}

	s.mu.Lock()
	local, err := s.readHead(o.Key)
	if err == nil && !local.Committed.Before(o.Committed) {
		s.mu.Unlock()
		return 0, nil
	}
	if c, ok := s.declined[o.Key]; ok && !o.Committed.After(c) {
		s.mu.Unlock()
		return 0, nil
	}
	if o.Size() > s.cfg.MaxPerCache {
		s.declined[o.Key] = o.Committed
		s.mu.Unlock()
		return 0, fmt.Errorf("%d bytes, over max_per_cache", o.Size())
	}
	var need int64
	var missing []int
	for i, f := range chain {
		if _, err := os.Stat(f); err != nil {
			missing = append(missing, i)
			need += o.Layers[i].stored()
		}
	}
	// Held as a lease, so sweep and evict leave the chain while it comes.
	hold := &Lease{chain: chain}
	s.leases[hold] = struct{}{}
	for _, i := range missing {
		s.pulling[chain[i]+".part"] = true
	}
	release := func() {
		delete(s.leases, hold)
		for _, i := range missing {
			delete(s.pulling, chain[i]+".part")
			_ = os.Remove(chain[i] + ".part")
		}
		s.sweep()
	}
	if local != nil {
		s.evictFor(need)
	}
	if !s.fitsWith(need) {
		release()
		s.mu.Unlock()
		return 0, ErrNoRoom
	}
	s.mu.Unlock()

	err = s.fetchLayers(ctx, chain, missing, o, fetch)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		release()
		return 0, err
	}
	used := o.Used
	local, lerr := s.readHead(o.Key)
	switch {
	case lerr == nil && !local.Committed.Before(o.Committed):
		// A job here kept a newer copy meanwhile.
		release()
		return 0, nil
	case lerr == nil:
		used = local.Used
	}
	h := &head{Scope: o.Scope, Chain: chain, Used: used, Committed: o.Committed}
	if err := s.writeHead(o.Key, h); err != nil {
		release()
		return 0, err
	}
	// A neighbour's chain may be deep: flattened here before a job needs it.
	s.keepShallow(o.Key, h)
	release()
	s.evict()
	return need, nil
}

// fetchLayers copies the missing layers of chain, each to a .part file,
// points it at its parent here, and puts it in place.
func (s *Store) fetchLayers(ctx context.Context, chain []string, missing []int, o Offer, fetch Fetch) error {
	for _, i := range missing {
		f := chain[i]
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			return err
		}
		part := f + ".part"
		out, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		sw := &sparseWriter{f: out, max: o.Layers[i].Size}
		err = fetch(ctx, o.Layers[i].Path, sw)
		if err == nil {
			err = sw.finish()
		}
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err == nil && sw.off != o.Layers[i].Size {
			err = fmt.Errorf("%s: %d bytes, not %d", o.Layers[i].Path, sw.off, o.Layers[i].Size)
		}
		if err == nil && i > 0 {
			err = s.img.Rebase(part, chain[i-1])
		}
		if err == nil {
			err = os.Rename(part, f)
		}
		if err != nil {
			return fmt.Errorf("copying %s: %w", o.Layers[i].Path, err)
		}
	}
	return nil
}

// sparseWriter writes a file, leaving holes where the data is zeros:
// layers are sparse.
type sparseWriter struct {
	f   *os.File
	off int64
	max int64 // the layer's size: a neighbour sending more is refused
}

var errTooLong = errors.New("longer than offered")

const sparseBlock = 64 << 10

func (w *sparseWriter) Write(p []byte) (int, error) {
	if w.off+int64(len(p)) > w.max {
		return 0, errTooLong
	}
	n := len(p)
	for len(p) > 0 {
		b := p[:min(len(p), sparseBlock)]
		if !zeros(b) {
			if _, err := w.f.WriteAt(b, w.off); err != nil {
				return n - len(p), err
			}
		}
		w.off += int64(len(b))
		p = p[len(b):]
	}
	return n, nil
}

// finish sets the file's size, holes at its end included.
func (w *sparseWriter) finish() error { return w.f.Truncate(w.off) }

var zeroBlock = make([]byte, sparseBlock)

func zeros(b []byte) bool { return bytes.Equal(b, zeroBlock[:len(b)]) }

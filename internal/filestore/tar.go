package filestore

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

// SaveTar saves as name the folder a tar holds, as GitLab's cache
// archiver writes one: the files of the job's cache paths, by their paths
// under the project. Every file is hashed for its bytes, and only the
// contents the bucket lacks are packed; nothing is written to a disk.
// Folders the tar names only in its files' paths are added. r is the tar,
// uncompressed. A later entry of a path takes the place of an earlier.
func (s *Store) SaveTar(ctx context.Context, r io.Reader, name string) (*Snapshot, Stats, error) {
	start := time.Now()
	var st Stats
	codecs()
	p := newPacker(ctx, s, &st)
	tp := newPacker(ctx, s, &st)
	var pmu sync.Mutex
	pending := map[string]bool{}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(Workers)

	byPath := map[string]*Entry{}
	tr := tar.NewReader(r)
	var rerr error
	for gctx.Err() == nil {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			rerr = fmt.Errorf("the tar: %w", err)
			break
		}
		rel, err := tarPath(h.Name)
		if err != nil {
			rerr = err
			break
		}
		if rel == "" {
			continue // the project's own folder
		}
		mode := h.FileInfo().Mode()
		e := &Entry{Path: rel, Mode: mode, MTime: h.ModTime.UnixNano()}
		switch {
		case mode.IsRegular():
			b := make([]byte, h.Size)
			if _, err := io.ReadFull(tr, b); err != nil {
				rerr = fmt.Errorf("the tar: %s: %w", rel, err)
				break
			}
			e.Size = h.Size
			g.Go(func() error {
				sum := sha256.Sum256(b)
				e.Hash = hex.EncodeToString(sum[:])
				atomic.AddInt64(&st.Hashed, int64(len(b)))
				s.mu.Lock()
				_, have := s.idx[e.Hash]
				s.mu.Unlock()
				pmu.Lock()
				if have || pending[e.Hash] {
					pmu.Unlock()
					return nil
				}
				pending[e.Hash] = true
				pmu.Unlock()
				return p.add(e.Hash, b, false)
			})
		case mode&fs.ModeSymlink != 0:
			e.Link = h.Linkname
		case mode.IsDir():
		default:
			continue // devices, pipes: not cached
		}
		if rerr != nil {
			break
		}
		byPath[rel] = e
	}
	if err := g.Wait(); err != nil || rerr != nil {
		p.abort()
		tp.abort()
		return nil, st, errors.Join(rerr, err)
	}

	// The folders above the cache paths, which the archiver leaves out.
	for rel := range byPath {
		for d := parent(rel); d != ""; d = parent(d) {
			if _, ok := byPath[d]; ok {
				break
			}
			byPath[d] = &Entry{Path: d, Mode: fs.ModeDir | 0o755, MTime: start.UnixNano()}
		}
	}
	snap := &Snapshot{Taken: start.UnixNano(), Entries: make([]Entry, 0, len(byPath))}
	for _, e := range byPath {
		switch {
		case e.Mode.IsRegular():
			st.Files++
			st.Bytes += e.Size
		case e.Mode.IsDir():
			st.Dirs++
		}
		snap.Entries = append(snap.Entries, *e)
	}
	// Parents first, as a walk of the folder lists them.
	sort.Slice(snap.Entries, func(i, j int) bool { return snap.Entries[i].Path < snap.Entries[j].Path })
	return s.commit(ctx, snap, name, &st, p, tp, &pmu, pending, start)
}

// tarPath is name as a path under the tar's root, "" for the root itself.
// A name that leaves the root is refused, so a restore never writes
// outside the folder it restores into.
func tarPath(name string) (string, error) {
	c := path.Clean(strings.TrimPrefix(name, "./"))
	switch {
	case c == ".":
		return "", nil
	case path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../"):
		return "", fmt.Errorf("the tar: %q is outside its folder", name)
	}
	return c, nil
}

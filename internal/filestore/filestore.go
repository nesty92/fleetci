// Package filestore keeps saves of a folder in a bucket by their files, so
// a save uploads only the files no earlier save had, and a restore fetches
// only the files the node does not already hold.
//
// The layout under a prefix follows restic's repository (packs, an index,
// snapshots, written in that order, each object once), without encryption:
//
//	packs/<sha256 of the pack>   blobs, one zstd frame each, back to back
//	index/<sha256 of the index>  one per save: which pack holds which blob, where
//	snaps/<name>                 one per save: the hash of its root folder
//
// A blob is a file's content, or a folder's listing (its entries' names,
// modes, times, and their blobs' hashes), known by its sha256: a Merkle
// tree, as restic's trees and git's are. A save uploads the files no
// earlier save had and the listings of the folders that changed, which
// are the changed files' folders and theirs up to the root.
//
// Removing what no snapshot names is left to a sweep in the manner of
// Loki's compactor: mark first, delete a while later, so a restore that
// read an older index finishes first. This prototype does not sweep.
package filestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sync/errgroup"
)

// Remote is the bucket, as the store needs it.
type Remote interface {
	List(ctx context.Context, prefix string) ([]string, error)
	Put(ctx context.Context, name string, body []byte) error
	// Get reads n bytes at off, or all of it when n < 0.
	Get(ctx context.Context, name string, off, n int64) ([]byte, error)
}

// PackSize is what a pack is filled to before it is uploaded.
var PackSize = 16 << 20

// Workers is how many files are hashed, and objects moved, at once.
var Workers = max(4, runtime.NumCPU())

// Entry is one path of a snapshot. Hash is set for files, Link for
// symbolic links.
type Entry struct {
	Path  string      `json:"p"`
	Mode  fs.FileMode `json:"m"`
	MTime int64       `json:"t"`
	Size  int64       `json:"s,omitempty"`
	Hash  string      `json:"h,omitempty"`
	Link  string      `json:"l,omitempty"`
	// Tree is a folder's listing's hash.
	Tree string `json:"-"`
}

// listing is a folder's blob.
type listing struct {
	Entries []listed `json:"e"`
}

type listed struct {
	Name  string      `json:"n"`
	Mode  fs.FileMode `json:"m"`
	MTime int64       `json:"t"`
	Size  int64       `json:"s,omitempty"`
	Hash  string      `json:"h,omitempty"`
	Link  string      `json:"l,omitempty"`
	Tree  string      `json:"d,omitempty"`
}

// snapObj is a snapshot's object.
type snapObj struct {
	Root  string `json:"root"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}

// Snapshot is one save.
type Snapshot struct {
	Entries []Entry `json:"entries"`
}

// where is a file's frame in a pack.
type where struct {
	Pack string `json:"pack"`
	Off  int64  `json:"off"`
	Len  int64  `json:"len"`
	Raw  int64  `json:"raw"`
}

type indexFile struct {
	Files map[string]where `json:"files"`
}

// Store is a client of one prefix. Its index is what it has loaded or
// written; Load reads the bucket's.
type Store struct {
	r      Remote
	prefix string

	mu  sync.Mutex
	idx map[string]where
	// loaded is the index objects already read.
	loaded map[string]bool
}

// New is a store over r under prefix.
func New(r Remote, prefix string) *Store {
	return &Store{r: r, prefix: strings.TrimSuffix(prefix, "/") + "/", idx: map[string]where{}, loaded: map[string]bool{}}
}

// Stats is what a save or a restore did.
type Stats struct {
	Files, Dirs        int
	Bytes              int64 // of the files, as on the disk
	Hashed             int64 // read and hashed: new or changed since the last save
	NewFiles           int
	NewBytes           int64 // of the new files, before compression
	NewTrees           int   // folders' listings uploaded
	TreeBytes          int64 // of them, before compression
	Put, PutBytes      int64 // objects and bytes uploaded
	Get, GetBytes      int64 // requests and bytes downloaded
	Local, LocalBytes  int64 // files copied from a folder on the node
	IndexLoad, Elapsed time.Duration
}

// Load reads the index objects this store has not read yet.
func (s *Store) Load(ctx context.Context, st *Stats) error {
	t := time.Now()
	defer func() { st.IndexLoad += time.Since(t) }()
	names, err := s.r.List(ctx, s.prefix+"index/")
	if err != nil {
		return err
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(Workers)
	for _, n := range names {
		s.mu.Lock()
		done := s.loaded[n]
		s.mu.Unlock()
		if done {
			continue
		}
		g.Go(func() error {
			b, err := s.r.Get(gctx, n, 0, -1)
			if err != nil {
				return err
			}
			atomic.AddInt64(&st.Get, 1)
			atomic.AddInt64(&st.GetBytes, int64(len(b)))
			var f indexFile
			if err := json.Unmarshal(b, &f); err != nil {
				return fmt.Errorf("%s: %w", n, err)
			}
			s.mu.Lock()
			for h, w := range f.Files {
				s.idx[h] = w
			}
			s.loaded[n] = true
			s.mu.Unlock()
			return nil
		})
	}
	return g.Wait()
}

// Save uploads root as the snapshot name. prev, when it is the snapshot
// root was restored from, spares reading files whose size and time it
// already has.
func (s *Store) Save(ctx context.Context, root, name string, prev *Snapshot) (*Snapshot, Stats, error) {
	start := time.Now()
	var st Stats
	known := map[string]Entry{}
	if prev != nil {
		for _, e := range prev.Entries {
			if e.Hash != "" {
				known[e.Path] = e
			}
		}
	}
	snap := &Snapshot{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		e := Entry{Path: filepath.ToSlash(rel), Mode: info.Mode(), MTime: info.ModTime().UnixNano()}
		switch {
		case info.Mode().IsRegular():
			e.Size = info.Size()
			if k, ok := known[e.Path]; ok && k.Size == e.Size && k.MTime == e.MTime {
				e.Hash = k.Hash
			}
			st.Files++
			st.Bytes += e.Size
		case info.Mode()&fs.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			e.Link = l
		case info.IsDir():
			st.Dirs++
		default:
			return nil // sockets, devices: not cached
		}
		snap.Entries = append(snap.Entries, e)
		return nil
	})
	if err != nil {
		return nil, st, err
	}

	p := newPacker(ctx, s, &st)
	// pending is the hashes being packed by this save, so two paths with
	// one content pack it once.
	var pmu sync.Mutex
	pending := map[string]bool{}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(Workers)
	for i := range snap.Entries {
		e := &snap.Entries[i]
		if !e.Mode.IsRegular() {
			continue
		}
		g.Go(func() error {
			if gctx.Err() != nil {
				return gctx.Err()
			}
			var b []byte
			if e.Hash == "" {
				var err error
				if b, err = os.ReadFile(filepath.Join(root, filepath.FromSlash(e.Path))); err != nil {
					return err
				}
				sum := sha256.Sum256(b)
				e.Hash = hex.EncodeToString(sum[:])
				atomic.AddInt64(&st.Hashed, int64(len(b)))
			}
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
			if b == nil {
				// Known by its time, but not in the bucket's index.
				var err error
				if b, err = os.ReadFile(filepath.Join(root, filepath.FromSlash(e.Path))); err != nil {
					return err
				}
			}
			return p.add(e.Hash, b, false)
		})
	}
	if err := g.Wait(); err != nil {
		p.abort()
		return nil, st, err
	}
	top, err := s.trees(snap, p, &pmu, pending)
	if err != nil {
		p.abort()
		return nil, st, err
	}
	written, err := p.close()
	if err != nil {
		return nil, st, err
	}
	// The index, then the snapshot: a snapshot never names a file no index
	// has.
	if len(written) > 0 {
		b, _ := json.Marshal(indexFile{Files: written})
		sum := sha256.Sum256(b)
		n := s.prefix + "index/" + hex.EncodeToString(sum[:])
		if err := s.put(ctx, n, b, &st); err != nil {
			return nil, st, err
		}
		s.mu.Lock()
		s.loaded[n] = true
		s.mu.Unlock()
	}
	b, _ := json.Marshal(snapObj{Root: top, Files: st.Files, Bytes: st.Bytes})
	if err := s.put(ctx, s.prefix+"snaps/"+name, b, &st); err != nil {
		return nil, st, err
	}
	st.Elapsed = time.Since(start)
	return snap, st, nil
}

func (s *Store) put(ctx context.Context, name string, b []byte, st *Stats) error {
	if err := s.r.Put(ctx, name, b); err != nil {
		return err
	}
	atomic.AddInt64(&st.Put, 1)
	atomic.AddInt64(&st.PutBytes, int64(len(b)))
	return nil
}

var (
	encOnce sync.Once
	enc     *zstd.Encoder
	dec     *zstd.Decoder
)

func codecs() {
	encOnce.Do(func() {
		enc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
		dec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	})
}

// trees packs the listing of every folder of snap whose listing no
// earlier save uploaded, deepest first, and says the root's hash.
func (s *Store) trees(snap *Snapshot, p *packer, pmu *sync.Mutex, pending map[string]bool) (string, error) {
	kids := map[string][]*Entry{} // by the folder's path; "" is the root
	for i := range snap.Entries {
		e := &snap.Entries[i]
		kids[parent(e.Path)] = append(kids[parent(e.Path)], e)
	}
	byPath := map[string]*Entry{}
	var dirs []string
	for i := range snap.Entries {
		if e := &snap.Entries[i]; e.Mode.IsDir() {
			byPath[e.Path] = e
			dirs = append(dirs, e.Path)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return depth(dirs[i]) > depth(dirs[j]) })
	hashOf := func(dir string) (string, error) {
		var l listing
		for _, e := range kids[dir] {
			l.Entries = append(l.Entries, listed{Name: path.Base(e.Path), Mode: e.Mode, MTime: e.MTime, Size: e.Size, Hash: e.Hash, Link: e.Link, Tree: e.Tree})
		}
		sort.Slice(l.Entries, func(i, j int) bool { return l.Entries[i].Name < l.Entries[j].Name })
		b, err := json.Marshal(l)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(b)
		h := hex.EncodeToString(sum[:])
		s.mu.Lock()
		_, have := s.idx[h]
		s.mu.Unlock()
		pmu.Lock()
		fresh := !have && !pending[h]
		pending[h] = true
		pmu.Unlock()
		if fresh {
			return h, p.add(h, b, true)
		}
		return h, nil
	}
	for _, d := range dirs {
		h, err := hashOf(d)
		if err != nil {
			return "", err
		}
		byPath[d].Tree = h
	}
	return hashOf("")
}

func parent(p string) string {
	d := path.Dir(p)
	if d == "." {
		return ""
	}
	return d
}

func depth(p string) int { return strings.Count(p, "/") }

// Snap reads the snapshot name: its object, then its folders' listings,
// a level at a time.
func (s *Store) Snap(ctx context.Context, name string, st *Stats) (*Snapshot, error) {
	codecs()
	b, err := s.r.Get(ctx, s.prefix+"snaps/"+name, 0, -1)
	if err != nil {
		return nil, err
	}
	st.Get++
	st.GetBytes += int64(len(b))
	var o snapObj
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, err
	}
	snap := &Snapshot{}
	level := map[string][]string{o.Root: {""}} // a listing's hash: the folders it lists
	for len(level) > 0 {
		var hs []string
		for h := range level {
			hs = append(hs, h)
		}
		var mu sync.Mutex
		next := map[string][]string{}
		err := s.fetch(ctx, hs, st, func(h string, raw []byte) error {
			var l listing
			if err := json.Unmarshal(raw, &l); err != nil {
				return fmt.Errorf("listing %s: %w", h, err)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, dir := range level[h] {
				for _, x := range l.Entries {
					p := x.Name
					if dir != "" {
						p = dir + "/" + x.Name
					}
					snap.Entries = append(snap.Entries, Entry{Path: p, Mode: x.Mode, MTime: x.MTime, Size: x.Size, Hash: x.Hash, Link: x.Link, Tree: x.Tree})
					if x.Tree != "" {
						next[x.Tree] = append(next[x.Tree], p)
					}
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		level = next
	}
	// Parents before children, as a walk lists them.
	sort.Slice(snap.Entries, func(i, j int) bool { return snap.Entries[i].Path < snap.Entries[j].Path })
	return snap, nil
}

// fetch reads the blobs hs from their packs, a range per run of nearby
// frames, and hands each to fn, maybe at once.
func (s *Store) fetch(ctx context.Context, hs []string, st *Stats, fn func(h string, raw []byte) error) error {
	if len(hs) == 0 {
		return nil
	}
	s.mu.Lock()
	unknown := 0
	for _, h := range hs {
		if _, ok := s.idx[h]; !ok {
			unknown++
		}
	}
	s.mu.Unlock()
	if unknown > 0 {
		if err := s.Load(ctx, st); err != nil {
			return err
		}
	}
	byPack := map[string][]string{}
	s.mu.Lock()
	for _, h := range hs {
		w, ok := s.idx[h]
		if !ok {
			s.mu.Unlock()
			return fmt.Errorf("no index has blob %s", h)
		}
		byPack[w.Pack] = append(byPack[w.Pack], h)
	}
	s.mu.Unlock()
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(Workers * 2)
	for pack, hs := range byPack {
		for _, rg := range s.ranges(hs) {
			g.Go(func() error {
				b, err := s.r.Get(gctx, pack, rg.off, rg.end-rg.off)
				if err != nil {
					return err
				}
				atomic.AddInt64(&st.Get, 1)
				atomic.AddInt64(&st.GetBytes, int64(len(b)))
				for _, h := range rg.hs {
					w := s.where(h)
					raw, err := dec.DecodeAll(b[w.Off-rg.off:w.Off-rg.off+w.Len], make([]byte, 0, w.Raw))
					if err != nil {
						return fmt.Errorf("blob %s: %w", h, err)
					}
					if err := fn(h, raw); err != nil {
						return err
					}
				}
				return nil
			})
		}
	}
	return g.Wait()
}

// packer fills packs and uploads them while the save goes on.
type packer struct {
	s   *Store
	st  *Stats
	ctx context.Context
	g   *errgroup.Group

	mu    sync.Mutex
	buf   bytes.Buffer
	files map[string]where // of the pack being filled; Pack is set on upload
	// wmu guards written apart from mu: an upload must not wait for mu,
	// which add holds while it waits for a free upload.
	wmu     sync.Mutex
	written map[string]where
}

func newPacker(ctx context.Context, s *Store, st *Stats) *packer {
	codecs()
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(Workers)
	return &packer{s: s, st: st, ctx: gctx, g: g, files: map[string]where{}, written: map[string]where{}}
}

func (p *packer) add(hash string, raw []byte, tree bool) error {
	z := enc.EncodeAll(raw, nil)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.files[hash] = where{Off: int64(p.buf.Len()), Len: int64(len(z)), Raw: int64(len(raw))}
	p.buf.Write(z)
	if tree {
		p.st.NewTrees++
		p.st.TreeBytes += int64(len(raw))
	} else {
		p.st.NewBytes += int64(len(raw))
		p.st.NewFiles++
	}
	if p.buf.Len() >= PackSize {
		p.flushLocked()
	}
	return p.ctx.Err()
}

func (p *packer) flushLocked() {
	if p.buf.Len() == 0 {
		return
	}
	body := bytes.Clone(p.buf.Bytes())
	files := p.files
	p.buf.Reset()
	p.files = map[string]where{}
	p.g.Go(func() error {
		sum := sha256.Sum256(body)
		name := p.s.prefix + "packs/" + hex.EncodeToString(sum[:])
		if err := p.s.put(p.ctx, name, body, p.st); err != nil {
			return err
		}
		p.wmu.Lock()
		defer p.wmu.Unlock()
		p.s.mu.Lock()
		defer p.s.mu.Unlock()
		for h, w := range files {
			w.Pack = name
			p.written[h] = w
			p.s.idx[h] = w
		}
		return nil
	})
}

func (p *packer) close() (map[string]where, error) {
	p.mu.Lock()
	p.flushLocked()
	p.mu.Unlock()
	if err := p.g.Wait(); err != nil {
		return nil, err
	}
	return p.written, nil
}

func (p *packer) abort() { _ = p.g.Wait() }

// Restore writes snap into dst, which should be empty. local, when not
// nil, is a folder on the node with an older snapshot of the same paths:
// its files whose hashes snap names are copied from there instead of
// fetched.
func (s *Store) Restore(ctx context.Context, snap *Snapshot, dst string, local string, localSnap *Snapshot, st *Stats) error {
	start := time.Now()
	codecs()
	near := map[string]string{}
	if localSnap != nil {
		for _, e := range localSnap.Entries {
			if e.Hash != "" {
				near[e.Hash] = filepath.Join(local, filepath.FromSlash(e.Path))
			}
		}
	}
	// paths is every path of each hash this snapshot names.
	paths := map[string][]*Entry{}
	for i := range snap.Entries {
		e := &snap.Entries[i]
		full := filepath.Join(dst, filepath.FromSlash(e.Path))
		if !filepath.IsLocal(e.Path) {
			return fmt.Errorf("snapshot path %q leaves the folder", e.Path)
		}
		switch {
		case e.Mode.IsDir():
			st.Dirs++
			if err := os.MkdirAll(full, 0o755); err != nil {
				return err
			}
		case e.Link != "":
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(e.Link, full); err != nil {
				return err
			}
		case e.Hash != "":
			st.Files++
			st.Bytes += e.Size
			paths[e.Hash] = append(paths[e.Hash], e)
		}
	}
	write := func(h string, b []byte) error {
		for _, e := range paths[h] {
			full := filepath.Join(dst, filepath.FromSlash(e.Path))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(full, b, e.Mode.Perm()); err != nil {
				return err
			}
		}
		return nil
	}
	// From the node first; what it lacks, from the bucket.
	var missing []string
	var mmu sync.Mutex
	g, _ := errgroup.WithContext(ctx)
	g.SetLimit(Workers)
	for h := range paths {
		if src, ok := near[h]; ok {
			g.Go(func() error {
				b, err := os.ReadFile(src)
				if err == nil && sha256.Sum256(b) == mustHash(h) {
					atomic.AddInt64(&st.Local, 1)
					atomic.AddInt64(&st.LocalBytes, int64(len(b)))
					return write(h, b)
				}
				// Gone or changed on the node: fetched with the rest.
				mmu.Lock()
				missing = append(missing, h)
				mmu.Unlock()
				return nil
			})
			continue
		}
		mmu.Lock()
		missing = append(missing, h)
		mmu.Unlock()
	}
	if err := g.Wait(); err != nil {
		return err
	}
	if err := s.fetch(ctx, missing, st, write); err != nil {
		return err
	}
	// Times last, deepest first, so writing a file does not move its
	// folder's.
	for i := len(snap.Entries) - 1; i >= 0; i-- {
		e := snap.Entries[i]
		full := filepath.Join(dst, filepath.FromSlash(e.Path))
		if e.Link != "" {
			continue
		}
		if e.Mode.IsDir() {
			_ = os.Chmod(full, e.Mode.Perm())
		}
		t := time.Unix(0, e.MTime)
		_ = os.Chtimes(full, t, t)
	}
	st.Elapsed = time.Since(start)
	return nil
}

func (s *Store) where(h string) where {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.idx[h]
}

type span struct {
	off, end int64
	hs       []string
}

// ranges joins a pack's frames into reads, bridging gaps under 1 MiB:
// fewer requests for a few wasted bytes.
func (s *Store) ranges(hs []string) []span {
	sort.Slice(hs, func(i, j int) bool { return s.where(hs[i]).Off < s.where(hs[j]).Off })
	var out []span
	for _, h := range hs {
		w := s.where(h)
		if n := len(out); n > 0 && w.Off-out[n-1].end < 1<<20 && w.Off+w.Len-out[n-1].off <= 4*int64(PackSize) {
			out[n-1].end = max(out[n-1].end, w.Off+w.Len)
			out[n-1].hs = append(out[n-1].hs, h)
			continue
		}
		out = append(out, span{off: w.Off, end: w.Off + w.Len, hs: []string{h}})
	}
	return out
}

func mustHash(h string) [32]byte {
	var out [32]byte
	_, _ = hex.Decode(out[:], []byte(h))
	return out
}

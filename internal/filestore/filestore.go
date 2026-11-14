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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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

// Opener is a Remote that can also hand a read over as it arrives, so
// files are written while the rest of their range is still on its way: a
// range from a bucket far away takes most of a second.
type Opener interface {
	Open(ctx context.Context, name string, off, n int64) (io.ReadCloser, error)
}

// PackSize is what a pack is filled to before it is uploaded.
var PackSize = 16 << 20

// MaxRange is about the most one read joins (a read may run a quarter
// over, so that a whole pack is one read). A bucket far away answers a
// read in a round trip of 100 to 200 ms; a read must be long enough that
// its transfer, not the wait, takes most of its time.
var MaxRange int64 = 16 << 20

// Level is the files' compression.
// The fastest uploads 4 to 6% more than the default on a first save of
// real caches, and saves in two thirds of the time; later saves upload
// little either way.
var Level = zstd.SpeedFastest

// Workers is how many files are hashed, and written, at once: work for
// the CPUs and the disk.
var Workers = max(4, runtime.NumCPU())

// Conns is how many objects are read or written in the bucket at once,
// whatever the CPUs: a bucket far away gives each connection a few tens
// of MB/s after a round trip of 100 to 200 ms, so a VM of 2 CPUs needs as
// many as one of 16. Each holds up to a pack in memory.
var Conns = 16

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

// snapObj is a snapshot's object. Trees is where its folders' listings
// are: they are packed apart from files, as restic packs trees, so a
// restore reads them all at once instead of a folder level at a time.
type snapObj struct {
	Root  string     `json:"root"`
	Files int        `json:"files"`
	Bytes int64      `json:"bytes"`
	Trees []treeSpan `json:"trees,omitempty"`
	Taken int64      `json:"taken,omitempty"`
}

// treeSpan is a run of listings' frames in a pack of listings.
type treeSpan struct {
	Pack     string `json:"pack"`
	Off, End int64
}

// Snapshot is one save.
type Snapshot struct {
	Entries []Entry `json:"entries"`
	// Taken is when the save that made it began to look at the folder, in
	// Unix nanoseconds. A file whose time is not well before it may have
	// changed again within its time's grain, and is read again by the
	// next save, as git treats a racily clean file.
	Taken int64 `json:"taken,omitempty"`
}

// racy is how close to a snapshot's Taken a file's time must not be for
// the next save to trust it: more than the coarsest time grain of the
// filesystems a cache lands on.
const racy = int64(2 * time.Second)

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
	// Of a stream: when its first file went out, and how long it waited
	// for the bucket and for its reader.
	FirstFile, Starved, Blocked time.Duration
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
	g.SetLimit(Conns)
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
	// A file is known by its size and time as prev has them, unless that
	// time was too close to prev's save to say the file has not changed
	// since.
	known := map[string]Entry{}
	if prev != nil {
		for _, e := range prev.Entries {
			if e.Hash != "" && e.MTime < prev.Taken-racy {
				known[e.Path] = e
			}
		}
	}
	snap := &Snapshot{Taken: time.Now().UnixNano()}
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
	tp := newPacker(ctx, s, &st) // listings, in packs of their own
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
			if e.Hash != "" {
				s.mu.Lock()
				_, have := s.idx[e.Hash]
				s.mu.Unlock()
				if have {
					return nil // known by its time, and in the bucket
				}
			}
			// Read, and hashed for what was read, whatever the walk saw or
			// prev said: the file is what these bytes are, with the size
			// and time they were read at.
			b, fi, err := readStill(filepath.Join(root, filepath.FromSlash(e.Path)))
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			e.Hash = hex.EncodeToString(sum[:])
			atomic.AddInt64(&st.Hashed, int64(len(b)))
			atomic.AddInt64(&st.Bytes, int64(len(b))-e.Size)
			e.Size, e.MTime = int64(len(b)), fi.ModTime().UnixNano()
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
	}
	if err := g.Wait(); err != nil {
		p.abort()
		return nil, st, err
	}
	top, lists, err := s.trees(snap, tp, &pmu, pending)
	if err != nil {
		p.abort()
		tp.abort()
		return nil, st, err
	}
	// Both packers' last uploads at once.
	var wt map[string]where
	var terr error
	tdone := make(chan struct{})
	go func() { wt, terr = tp.close(); close(tdone) }()
	written, err := p.close()
	<-tdone
	if err == nil {
		err = terr
	}
	if err != nil {
		return nil, st, err
	}
	for h, w := range wt {
		written[h] = w
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
	o := snapObj{Root: top, Files: st.Files, Bytes: st.Bytes, Taken: snap.Taken}
	byPack := map[string][]string{}
	for _, h := range lists {
		byPack[s.where(h).Pack] = append(byPack[s.where(h).Pack], h)
	}
	for pack, hs := range byPack {
		for _, rg := range s.ranges(hs) {
			o.Trees = append(o.Trees, treeSpan{Pack: pack, Off: rg.off, End: rg.end})
		}
	}
	sort.Slice(o.Trees, func(i, j int) bool {
		a, b := o.Trees[i], o.Trees[j]
		return a.Pack < b.Pack || a.Pack == b.Pack && a.Off < b.Off
	})
	b, _ := json.Marshal(o)
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
		// EncodeAll on a shared encoder runs as many at once as its
		// concurrency, so one per CPU.
		enc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(Level), zstd.WithEncoderConcurrency(runtime.GOMAXPROCS(0)))
		dec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	})
}

// trees packs the listing of every folder of snap whose listing no
// earlier save uploaded, deepest first, and says the root's hash and the
// hashes of all its listings.
func (s *Store) trees(snap *Snapshot, p *packer, pmu *sync.Mutex, pending map[string]bool) (string, []string, error) {
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
	var all []string
	seen := map[string]bool{}
	for _, d := range dirs {
		h, err := hashOf(d)
		if err != nil {
			return "", nil, err
		}
		byPath[d].Tree = h
		if !seen[h] {
			seen[h] = true
			all = append(all, h)
		}
	}
	h, err := hashOf("")
	if err == nil && !seen[h] {
		all = append(all, h)
	}
	return h, all, err
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
// all at once from the spans the object names, while the index loads for
// the restore that follows. A listing the spans lack is fetched a folder
// level at a time.
func (s *Store) Snap(ctx context.Context, name string, st *Stats) (*Snapshot, error) {
	codecs()
	// The index is read while the snapshot is: a round trip less.
	loaded := make(chan error, 1)
	go func() { loaded <- s.Load(ctx, st) }()
	b, err := s.r.Get(ctx, s.prefix+"snaps/"+name, 0, -1)
	var o snapObj
	if err == nil {
		atomic.AddInt64(&st.Get, 1)
		atomic.AddInt64(&st.GetBytes, int64(len(b)))
		err = json.Unmarshal(b, &o)
	}
	if err != nil {
		<-loaded
		return nil, err
	}
	got, err := s.listings(ctx, o.Trees, st)
	if err != nil {
		<-loaded
		return nil, err
	}
	if err := <-loaded; err != nil {
		return nil, err
	}
	snap := &Snapshot{Taken: o.Taken}
	level := map[string][]string{o.Root: {""}} // a listing's hash: the folders it lists
	for len(level) > 0 {
		var hs []string
		for h := range level {
			hs = append(hs, h)
		}
		var mu sync.Mutex
		next := map[string][]string{}
		each := func(h string, raw []byte) error {
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
		}
		var missing []string
		for _, h := range hs {
			if raw, ok := got[h]; ok {
				if err := each(h, raw); err != nil {
					return nil, err
				}
			} else {
				missing = append(missing, h)
			}
		}
		if err := s.fetch(ctx, missing, st, each); err != nil {
			return nil, err
		}
		level = next
	}
	// Parents before children, as a walk lists them.
	sort.Slice(snap.Entries, func(i, j int) bool { return snap.Entries[i].Path < snap.Entries[j].Path })
	return snap, nil
}

// listings reads spans of listings' frames in parallel and says each
// listing by its hash, as computed from its bytes: a frame that does not
// match what a folder names is never used.
func (s *Store) listings(ctx context.Context, spans []treeSpan, st *Stats) (map[string][]byte, error) {
	got := map[string][]byte{}
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(Conns)
	for _, sp := range spans {
		g.Go(func() error {
			b, err := s.r.Get(gctx, sp.Pack, sp.Off, sp.End-sp.Off)
			if err != nil {
				return err
			}
			atomic.AddInt64(&st.Get, 1)
			atomic.AddInt64(&st.GetBytes, int64(len(b)))
			raw, err := dec.DecodeAll(b, nil) // its frames, one after another
			if err != nil {
				return fmt.Errorf("listings in %s: %w", sp.Pack, err)
			}
			d := json.NewDecoder(bytes.NewReader(raw))
			for d.More() {
				var m json.RawMessage
				if err := d.Decode(&m); err != nil {
					return fmt.Errorf("listings in %s: %w", sp.Pack, err)
				}
				sum := sha256.Sum256(m)
				mu.Lock()
				got[hex.EncodeToString(sum[:])] = m
				mu.Unlock()
			}
			return nil
		})
	}
	return got, g.Wait()
}

// fetch reads the blobs hs from their packs, a range per run of nearby
// frames, and hands each to fn, maybe at once; raw is fn's to keep. A blob whose content is not its hash is refused: a pack is
// never trusted for what its index says it holds.
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
	g.SetLimit(Conns)
	for pack, hs := range byPack {
		for _, rg := range s.ranges(hs) {
			g.Go(func() error {
				next, done, err := s.read(gctx, pack, rg, st)
				if err != nil {
					return err
				}
				defer done()
				for _, h := range rg.hs {
					w := s.where(h)
					frame, err := next(w)
					if err != nil {
						return err
					}
					raw, err := dec.DecodeAll(frame, make([]byte, 0, w.Raw))
					if err != nil {
						return fmt.Errorf("blob %s: %w", h, err)
					}
					if sha256.Sum256(raw) != mustHash(h) {
						return fmt.Errorf("blob %s: its content has another hash", h)
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
	g.SetLimit(Conns)
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
		up := p.take()
		p.mu.Unlock()
		p.g.Go(up) // may wait for a free upload, without the lock
		p.mu.Lock()
	}
	return p.ctx.Err()
}

// take hands the pack being filled to an upload, and starts another.
// p.mu is held.
func (p *packer) take() func() error {
	body := bytes.Clone(p.buf.Bytes())
	files := p.files
	p.buf.Reset()
	p.files = map[string]where{}
	return func() error {
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
	}
}

func (p *packer) close() (map[string]where, error) {
	p.mu.Lock()
	var up func() error
	if p.buf.Len() > 0 {
		up = p.take()
	}
	p.mu.Unlock()
	if up != nil {
		p.g.Go(up)
	}
	if err := p.g.Wait(); err != nil {
		return nil, err
	}
	return p.written, nil
}

func (p *packer) abort() { _ = p.g.Wait() }

// Restore writes snap into dst, which should be empty. local, when not
// nil, is a folder on the node with an older snapshot of the same paths:
// a file of it whose content snap names is copied from there, by the
// kernel, instead of fetched, when it still has the size and time
// localSnap gives it (a file is known by those as Save knows it). What
// the node lacks is fetched from the bucket meanwhile.
func (s *Store) Restore(ctx context.Context, snap *Snapshot, dst string, local string, localSnap *Snapshot, st *Stats) error {
	start := time.Now()
	codecs()
	near := map[string]*Entry{}
	if localSnap != nil {
		for i := range localSnap.Entries {
			if e := &localSnap.Entries[i]; e.Hash != "" {
				near[e.Hash] = e
			}
		}
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	// Folders, parents first (Snap sorts by path), then links; paths is
	// every path of each content.
	paths := map[string][]*Entry{}
	var dirs []*Entry
	for i := range snap.Entries {
		e := &snap.Entries[i]
		if !filepath.IsLocal(e.Path) {
			return fmt.Errorf("snapshot path %q leaves the folder", e.Path)
		}
		full := filepath.Join(dst, filepath.FromSlash(e.Path))
		switch {
		case e.Mode.IsDir():
			st.Dirs++
			dirs = append(dirs, e)
			if err := os.Mkdir(full, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
		case e.Link != "":
			if err := os.Symlink(e.Link, full); err != nil {
				return err
			}
		case e.Hash != "":
			st.Files++
			st.Bytes += e.Size
			paths[e.Hash] = append(paths[e.Hash], e)
		}
	}
	at := func(e *Entry) string { return filepath.Join(dst, filepath.FromSlash(e.Path)) }
	// Files are written by a pool of their own, far more at once than the
	// ranges being read: a file is a few syscalls, which wait on the disk
	// more than they use a CPU.
	wg, wctx := errgroup.WithContext(ctx)
	wg.SetLimit(Workers * 4)
	write := func(h string, b []byte) error {
		if wctx.Err() != nil {
			return wctx.Err()
		}
		wg.Go(func() error {
			for _, e := range paths[h] {
				if err := writeFile(at(e), b, e); err != nil {
					return err
				}
			}
			return nil
		})
		return nil
	}
	var missing, mine []string
	for h := range paths {
		if near[h] != nil {
			mine = append(mine, h)
		} else {
			missing = append(missing, h)
		}
	}
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return s.fetch(gctx, missing, st, write) })
	// From the node, while the rest comes: syscalls, so more at once than
	// CPUs.
	var gone []string
	var gmu sync.Mutex
	lg, lctx := errgroup.WithContext(gctx)
	lg.SetLimit(Workers * 2)
	for _, h := range mine {
		lg.Go(func() error {
			if lctx.Err() != nil {
				return lctx.Err()
			}
			n := near[h]
			src := filepath.Join(local, filepath.FromSlash(n.Path))
			ok, err := copyFile(src, at(paths[h][0]), n, paths[h][0])
			if err != nil {
				return err
			}
			if !ok { // gone or changed on the node: fetched instead
				gmu.Lock()
				gone = append(gone, h)
				gmu.Unlock()
				return nil
			}
			atomic.AddInt64(&st.Local, 1)
			atomic.AddInt64(&st.LocalBytes, n.Size)
			for _, e := range paths[h][1:] {
				if _, err := copyFile(at(paths[h][0]), at(e), paths[h][0], e); err != nil {
					return err
				}
			}
			return nil
		})
	}
	g.Go(func() error {
		if err := lg.Wait(); err != nil {
			return err
		}
		return s.fetch(gctx, gone, st, write)
	})
	err := g.Wait()
	if werr := wg.Wait(); err == nil {
		err = werr
	}
	if err != nil {
		return err
	}
	// Folders' modes and times last, once nothing more is written in them.
	dg, _ := errgroup.WithContext(ctx)
	dg.SetLimit(Workers * 2)
	for _, e := range dirs {
		dg.Go(func() error {
			full := at(e)
			if e.Mode.Perm() != 0o755 {
				_ = os.Chmod(full, e.Mode.Perm())
			}
			t := time.Unix(0, e.MTime)
			_ = os.Chtimes(full, t, t)
			return nil
		})
	}
	_ = dg.Wait()
	st.Elapsed = time.Since(start)
	return nil
}

// readStill reads the file at path whole, and its size and time as it was
// read: again if it changed while it was read, and as last read if it
// keeps changing (a save of a folder in use is of whatever it held).
func readStill(path string) ([]byte, fs.FileInfo, error) {
	var b []byte
	var after fs.FileInfo
	for try := 0; ; try++ {
		f, err := os.Open(path)
		if err != nil {
			return nil, nil, err
		}
		before, err := f.Stat()
		if err == nil {
			b = make([]byte, 0, before.Size()+1)
			b, err = readAll(f, b)
		}
		if err == nil {
			after, err = f.Stat()
		}
		f.Close()
		if err != nil {
			return nil, nil, err
		}
		if try == 2 || before.Size() == after.Size() && before.ModTime().Equal(after.ModTime()) && int64(len(b)) == after.Size() {
			return b, after, nil
		}
	}
}

// readAll is io.ReadAll into b, which has room for the whole file.
func readAll(f *os.File, b []byte) ([]byte, error) {
	for {
		if len(b) == cap(b) {
			b = append(b, 0)[:len(b)]
		}
		n, err := f.Read(b[len(b):cap(b)])
		b = b[:len(b)+n]
		if err == io.EOF {
			return b, nil
		}
		if err != nil {
			return b, err
		}
	}
}

// writeFile writes b as e at path, with e's mode and time.
func writeFile(path string, b []byte, e *Entry) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, e.Mode.Perm())
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return finish(f, path, e)
}

// copyFile copies src, which should be as want says, to dst as e, by the
// kernel (copy_file_range: a reflink where the filesystem has them). It
// says false, having written nothing, when src is not as want says.
func copyFile(src, dst string, want, e *Entry) (bool, error) {
	in, err := os.Open(src)
	if err != nil {
		return false, nil
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != want.Size || fi.ModTime().UnixNano() != want.MTime {
		return false, nil
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, e.Mode.Perm())
	if err != nil {
		return false, err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return false, err
	}
	return true, finish(out, dst, e)
}

// finish gives f e's mode, which the umask may have narrowed, and time.
func finish(f *os.File, path string, e *Entry) error {
	if e.Mode.Perm()&0o022 != 0 {
		if err := f.Chmod(e.Mode.Perm()); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	t := time.Unix(0, e.MTime)
	return os.Chtimes(path, t, t)
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

// ranges joins a pack's frames into reads of up to MaxRange, bridging gaps
// under 1 MiB: fewer requests for a few wasted bytes, but enough of them to
// fill the connections a store reads over, each of which a bucket far away
// serves at a few tens of MB/s, and a first file soon for the stream.
func (s *Store) ranges(hs []string) []span {
	sort.Slice(hs, func(i, j int) bool { return s.where(hs[i]).Off < s.where(hs[j]).Off })
	var out []span
	for _, h := range hs {
		w := s.where(h)
		if n := len(out); n > 0 && w.Off-out[n-1].end < 1<<20 && w.Off+w.Len-out[n-1].off <= MaxRange+MaxRange/4 {
			out[n-1].end = max(out[n-1].end, w.Off+w.Len)
			out[n-1].hs = append(out[n-1].hs, h)
			continue
		}
		out = append(out, span{off: w.Off, end: w.Off + w.Len, hs: []string{h}})
	}
	return out
}

// read opens rg of pack and gives its frames in order, each by next(w) for
// the frames rg.hs name, as they arrive when the Remote is an Opener; a
// frame next gives is valid until its next call. done closes the read.
func (s *Store) read(ctx context.Context, pack string, rg span, st *Stats) (next func(where) ([]byte, error), done func(), err error) {
	n := rg.end - rg.off
	o, ok := s.r.(Opener)
	if !ok {
		b, err := s.r.Get(ctx, pack, rg.off, n)
		if err != nil {
			return nil, nil, err
		}
		st.addGet(len(b))
		return func(w where) ([]byte, error) { return b[w.Off-rg.off : w.Off-rg.off+w.Len], nil }, func() {}, nil
	}
	body, err := o.Open(ctx, pack, rg.off, n)
	if err != nil {
		return nil, nil, err
	}
	atomic.AddInt64(&st.Get, 1)
	at := rg.off
	var buf []byte
	next = func(w where) ([]byte, error) {
		if w.Off < at {
			return nil, fmt.Errorf("pack %s: frames out of order at %d", pack, w.Off)
		}
		if gap := w.Off - at; gap > 0 {
			if _, err := io.CopyN(io.Discard, body, gap); err != nil {
				return nil, fmt.Errorf("pack %s: %w", pack, err)
			}
		}
		if int64(cap(buf)) < w.Len {
			buf = make([]byte, w.Len)
		}
		b := buf[:w.Len]
		if _, err := io.ReadFull(body, b); err != nil {
			return nil, fmt.Errorf("pack %s: %w", pack, err)
		}
		atomic.AddInt64(&st.GetBytes, w.Off+w.Len-at)
		at = w.Off + w.Len
		return b, nil
	}
	return next, func() { body.Close() }, nil
}

func mustHash(h string) [32]byte {
	var out [32]byte
	_, _ = hex.Decode(out[:], []byte(h))
	return out
}

func (st *Stats) addGet(n int) {
	atomic.AddInt64(&st.Get, 1)
	atomic.AddInt64(&st.GetBytes, int64(n))
}

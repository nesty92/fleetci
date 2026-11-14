package filestore

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"time"

	"golang.org/x/sync/errgroup"
)

// Stream writes snap to w as one tar.zst that a stock client unpacks
// (actions/cache: `tar -x --use-compress-program unzstd`). zstd frames
// joined end to end are one stream, so the files' frames are copied from
// their packs as stored, never compressed again; only the tar headers,
// the padding after each file and the end are framed here, as Podman's
// zstd:chunked layers frame headers and contents apart.
//
// Folders come first, writable by their owner whatever their mode, then
// links, then the files in the order their frames sit in the packs, so the
// packs are read front to back, a few ranges ahead of the writer. Last
// come the folders again, deepest first, with their own modes and times:
// tar sets a folder's mode as soon as an entry outside it follows, and a
// read-only folder (Go's module cache keeps them so) would refuse the
// files that come after; written again at the end, a folder's mode and
// time are what it was saved with, files written into it or not.
func (s *Store) Stream(ctx context.Context, snap *Snapshot, w io.Writer, st *Stats) error {
	start := time.Now()
	codecs()
	var dirs, links []*Entry
	paths := map[string][]*Entry{}
	var hs []string
	for i := range snap.Entries {
		e := &snap.Entries[i]
		switch {
		case e.Mode.IsDir():
			dirs = append(dirs, e)
		case e.Link != "":
			links = append(links, e)
		case e.Hash != "":
			if paths[e.Hash] == nil {
				hs = append(hs, e.Hash)
			}
			paths[e.Hash] = append(paths[e.Hash], e)
		}
	}
	if err := s.fetchIndex(ctx, hs, st); err != nil {
		return err
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Path < dirs[j].Path })

	tw := &tarFramer{w: w}
	for _, e := range dirs {
		tw.add(e, e.Mode.Perm()|0o700)
		st.Dirs++
	}
	for _, e := range links {
		tw.add(e, e.Mode.Perm())
	}
	if err := tw.flush(); err != nil {
		return err
	}

	// The files' spans, pack by pack, each pack front to back.
	byPack := map[string][]string{}
	var packs []string
	for _, h := range hs {
		p := s.where(h).Pack
		if byPack[p] == nil {
			packs = append(packs, p)
		}
		byPack[p] = append(byPack[p], h)
	}
	sort.Strings(packs)
	// Each range is turned into its part of the stream, headers framed
	// and files' frames after them, by the goroutine that read it, so the
	// framing runs on every CPU and the writer only writes, once a range.
	// The padding a range starts with is its previous file's, known from
	// the sizes before anything is read.
	type job struct {
		pack string
		sp   span
		pad  int64
		done chan []byte
	}
	var jobs []*job
	var pad int64
	for _, p := range packs {
		for _, sp := range s.ranges(byPack[p]) {
			jobs = append(jobs, &job{pack: p, sp: sp, pad: pad, done: make(chan []byte, 1)})
			for _, h := range sp.hs {
				if es := paths[h]; len(es) > 0 {
					pad = (512 - es[len(es)-1].Size%512) % 512
				}
			}
		}
	}
	g, gctx := errgroup.WithContext(ctx)
	ahead := make(chan struct{}, Conns) // ranges read and not yet written
	g.Go(func() error {
		for _, j := range jobs {
			select {
			case ahead <- struct{}{}:
			case <-gctx.Done():
				return gctx.Err()
			}
			g.Go(func() error {
				b, err := s.r.Get(gctx, j.pack, j.sp.off, j.sp.end-j.sp.off)
				if err != nil {
					return err
				}
				st.addGet(len(b))
				var out bytes.Buffer
				out.Grow(len(b) + len(j.sp.hs)*128)
				jt := &tarFramer{w: &out, pad: j.pad}
				for _, h := range j.sp.hs {
					wh := s.where(h)
					frame := b[wh.Off-j.sp.off : wh.Off-j.sp.off+wh.Len]
					var raw []byte // a small file's content, once for its paths
					for _, e := range paths[h] {
						jt.add(e, e.Mode.Perm())
						if e.Size <= smallFile {
							// Into the frame being gathered: a frame each
							// would cost the reader more than the file.
							if raw == nil {
								if raw, err = dec.DecodeAll(frame, make([]byte, 0, e.Size)); err != nil {
									return fmt.Errorf("blob %s: %w", h, err)
								}
							}
							jt.buf.Write(raw)
						} else {
							_ = jt.flush() // into out, which never fails
							out.Write(frame)
						}
						jt.pad = (512 - e.Size%512) % 512
						if jt.buf.Len() >= groupFrame {
							_ = jt.flush()
						}
					}
				}
				_ = jt.flush()
				j.done <- out.Bytes()
				return nil
			})
		}
		return nil
	})
	g.Go(func() error {
		for i, j := range jobs {
			var b []byte
			t := time.Now()
			select {
			case b = <-j.done:
			case <-gctx.Done():
				return gctx.Err()
			}
			t2 := time.Now()
			st.Starved += t2.Sub(t)
			if i == 0 {
				st.FirstFile = t2.Sub(start)
			}
			if _, err := w.Write(b); err != nil {
				return err
			}
			st.Blocked += time.Since(t2)
			for _, h := range j.sp.hs {
				for _, e := range paths[h] {
					st.Files++
					st.Bytes += e.Size
				}
			}
			<-ahead
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		return err
	}
	tw.pad = pad
	for i := len(dirs) - 1; i >= 0; i-- {
		tw.add(dirs[i], dirs[i].Mode.Perm())
	}
	if err := tw.end(); err != nil {
		return err
	}
	st.Elapsed = time.Since(start)
	return nil
}

// fetchIndex makes sure the index knows every blob of hs.
func (s *Store) fetchIndex(ctx context.Context, hs []string, st *Stats) error {
	s.mu.Lock()
	unknown := false
	for _, h := range hs {
		if _, ok := s.idx[h]; !ok {
			unknown = true
			break
		}
	}
	s.mu.Unlock()
	if !unknown {
		return nil
	}
	return s.Load(ctx, st)
}

// tarFramer frames tar headers, with the padding of the file before them
// and any small files' contents between them.
type tarFramer struct {
	w   io.Writer
	pad int64
	buf bytes.Buffer
	out []byte
}

var zeros [1024]byte

// A file of smallFile bytes or fewer is not copied as its own frame but
// framed again with the headers and files around it, up to groupFrame of
// them: unzstd sets up for every frame, and a cache of small files would
// be more frames than content. Larger files' frames are copied as stored.
const (
	smallFile  = 64 << 10
	groupFrame = 1 << 20
)

// add puts e's header, with mode, after what is held; flush frames it.
// Headers in a row share a frame.
func (t *tarFramer) add(e *Entry, mode fs.FileMode) {
	h := &tar.Header{
		Name:    e.Path,
		Mode:    int64(mode),
		ModTime: time.Unix(0, e.MTime),
		Format:  tar.FormatPAX,
	}
	switch {
	case e.Mode.IsDir():
		h.Typeflag, h.Name = tar.TypeDir, e.Path+"/"
	case e.Link != "":
		h.Typeflag, h.Linkname = tar.TypeSymlink, e.Link
	default:
		h.Typeflag, h.Size = tar.TypeReg, e.Size
	}
	t.buf.Write(zeros[:t.pad])
	t.pad = 0
	// A tar.Writer over the buffer writes the header (and its PAX records
	// for long names or exact times); a file's content follows from its
	// frame. Its error is the buffer's, which has none.
	_ = tar.NewWriter(&t.buf).WriteHeader(h)
}

func (t *tarFramer) flush() error {
	if t.buf.Len() == 0 {
		return nil
	}
	t.out = enc.EncodeAll(t.buf.Bytes(), t.out[:0])
	t.buf.Reset()
	_, err := t.w.Write(t.out)
	return err
}

func (t *tarFramer) end() error {
	t.buf.Write(zeros[:t.pad])
	t.pad = 0
	t.buf.Write(zeros[:])
	return t.flush()
}

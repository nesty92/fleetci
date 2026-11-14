package filestore

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
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
// Folders come first, then links, then the files in the order their
// frames sit in the packs, so the packs are read front to back, a few
// ranges ahead of the writer.
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
	for _, e := range append(dirs, links...) {
		if err := tw.header(e); err != nil {
			return err
		}
		st.Dirs++
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
	type job struct {
		pack string
		sp   span
		done chan []byte
	}
	var jobs []*job
	for _, p := range packs {
		for _, sp := range s.ranges(byPack[p]) {
			jobs = append(jobs, &job{pack: p, sp: sp, done: make(chan []byte, 1)})
		}
	}
	g, gctx := errgroup.WithContext(ctx)
	ahead := make(chan struct{}, Workers*2) // ranges read and not yet written
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
				j.done <- b
				return nil
			})
		}
		return nil
	})
	g.Go(func() error {
		for _, j := range jobs {
			var b []byte
			select {
			case b = <-j.done:
			case <-gctx.Done():
				return gctx.Err()
			}
			for _, h := range j.sp.hs {
				wh := s.where(h)
				frame := b[wh.Off-j.sp.off : wh.Off-j.sp.off+wh.Len]
				for _, e := range paths[h] {
					if err := tw.header(e); err != nil {
						return err
					}
					if _, err := w.Write(frame); err != nil {
						return err
					}
					tw.pad = (512 - e.Size%512) % 512
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

// tarFramer writes tar headers, each in a zstd frame of its own with the
// padding of the file before it.
type tarFramer struct {
	w   io.Writer
	pad int64
	buf bytes.Buffer
}

var zeros [1024]byte

func (t *tarFramer) header(e *Entry) error {
	h := &tar.Header{
		Name:    e.Path,
		Mode:    int64(e.Mode.Perm()),
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
	t.buf.Reset()
	t.buf.Write(zeros[:t.pad])
	t.pad = 0
	// A tar.Writer over the buffer writes the header (and its PAX records
	// for long names or exact times); the content follows from its frame.
	tw := tar.NewWriter(&t.buf)
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	_, err := t.w.Write(enc.EncodeAll(t.buf.Bytes(), nil))
	return err
}

func (t *tarFramer) end() error {
	t.buf.Reset()
	t.buf.Write(zeros[:t.pad])
	t.buf.Write(zeros[:])
	_, err := t.w.Write(enc.EncodeAll(t.buf.Bytes(), nil))
	return err
}

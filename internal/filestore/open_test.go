package filestore

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// openRemote is a memRemote that hands reads over as streams, as Linked
// does, and counts them.
type openRemote struct {
	*memRemote
	opens int
}

func (o *openRemote) Open(ctx context.Context, name string, off, n int64) (io.ReadCloser, error) {
	b, err := o.Get(ctx, name, off, n)
	if err != nil {
		return nil, err
	}
	o.mu.Lock()
	o.opens++
	o.mu.Unlock()
	return io.NopCloser(bytes.NewReader(b)), nil
}

func TestRestoreReadsFramesAsTheyArrive(t *testing.T) {
	ctx := context.Background()
	PackSize = 64 // several frames to a range, and gaps between those asked for
	defer func() { PackSize = 16 << 20 }()
	m := &openRemote{memRemote: &memRemote{objs: map[string][]byte{}}}
	files := map[string]string{}
	for _, c := range "abcdefghij" {
		files["d/"+string(c)] = strings.Repeat(string(c), 40)
	}
	d1 := t.TempDir()
	writeTree(t, d1, files)
	s := New(m, "c")
	snap1, _, err := s.Save(ctx, d1, "s1", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Change every other file, so a warm restore asks for frames with
	// others between them.
	d2 := t.TempDir()
	writeTree(t, d2, files)
	for i, c := range "abcdefghij" {
		if i%2 == 0 {
			files["d/"+string(c)] = strings.Repeat(string(c), 41)
		}
	}
	writeTree(t, d2, files)
	if _, _, err := s.Save(ctx, d2, "s2", nil); err != nil {
		t.Fatal(err)
	}

	for _, local := range []string{"", d1} {
		cold := New(m, "c")
		var st Stats
		snap, err := cold.Snap(ctx, "s2", &st)
		if err != nil {
			t.Fatal(err)
		}
		var near *Snapshot
		if local != "" {
			near = snap1
		}
		m.opens = 0
		dst := t.TempDir()
		if err := cold.Restore(ctx, snap, dst, local, near, &st); err != nil {
			t.Fatal(err)
		}
		if got := readTree(t, dst); len(got) != len(files) || got["d/a"] != files["d/a"] || got["d/b"] != files["d/b"] {
			t.Fatalf("restore next to %q: %v", local, got)
		}
		if m.opens == 0 {
			t.Fatalf("restore next to %q read nothing as a stream", local)
		}
	}

	// A pack that holds other content than its index says, in frames that
	// decode, is refused: every blob is checked against its hash.
	cold := New(m, "c")
	var st Stats
	snap, err := cold.Snap(ctx, "s2", &st)
	if err != nil {
		t.Fatal(err)
	}
	other := enc.EncodeAll([]byte(strings.Repeat("z", 40)), nil)
	for _, e := range snap.Entries {
		if e.Hash == "" {
			continue
		}
		w := cold.where(e.Hash)
		b := m.objs[w.Pack]
		m.objs[w.Pack] = append(append(append([]byte{}, b[:w.Off]...), other...), b[w.Off+w.Len:]...)
		cold.idx[e.Hash] = where{Pack: w.Pack, Off: w.Off, Len: int64(len(other)), Raw: 40}
		break
	}
	if err := cold.Restore(ctx, snap, filepath.Join(t.TempDir(), "x"), "", nil, &st); err == nil || !strings.Contains(err.Error(), "another hash") {
		t.Fatalf("restored from a spoiled pack: %v", err)
	}
}

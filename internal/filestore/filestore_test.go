package filestore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type memRemote struct {
	mu   sync.Mutex
	objs map[string][]byte
	puts int
}

func (m *memRemote) List(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (m *memRemote) Put(_ context.Context, name string, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[name] = bytes.Clone(body)
	m.puts++
	return nil
}

func (m *memRemote) Get(_ context.Context, name string, off, n int64) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	if n < 0 {
		return bytes.Clone(b), nil
	}
	return bytes.Clone(b[off : off+n]), nil
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		b, err := os.ReadFile(p)
		rel, _ := filepath.Rel(root, p)
		out[rel] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSaveUploadsOnlyNewFilesAndRestoresBoth(t *testing.T) {
	ctx := context.Background()
	PackSize = 64 // several packs, and frames joined into ranges
	defer func() { PackSize = 16 << 20 }()
	m := &memRemote{objs: map[string][]byte{}}
	s := New(m, "c")
	v1 := map[string]string{"a/1": strings.Repeat("one", 100), "a/2": "two", "b/3": "three", "b/dup": "two"}
	d1 := t.TempDir()
	writeTree(t, d1, v1)
	if err := os.Symlink("1", filepath.Join(d1, "a", "link")); err != nil {
		t.Fatal(err)
	}
	ago(t, d1) // well before the save, so the next one trusts their times
	snap1, st1, err := s.Save(ctx, d1, "s1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if st1.NewFiles != 3 {
		t.Fatalf("first save packed %d files, want 3 (two paths share one)", st1.NewFiles)
	}

	// The second save: one file changed, one added; the rest known by time.
	d2 := t.TempDir()
	if err := s.Restore(ctx, snap1, d2, "", nil, &Stats{}); err != nil {
		t.Fatal(err)
	}
	if got := readTree(t, d2); len(got) != 4 || got["a/1"] != v1["a/1"] || got["b/dup"] != "two" {
		t.Fatalf("restored %v", got)
	}
	if l, _ := os.Readlink(filepath.Join(d2, "a", "link")); l != "1" {
		t.Fatalf("link is %q", l)
	}
	later := time.Now().Add(time.Minute)
	writeTree(t, d2, map[string]string{"a/2": "two, changed", "c/4": "four"})
	_ = os.Chtimes(filepath.Join(d2, "a/2"), later, later)
	snap2, st2, err := s.Save(ctx, d2, "s2", snap1)
	if err != nil {
		t.Fatal(err)
	}
	if st2.NewFiles != 2 || st2.Hashed != int64(len("two, changed")+len("four")) {
		t.Fatalf("second save: %d new files, %d bytes hashed", st2.NewFiles, st2.Hashed)
	}

	// Cold: a store that has read nothing, into an empty folder.
	d3 := t.TempDir()
	cold := New(m, "c")
	var st3 Stats
	snapR, err := cold.Snap(ctx, "s2", &st3)
	if err != nil {
		t.Fatal(err)
	}
	if err := cold.Restore(ctx, snapR, d3, "", nil, &st3); err != nil {
		t.Fatal(err)
	}
	want := readTree(t, d2)
	if got := readTree(t, d3); len(got) != len(want) || got["a/2"] != "two, changed" || got["c/4"] != "four" {
		t.Fatalf("cold restore %v, want %v", got, want)
	}
	if fi, _ := os.Stat(filepath.Join(d3, "a/2")); !fi.ModTime().Equal(later.Truncate(0)) && fi.ModTime().Unix() != later.Unix() {
		t.Fatalf("time %v, want %v", fi.ModTime(), later)
	}

	// Warm: the node holds the first folder; only the new files are fetched.
	d4 := t.TempDir()
	var st4 Stats
	if err := cold.Restore(ctx, snap2, d4, d1, snap1, &st4); err != nil {
		t.Fatal(err)
	}
	if got := readTree(t, d4); got["a/2"] != "two, changed" || got["a/1"] != v1["a/1"] {
		t.Fatalf("warm restore %v", got)
	}
	if st4.Local != 3 || st4.GetBytes == 0 {
		t.Fatalf("warm restore: %d files from the node, %d bytes fetched", st4.Local, st4.GetBytes)
	}
}

func TestRestoreRefusesPathsOutside(t *testing.T) {
	s := New(&memRemote{objs: map[string][]byte{}}, "c")
	snap := &Snapshot{Entries: []Entry{{Path: "../x", Hash: "00"}}}
	if err := s.Restore(context.Background(), snap, t.TempDir(), "", nil, &Stats{}); err == nil {
		t.Fatal("restored a path outside the folder")
	}
}

// ago dates root's files an hour back.
func ago(t *testing.T, root string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			_ = os.Chtimes(p, old, old)
		}
		return nil
	})
}

// A file changed in the second its save looked at it, its size and time
// kept, is read again by the next save, not taken for what it was.
func TestSaveRereadsAFileChangedWithinItsTimesGrain(t *testing.T) {
	ctx := context.Background()
	m := &memRemote{objs: map[string][]byte{}}
	s := New(m, "c")
	d := t.TempDir()
	writeTree(t, d, map[string]string{"old": "old one", "new": "aaaa"})
	ago(t, d)
	now := time.Now().Truncate(time.Second)
	f := filepath.Join(d, "new")
	_ = os.Chtimes(f, now, now)
	snap1, _, err := s.Save(ctx, d, "s1", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The same size, the same time, other content: as a tool that
	// restores times, or a filesystem with seconds, would leave it.
	if err := os.WriteFile(f, []byte("bbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(f, now, now)
	snap2, st, err := s.Save(ctx, d, "s2", snap1)
	if err != nil {
		t.Fatal(err)
	}
	if st.Hashed != 4 {
		t.Fatalf("hashed %d bytes, want the 4 of the racy file only", st.Hashed)
	}
	out := t.TempDir()
	if err := New(m, "c").Restore(ctx, snap2, out, "", nil, &Stats{}); err != nil {
		t.Fatal(err)
	}
	if got := readTree(t, out); got["new"] != "bbbb" || got["old"] != "old one" {
		t.Fatalf("restored %v", got)
	}
}

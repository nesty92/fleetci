package filestore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"gitlab.com/gitlab-org/gitlab-runner/commands/helpers/archive"
	"gitlab.com/gitlab-org/gitlab-runner/commands/helpers/archive/tarzstd"
)

// gitlabArchive is the tar.zst GitLab's cache archiver makes of the given
// paths under dir, as a job's cache upload sends it.
func gitlabArchive(t *testing.T, dir string, paths ...string) []byte {
	t.Helper()
	files := map[string]os.FileInfo{}
	for _, p := range paths {
		_ = filepath.Walk(filepath.Join(dir, p), func(f string, fi os.FileInfo, err error) error {
			if err == nil {
				files[f] = fi
			}
			return err
		})
	}
	var b bytes.Buffer
	a, _ := tarzstd.NewArchiver(&b, dir, archive.FastestCompression)
	if err := a.Archive(context.Background(), files); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// A job's cache upload, saved from the stream with nothing on a disk, and
// streamed back, unpacks with GitLab's own extractor to the same files;
// the same upload again packs no file.
func TestSaveTarTakesGitLabsArchiveAndStreamsItBack(t *testing.T) {
	ctx := context.Background()
	m := &memRemote{objs: map[string][]byte{}}
	project := t.TempDir()
	writeTree(t, project, map[string]string{
		"node_modules/a/index.js": strings.Repeat("a", 3000),
		"node_modules/b/index.js": strings.Repeat("a", 3000), // the same content
		".cache/go/mod/x@v1/x.go": "package x",
		"src/main.go":             "not cached",
	})
	_ = os.Symlink("a", filepath.Join(project, "node_modules", "link"))
	// Read-only, as Go's module cache keeps its folders.
	ro := filepath.Join(project, ".cache", "go", "mod", "x@v1")
	_ = os.Chmod(ro, 0o555)
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })

	up := gitlabArchive(t, project, "node_modules", ".cache/go/mod")
	zr, _ := zstd.NewReader(bytes.NewReader(up))
	s := New(m, "c")
	_, st, err := s.SaveTar(ctx, zr, "key-1")
	zr.Close()
	if err != nil {
		t.Fatal(err)
	}
	if st.NewFiles != 2 {
		t.Fatalf("packed %d contents, want 2: two paths share one", st.NewFiles)
	}

	s2 := New(m, "c")
	if err := s2.Load(ctx, &Stats{}); err != nil {
		t.Fatal(err)
	}
	zr, _ = zstd.NewReader(bytes.NewReader(up))
	_, st2, err := s2.SaveTar(ctx, zr, "key-2")
	zr.Close()
	if err != nil {
		t.Fatal(err)
	}
	if st2.NewFiles != 0 {
		t.Fatalf("the same upload packed %d contents again", st2.NewFiles)
	}

	s3 := New(m, "c")
	snap, err := s3.Snap(ctx, "key-2", &Stats{})
	if err != nil {
		t.Fatal(err)
	}
	var down bytes.Buffer
	if err := s3.Stream(ctx, snap, &down, &Stats{}); err != nil {
		t.Fatal(err)
	}
	// The helper downloads to a file, then extracts it.
	f := filepath.Join(t.TempDir(), "cache.tar.zst")
	if err := os.WriteFile(f, down.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	fh, _ := os.Open(f)
	defer fh.Close()
	dst := t.TempDir()
	x, _ := tarzstd.NewExtractor(fh, int64(down.Len()), dst)
	if err := x.Extract(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dst, ".cache", "go", "mod", "x@v1"), 0o755) })

	want := readTree(t, project)
	delete(want, "src/main.go")
	got := readTree(t, dst)
	if len(got) != len(want) {
		t.Fatalf("unpacked %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: %q, want %q", k, got[k], v)
		}
	}
	if l, _ := os.Readlink(filepath.Join(dst, "node_modules", "link")); l != "a" {
		t.Fatalf("link is %q", l)
	}
	if fi, err := os.Stat(filepath.Join(dst, ".cache", "go", "mod", "x@v1")); err != nil || fi.Mode().Perm() != 0o555 {
		t.Fatalf("x@v1: %v %v, want read-only", fi.Mode(), err)
	}
}

func TestSaveTarRefusesPathsOutside(t *testing.T) {
	for _, n := range []string{"../x", "/etc/x", "a/../../x"} {
		if _, err := tarPath(n); err == nil {
			t.Fatalf("%q: no error", n)
		}
	}
	for n, want := range map[string]string{"./a/b/": "a/b", "a": "a", "./": ""} {
		if got, err := tarPath(n); err != nil || got != want {
			t.Fatalf("%q: %q %v, want %q", n, got, err, want)
		}
	}
}

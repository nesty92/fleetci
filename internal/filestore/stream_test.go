package filestore

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestStreamIsATarZstThatTarUnpacks streams a snapshot whose files sit in
// two saves' packs, and unpacks it as actions/cache would, with the
// system's tar and unzstd.
func TestStreamIsATarZstThatTarUnpacks(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("no zstd")
	}
	ctx := context.Background()
	PackSize = 200
	defer func() { PackSize = 16 << 20 }()
	m := &memRemote{objs: map[string][]byte{}}
	s := New(m, "c")
	long := strings.Repeat("deep/", 30) + "name-past-the-ustar-limit"
	d1 := t.TempDir()
	writeTree(t, d1, map[string]string{"a/1": strings.Repeat("x", 1000), "a/empty": "", "b/2": "two", long: "long"})
	_ = os.Symlink("1", filepath.Join(d1, "a", "link"))
	snap1, _, err := s.Save(ctx, d1, "s1", nil)
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, d1, map[string]string{"b/2": "two, changed", "c/3": strings.Repeat("y", 513), "ro/x/4": "four"})
	// Read-only folders, as Go's module cache keeps them.
	for _, d := range []string{"ro/x", "ro"} {
		if err := os.Chmod(filepath.Join(d1, d), 0o555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(d1, "ro"), 0o755)
		_ = os.Chmod(filepath.Join(d1, "ro", "x"), 0o755)
	})
	snap2, _, err := s.Save(ctx, d1, "s2", snap1)
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	var st Stats
	if err := New(m, "c").Stream(ctx, snap2, &out, &st); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	cmd := exec.Command("tar", "-xf", "-", "-P", "-C", dst, "--use-compress-program", "unzstd")
	cmd.Stdin = &out
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tar: %v %s", err, b)
	}
	want, got := readTree(t, d1), readTree(t, dst)
	if len(got) != len(want) {
		t.Fatalf("unpacked %d files, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: %q, want %q", k, got[k], v)
		}
	}
	if l, _ := os.Readlink(filepath.Join(dst, "a", "link")); l != "1" {
		t.Fatalf("link is %q", l)
	}
	for _, d := range []string{"ro", "ro/x"} {
		fi, err := os.Stat(filepath.Join(dst, d))
		if err != nil || fi.Mode().Perm() != 0o555 {
			t.Fatalf("%s: %v %v, want read-only", d, fi.Mode(), err)
		}
		_ = os.Chmod(filepath.Join(dst, d), 0o755)
	}
}

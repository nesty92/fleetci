package actionscache

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// actions/cache's tar arguments on Linux, as @actions/cache 4 makes them.
func createArgs(archive, ws, manifest string) []string {
	return []string{"--posix", "-cf", archive, "--exclude", archive, "-P", "-C", ws, "--files-from", manifest, "--use-compress-program", "zstdmt --long=30"}
}

func TestParseTarArgs(t *testing.T) {
	c, ok := parseCreate(createArgs("/t/cache.tzst", "/w", "/t/manifest.txt"))
	if !ok || c.archive != "/t/cache.tzst" || c.dir != "/w" || c.manifest != "/t/manifest.txt" {
		t.Fatalf("create: %+v %v", c, ok)
	}
	if _, ok := parseCreate([]string{"-czf", "x.tgz", "dir"}); ok {
		t.Fatal("a job's own tar is not actions/cache's")
	}
	if _, ok := parseCreate(append(createArgs("a", "/w", "m")[:10], "--use-compress-program", "xz")); ok {
		t.Fatal("a compression actions/cache doesn't use")
	}
	x, ok := parseExtract([]string{"-xf", "/t/cache.tzst", "-P", "-C", "/w", "--use-compress-program", "unzstd", "--long=30"})
	if !ok || x.archive != "/t/cache.tzst" || x.dir != "/w" {
		t.Fatalf("extract: %+v %v", x, ok)
	}
	if _, ok := parseExtract([]string{"-xzf", "x.tgz"}); ok {
		t.Fatal("a job's own extract")
	}
}

func treeDirs(t *testing.T) (root, ws, tmp string) {
	t.Helper()
	d := t.TempDir()
	root, ws, tmp = filepath.Join(d, "trees"), filepath.Join(d, "ws"), filepath.Join(d, "tmp")
	for _, p := range []string{root, ws, tmp} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := TreeRoot
	TreeRoot = root
	t.Cleanup(func() { TreeRoot = old })
	return
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSaveTreeCopiesTheFoldersAndPacksThemLater(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("no tar")
	}
	root, ws, tmp := treeDirs(t)
	write(t, filepath.Join(ws, "node_modules/a/index.js"), "a")
	write(t, filepath.Join(ws, "../home/.cache/go-build/00/x"), "x")
	man := filepath.Join(tmp, "manifest.txt")
	write(t, man, "node_modules\n../home/.cache/go-build\n")
	arch := filepath.Join(tmp, "cache.tzst")
	a := createArgs(arch, ws, man)[:10] // uncompressed, so the test needs no zstd
	c, ok := parseCreate(a)
	if !ok {
		t.Fatal("not parsed")
	}
	if err := saveTree(c); err != nil {
		t.Fatal(err)
	}
	m, ok := readMarker(arch)
	if !ok || !IsMarker(arch) {
		t.Fatal("the archive is not a marker")
	}
	if b, err := os.ReadFile(filepath.Join(root, m.ID, "0/a/index.js")); err != nil || string(b) != "a" {
		t.Fatalf("tree 0: %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(root, m.ID, "1/00/x")); err != nil || string(b) != "x" {
		t.Fatalf("tree 1: %q %v", b, err)
	}
	if !slices.Equal(m.Paths, []string{"node_modules", "../home/.cache/go-build"}) || m.Dir != ws {
		t.Fatalf("marker: %+v", m)
	}

	out := filepath.Join(tmp, "packed.tar")
	if err := Pack(arch, out); err != nil {
		t.Fatal(err)
	}
	list, err := exec.Command("tar", "-tf", out, "-P").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"node_modules/a/index.js", "../home/.cache/go-build/00/x"} {
		if !strings.Contains(string(list), want) {
			t.Fatalf("packed archive lacks %s:\n%s", want, list)
		}
	}
	if _, err := os.Stat(out + ".manifest"); !os.IsNotExist(err) {
		t.Fatal("Pack left its manifest")
	}
}

func TestSaveTreeTakesOnlyFolders(t *testing.T) {
	_, ws, tmp := treeDirs(t)
	write(t, filepath.Join(ws, "file.txt"), "f")
	man := filepath.Join(tmp, "manifest.txt")
	write(t, man, "file.txt\n")
	c, _ := parseCreate(createArgs(filepath.Join(tmp, "cache.tzst"), ws, man))
	if err := saveTree(c); err == nil {
		t.Fatal("a file saved as a tree: tar packs it")
	}
}

func TestReadMarkerRefusesArchives(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "a")
	write(t, p, "\x28\xb5\x2f\xfd not a marker")
	if _, ok := readMarker(p); ok || IsMarker(p) {
		t.Fatal("an archive read as a marker")
	}
	write(t, p, treeMagic+"abc\n{\"id\":\"other\"}\n")
	if _, ok := readMarker(p); ok {
		t.Fatal("a marker whose id disagrees")
	}
}

func TestSweepTreesKeepsNamedOnes(t *testing.T) {
	root, ws, tmp := treeDirs(t)
	store := filepath.Join(filepath.Dir(root), "store")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	s := New(Config{Store: store})
	write(t, filepath.Join(ws, "d/x"), "x")
	man := filepath.Join(tmp, "manifest.txt")
	write(t, man, "d\n")
	arch := filepath.Join(tmp, "cache.tzst")
	c, _ := parseCreate(createArgs(arch, ws, man))
	if err := saveTree(c); err != nil {
		t.Fatal(err)
	}
	m, _ := readMarker(arch)
	b, _ := os.ReadFile(arch)
	write(t, filepath.Join(store, "e1.blob"), string(b))
	write(t, filepath.Join(root, "orphan/0/y"), "y")
	old := time.Now().Add(-2 * time.Hour)
	for _, id := range []string{m.ID, "orphan"} {
		_ = os.Chtimes(filepath.Join(root, id), old, old)
	}
	write(t, filepath.Join(root, "fresh/0/z"), "z")
	s.sweepTrees()
	for id, want := range map[string]bool{m.ID: true, "orphan": false, "fresh": true} {
		_, err := os.Stat(filepath.Join(root, id))
		if (err == nil) != want {
			t.Errorf("tree %s kept %v, want %v", id, err == nil, want)
		}
	}
}

func TestFromContainer(t *testing.T) {
	from := func(remote string) *http.Request {
		r, _ := http.NewRequest(http.MethodPost, "http://x/", nil)
		r.RemoteAddr = remote
		return r
	}
	// The VM's eth0 and docker0.
	own := []net.IP{net.ParseIP("10.0.0.5"), net.ParseIP("172.17.0.1")}
	if fromContainerAmong(from("10.0.0.5:5000"), own) {
		t.Error("the VM at its bridge's address, from its main interface's")
	}
	if fromContainerAmong(from("172.17.0.1:5000"), own) {
		t.Error("the VM, from its bridge's address")
	}
	if fromContainerAmong(from("127.0.0.1:5000"), own) {
		t.Error("the VM, by loopback")
	}
	if !fromContainerAmong(from("172.17.0.2:5000"), own) {
		t.Error("a container")
	}
}

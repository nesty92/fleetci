package actionscache

// Trees: actions/cache's folders kept as folders on the cache disk, not
// as archives, so a restore mounts the folder and a save of a folder that
// was restored that way copies nothing.
//
// fleetci-actions-cache is also the job's tar (TarMain), through a link
// named tar ahead of the system's in the job's PATH: actions/cache runs
// tar, found on the PATH (@actions/cache's getTarPath), to pack and
// unpack its archives. Every other use of tar goes to the system's
// unchanged.
//
// A save (tar --posix -cf <archive> ... -C <workspace> --files-from
// <manifest>) puts each folder of the manifest under TreeRoot/<id>/<n>,
// copying it once if it isn't there already, and writes a marker as the
// archive: what the tree is, and the save's own tar arguments.
// actions/cache saves the marker as the entry, as any archive. A restore
// whose archive is a marker (tar -xf <marker> ...) bind-mounts each
// folder of the tree at its path; the job works in the tree itself, on
// the disk. The disk is the job's ref's own copy-on-write layer, so what
// the job changes stays in its layer, and the layer it started from is
// unchanged: a tree is a sticky disk's folder (Blacksmith's sticky
// disks, Namespace's cache volumes), here behind actions/cache.
//
// The bucket gets archives, never markers: Server.flush packs a marker's
// tree with the save's own tar arguments, after the job, so a VM without
// the disk, or a container's tar, restores it as any entry.
//
// A tree changes when a job that mounted it saves again; the entries
// that name it then restore its newest state. That is what a cache
// restored from a restore key gets anyway; an entry restored by its exact
// key is not saved again.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// TreeRoot is where trees live on the cache disk.
var TreeRoot = "/cache/trees"

// treeMagic starts a marker: its first line is treeMagic and the tree's id.
const treeMagic = "FLEETCI-TREE v1 "

// marker is what a marker holds after its first line.
type marker struct {
	ID string `json:"id"`
	// Dir is the -C directory of the save; Paths, its manifest.
	Dir   string   `json:"dir"`
	Paths []string `json:"paths"`
	// Args are the save's tar arguments, its archive's name and the
	// manifest's left out (packArgs).
	Args []string `json:"args"`
}

// realTars are where the system's tar is, in order.
var realTars = []string{"/usr/bin/tar", "/bin/tar"}

func realTar() string {
	for _, p := range realTars {
		if isFile(p) {
			return p
		}
	}
	return "tar"
}

// TarMain is fleetci-actions-cache run as tar.
func TarMain(args []string) int {
	if c, ok := parseCreate(args); ok && treesOn() {
		if err := saveTree(c); err == nil {
			return 0
		} else {
			fmt.Fprintf(os.Stderr, "fleetci cache: packing as an archive (%v)\n", err)
		}
	}
	if x, ok := parseExtract(args); ok {
		if m, ok := readMarker(x.archive); ok {
			if err := mountTree(m, x.dir); err != nil {
				fmt.Fprintf(os.Stderr, "fleetci cache: %v\n", err)
				return 2
			}
			return 0
		}
	}
	return execTar(args)
}

// treesOn is whether this VM keeps trees: its cache disk has TreeRoot.
func treesOn() bool {
	st, err := os.Stat(TreeRoot)
	return err == nil && st.IsDir()
}

func execTar(args []string) int {
	p := realTar()
	if err := syscall.Exec(p, append([]string{"tar"}, args...), os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "fleetci cache: running %s: %v\n", p, err)
	}
	return 127
}

// create is a save's tar, as @actions/cache's createTar runs it.
type create struct {
	archive, dir, manifest string
	compress               []string // the arguments after the manifest's
}

// parseCreate reads @actions/cache's createTar arguments on Linux:
// --posix -cf A --exclude A -P -C D --files-from M, then its compression
// (--use-compress-program zstdmt [--long=30], or -z). Anything else is
// not actions/cache's, and goes to tar.
func parseCreate(a []string) (create, bool) {
	var c create
	if len(a) < 10 || a[0] != "--posix" || a[1] != "-cf" || a[3] != "--exclude" || a[4] != a[2] ||
		a[5] != "-P" || a[6] != "-C" || a[8] != "--files-from" {
		return c, false
	}
	c.archive, c.dir, c.manifest, c.compress = a[2], a[7], a[9], a[10:]
	return c, compressOK(c.compress)
}

// extract is a restore's tar.
type extract struct {
	archive, dir string
}

// parseExtract reads @actions/cache's extractTar arguments on Linux: -xf A
// -P -C D, then its compression.
func parseExtract(a []string) (extract, bool) {
	if len(a) < 5 || a[0] != "-xf" || a[2] != "-P" || a[3] != "-C" {
		return extract{}, false
	}
	return extract{archive: a[1], dir: a[4]}, compressOK(a[5:])
}

func compressOK(a []string) bool {
	switch {
	case len(a) == 0:
		return true
	case len(a) == 1:
		return a[0] == "-z"
	case a[0] != "--use-compress-program":
		return false
	}
	for _, x := range a[1:] {
		f := strings.Fields(x)
		if len(f) == 0 || !(strings.HasPrefix(f[0], "zstd") || strings.HasPrefix(f[0], "unzstd") || strings.HasPrefix(f[0], "--long")) {
			return false
		}
	}
	return true
}

// readMarker is the marker an archive is, if it is one.
func readMarker(path string) (marker, bool) {
	var m marker
	f, err := os.Open(path)
	if err != nil {
		return m, false
	}
	defer f.Close()
	br := bufio.NewReader(io.LimitReader(f, 1<<20))
	first, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(first, treeMagic) {
		return m, false
	}
	if json.NewDecoder(br).Decode(&m) != nil || m.ID == "" || m.ID != strings.TrimSpace(first[len(treeMagic):]) {
		return m, false
	}
	return m, true
}

// IsMarker is whether the file at path is a tree's marker.
func IsMarker(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	b := make([]byte, len(treeMagic))
	_, err = io.ReadFull(f, b)
	return err == nil && string(b) == treeMagic
}

// at is where a manifest path is.
func at(dir, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(dir, p)
}

func saveTree(c create) error {
	raw, err := os.ReadFile(c.manifest)
	if err != nil {
		return err
	}
	var paths []string
	for _, p := range strings.Split(string(raw), "\n") {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return errors.New("no paths")
	}
	mounts := treeMounts()
	// The tree its folders were restored from, when they all were.
	id := ""
	for i, p := range paths {
		st, err := os.Stat(at(c.dir, p))
		if err != nil || !st.IsDir() {
			return fmt.Errorf("%s is not a folder", p)
		}
		src := mounts[at(c.dir, p)]
		want := ""
		if src != "" {
			want = strings.TrimSuffix(src, "/"+strconv.Itoa(i))
		}
		if i == 0 {
			id = want
		} else if want != id {
			id = ""
		}
	}
	if id != "" && mountedAll(mounts, c.dir, paths, id) {
		return writeMarker(c, id, paths)
	}
	// Copied in, once: a new tree.
	id = randHex(12)
	root := filepath.Join(TreeRoot, id)
	for i, p := range paths {
		dst := filepath.Join(root, strconv.Itoa(i))
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		if out, err := exec.Command("cp", "-a", "--reflink=auto", at(c.dir, p)+"/.", dst+"/").CombinedOutput(); err != nil {
			_ = os.RemoveAll(root)
			return fmt.Errorf("copying %s: %v: %s", p, err, bytes.TrimSpace(out))
		}
	}
	return writeMarker(c, id, paths)
}

func mountedAll(mounts map[string]string, dir string, paths []string, id string) bool {
	for i, p := range paths {
		if mounts[at(dir, p)] != id+"/"+strconv.Itoa(i) {
			return false
		}
	}
	return true
}

func writeMarker(c create, id string, paths []string) error {
	var b bytes.Buffer
	b.WriteString(treeMagic + id + "\n")
	m := marker{ID: id, Dir: c.dir, Paths: paths, Args: packArgs(c)}
	if err := json.NewEncoder(&b).Encode(m); err != nil {
		return err
	}
	return os.WriteFile(c.archive, b.Bytes(), 0o644)
}

// packArgs are the save's tar arguments, to pack the tree as an archive
// later (Pack): the archive's name and the manifest's are filled in then.
func packArgs(c create) []string {
	return append([]string{"--posix", "-cf", "@ARCHIVE@", "--exclude", "@ARCHIVE@", "-P", "-C", c.dir, "--files-from", "@MANIFEST@"}, c.compress...)
}

func mountTree(m marker, dir string) error {
	mounts := treeMounts()
	for i, p := range m.Paths {
		src := filepath.Join(TreeRoot, m.ID, strconv.Itoa(i))
		if st, err := os.Stat(src); err != nil || !st.IsDir() {
			return fmt.Errorf("the tree %s is not on this disk", m.ID)
		}
		dst := at(dir, p)
		if mounts[dst] == m.ID+"/"+strconv.Itoa(i) {
			continue
		}
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		if out, err := exec.Command("sudo", "-n", "mount", "--bind", src, dst).CombinedOutput(); err != nil {
			return fmt.Errorf("mounting %s: %v: %s", p, err, bytes.TrimSpace(out))
		}
	}
	fmt.Fprintf(os.Stderr, "fleetci cache: %d folder(s) mounted from the cache disk, nothing unpacked\n", len(m.Paths))
	return nil
}

// treeMounts are the bind mounts of trees: where each is mounted → its
// "<id>/<n>" under TreeRoot.
func treeMounts() map[string]string {
	out := map[string]string{}
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return out
	}
	rel := "/" + filepath.Base(TreeRoot) + "/"
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || !strings.HasPrefix(unescape(f[3]), rel) {
			continue
		}
		out[unescape(f[4])] = strings.TrimPrefix(unescape(f[3]), rel)
	}
	return out
}

// unescape undoes mountinfo's octal escapes (\040 is a space).
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Pack makes the archive a marker stands for, at out, with the save's own
// tar arguments: what actions/cache would have saved.
func Pack(markerPath, out string) error {
	m, ok := readMarker(markerPath)
	if !ok {
		return errors.New("not a marker")
	}
	man := out + ".manifest"
	if err := os.WriteFile(man, []byte(strings.Join(m.Paths, "\n")), 0o644); err != nil {
		return err
	}
	defer os.Remove(man)
	args := make([]string, len(m.Args))
	for i, a := range m.Args {
		args[i] = strings.NewReplacer("@ARCHIVE@", out, "@MANIFEST@", man).Replace(a)
	}
	cmd := exec.Command(realTar(), args...)
	cmd.Dir = filepath.Dir(out)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("packing the tree %s: %v: %s", m.ID, err, bytes.TrimSpace(b))
	}
	return nil
}

// fromContainer is whether r came from one of the VM's containers, not
// the VM itself: the VM reaches its own address, so the two ends are one
// address; a container's end is its own.
func fromContainer(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	la, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	tcp, ok := la.(*net.TCPAddr)
	if ip == nil || !ok || ip.IsLoopback() {
		return false
	}
	return !ip.Equal(tcp.IP)
}

// markerUse says whether an entry's blob is a tree's marker, and if so
// whether r's client can restore it: only the VM's own tar is fleetci's,
// and the tree has to be on the disk still.
func (s *Server) markerUse(r *http.Request, eid string) (isMarker, usable, gone bool) {
	m, ok := readMarker(s.blobPath(eid))
	if !ok {
		return false, true, false
	}
	if st, err := os.Stat(filepath.Join(TreeRoot, m.ID)); err != nil || !st.IsDir() {
		return true, false, true
	}
	return true, !fromContainer(r), false
}

// sweepTrees removes the trees no entry's marker names, none mounts, and
// that are an hour old: a save's tree is named once its entry is kept.
func (s *Server) sweepTrees() {
	des, err := os.ReadDir(TreeRoot)
	if err != nil {
		return
	}
	named := map[string]bool{}
	if bs, err := os.ReadDir(s.cfg.Store); err == nil {
		for _, b := range bs {
			if eid, ok := strings.CutSuffix(b.Name(), ".blob"); ok {
				if m, ok := readMarker(s.blobPath(eid)); ok {
					named[m.ID] = true
				}
			}
		}
	}
	for _, v := range treeMounts() {
		named[strings.SplitN(v, "/", 2)[0]] = true
	}
	for _, de := range des {
		info, err := de.Info()
		if err != nil || named[de.Name()] || time.Since(info.ModTime()) < time.Hour {
			continue
		}
		if os.RemoveAll(filepath.Join(TreeRoot, de.Name())) == nil {
			s.logf("removed the tree %s, which no entry names", de.Name())
		}
	}
}

// localOK says the disk's copy of the entry is one r's client can
// restore: an archive, or a tree's marker for the VM's own tar with its
// tree on the disk.
func (s *Server) localOK(r *http.Request, eid string) bool {
	isM, ok, _ := s.markerUse(r, eid)
	return !isM || ok
}

// containerMarker says the disk's copy is a tree's marker, its tree on
// the disk, and r's client a container: the bucket's archive is for it.
func (s *Server) containerMarker(r *http.Request, eid string) bool {
	isM, ok, gone := s.markerUse(r, eid)
	return isM && !ok && !gone
}

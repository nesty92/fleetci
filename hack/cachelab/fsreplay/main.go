// Command fsreplay replays a chain of actions/cache entries into a
// filestore in a bucket, and measures it against keeping whole archives.
//
//	fsreplay -prefix P -work DIR <entry.tzst>...
//
// The entries are given in the order they were saved, and can be named
// pipes filled from the bucket. For each, as a job would:
//
//   - archive restore: the entry is unpacked into a folder (download,
//     zstd and tar, streamed);
//   - archive save: the folder is packed again with tar and zstd -T0 into a
//     file and uploaded whole, as actions/cache does;
//   - store save: the folder is saved into the store, knowing the last
//     save's snapshot, as the node does;
//   - cold restore: a store that has read nothing restores it into an
//     empty folder (a VM with no cache disk, or another node), and the
//     result is checked against the folder;
//   - warm restore: it is restored next to the last entry's folder, which
//     the node holds (copied files from there, fetched the rest);
//   - stock restore: the store streams it as one tar.zst into the system's
//     tar and unzstd, as actions/cache unpacks, and the result is checked.
//
// The bucket is a [runners.cache] S3 one, from FS_S3_ADDR, FS_BUCKET,
// AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY; the store uses signed links
// only, as a VM would.
package main

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"gitlab.com/gitlab-org/gitlab-runner/cache/cacheconfig"

	"github.com/nesty92/fleetci/internal/cachebucket"
	"github.com/nesty92/fleetci/internal/filestore"
)

type row struct {
	Entry                    string
	ArchiveBytes             int64
	ArchiveRestore           float64
	ArchiveSave              float64
	ArchiveSaveBytes         int64
	Store                    filestore.Stats
	StoreSave                float64
	Cold, Warm               filestore.Stats
	ColdRestore, WarmRestore float64
	ColdIndexLoad            float64
	Stock                    filestore.Stats
	StockRestore             float64
	StockBytes               int64
	Files                    int
	FolderBytes              int64
}

func main() {
	prefix := flag.String("prefix", "", "the store's prefix in the bucket")
	work := flag.String("work", "", "a folder for the entries' folders")
	flag.Parse()
	if *prefix == "" || *work == "" {
		fail(fmt.Errorf("-prefix and -work are needed"))
	}
	b, err := cachebucket.New(&cachebucket.Config{Type: "s3", S3: &cacheconfig.CacheS3Config{
		ServerAddress: os.Getenv("FS_S3_ADDR"), BucketName: os.Getenv("FS_BUCKET"), Insecure: true,
		AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"), SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
	}})
	if err != nil {
		fail(err)
	}
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 32}}
	remote := filestore.Linked{B: b, C: client}
	ctx := context.Background()
	s := filestore.New(remote, *prefix)

	var prevDir string
	var prevSnap *filestore.Snapshot
	var rows []row
	for i, path := range flag.Args() {
		r := row{Entry: filepath.Base(path)}
		cur := filepath.Join(*work, fmt.Sprintf("e%03d", i))
		must(os.RemoveAll(cur))

		t := time.Now()
		n, err := unpack(path, cur)
		if err != nil {
			fail(fmt.Errorf("%s: %w", path, err))
		}
		r.ArchiveBytes, r.ArchiveRestore = n, time.Since(t).Seconds()

		t = time.Now()
		r.ArchiveSaveBytes, err = archiveSave(ctx, b, client, cur, *prefix+"-archives/last", *work)
		if err != nil {
			fail(err)
		}
		r.ArchiveSave = time.Since(t).Seconds()

		snap, st, err := s.Save(ctx, cur, fmt.Sprintf("%03d", i), prevSnap)
		if err != nil {
			fail(err)
		}
		r.Store, r.StoreSave, r.Files, r.FolderBytes = st, st.Elapsed.Seconds(), st.Files, st.Bytes

		cold := filepath.Join(*work, "cold")
		must(os.RemoveAll(cold))
		cs := filestore.New(remote, *prefix)
		t = time.Now()
		cSnap, err := cs.Snap(ctx, fmt.Sprintf("%03d", i), &r.Cold)
		if err == nil {
			err = cs.Restore(ctx, cSnap, cold, "", nil, &r.Cold)
		}
		if err != nil {
			fail(fmt.Errorf("cold restore %d: %w", i, err))
		}
		r.ColdRestore, r.ColdIndexLoad = time.Since(t).Seconds(), r.Cold.IndexLoad.Seconds()
		if err := same(cur, cold); err != nil {
			fail(fmt.Errorf("cold restore %d differs: %w", i, err))
		}
		must(os.RemoveAll(cold))

		t = time.Now()
		r.StockBytes, err = stock(ctx, filestore.New(remote, *prefix), cSnap, cold, &r.Stock)
		if err != nil {
			fail(fmt.Errorf("stock restore %d: %w", i, err))
		}
		r.StockRestore = time.Since(t).Seconds()
		if err := same(cur, cold); err != nil {
			fail(fmt.Errorf("stock restore %d differs: %w", i, err))
		}
		must(os.RemoveAll(cold))

		if prevDir != "" {
			warm := filepath.Join(*work, "warm")
			must(os.RemoveAll(warm))
			t = time.Now()
			if err := s.Restore(ctx, snap, warm, prevDir, prevSnap, &r.Warm); err != nil {
				fail(fmt.Errorf("warm restore %d: %w", i, err))
			}
			r.WarmRestore = time.Since(t).Seconds()
			if i%6 == 5 {
				if err := same(cur, warm); err != nil {
					fail(fmt.Errorf("warm restore %d differs: %w", i, err))
				}
			}
			must(os.RemoveAll(warm))
			must(os.RemoveAll(prevDir))
		}
		prevDir, prevSnap = cur, snap
		rows = append(rows, r)
		j, _ := json.Marshal(r)
		fmt.Fprintln(os.Stderr, string(j))
		fmt.Printf("%3d %-12s files %6d %7.0fM | archive %6.0fM restore %5.1fs save %5.1fs | store up %6.1fM in %3d puts, hashed %6.0fM, %4d dirs listed, save %5.1fs | cold %6.0fM %4d gets %5.1fs (index %.1fs) | warm %6.1fM %4d gets, %5d local, %5.1fs | stock %6.0fM %5.1fs\n",
			i, short(r.Entry), r.Files, mb(r.FolderBytes), mb(r.ArchiveBytes), r.ArchiveRestore, r.ArchiveSave,
			mb(st.PutBytes), st.Put, mb(st.Hashed), st.NewTrees, r.StoreSave,
			mb(r.Cold.GetBytes), r.Cold.Get, r.ColdRestore, r.ColdIndexLoad,
			mb(r.Warm.GetBytes), r.Warm.Get, r.Warm.Local, r.WarmRestore, mb(r.StockBytes), r.StockRestore)
	}
	if prevDir != "" {
		must(os.RemoveAll(prevDir))
	}
	summary(rows)
}

func summary(rows []row) {
	var arch, archSave, up, puts int64
	var aR, aS, sS, cR, wR, kR []float64
	var cold, warm int64
	for i, r := range rows {
		arch += r.ArchiveBytes
		archSave += r.ArchiveSaveBytes
		up += r.Store.PutBytes
		puts += r.Store.Put
		cold += r.Cold.GetBytes
		aR, aS, sS, cR = append(aR, r.ArchiveRestore), append(aS, r.ArchiveSave), append(sS, r.StoreSave), append(cR, r.ColdRestore)
		if r.StockRestore > 0 {
			kR = append(kR, r.StockRestore)
		}
		if i > 0 {
			wR = append(wR, r.WarmRestore)
			warm += r.Warm.GetBytes
		}
	}
	fmt.Printf("\n%d entries\n", len(rows))
	fmt.Printf("uploaded:  archives %.0fM (as saved %.0fM); store %.0fM in %d objects (%.0f%%)\n", mb(archSave), mb(arch), mb(up), puts, 100*float64(up)/float64(archSave))
	fmt.Printf("save p50:  archive %.1fs, store %.1fs (first save included)\n", med(aS), med(sS))
	fmt.Printf("restore p50: archive %.1fs; store cold %.1fs (%.0fM fetched in all); store warm %.1fs (%.0fM fetched in all)\n", med(aR), med(cR), mb(cold), med(wR), mb(warm))
	fmt.Printf("stock client p50: %.1fs, from the store's stream into tar and unzstd\n", med(kR))
}

// stock streams snap into the system's tar, unpacking into dir as
// actions/cache does, and says how long the stream was.
func stock(ctx context.Context, s *filestore.Store, snap *filestore.Snapshot, dir string, st *filestore.Stats) (int64, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	cmd := exec.CommandContext(ctx, "tar", "-xf", "-", "-P", "-C", dir, "--use-compress-program", "unzstd")
	in, err := cmd.StdinPipe()
	if err != nil {
		return 0, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	c := &countW{w: in}
	serr := s.Stream(ctx, snap, c, st)
	in.Close()
	if err := cmd.Wait(); err != nil {
		return c.n, fmt.Errorf("tar: %w", err)
	}
	return c.n, serr
}

type countW struct {
	w io.Writer
	n int64
}

func (c *countW) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// unpack streams a tar.zst into dir, and says how many bytes it read.
func unpack(path, dir string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	c := &counter{r: f}
	d, err := zstd.NewReader(c, zstd.WithDecoderMaxWindow(1<<31))
	if err != nil {
		return 0, err
	}
	defer d.Close()
	tr := tar.NewReader(d)
	type dt struct {
		p string
		t time.Time
	}
	var dirs []dt
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return c.n, err
		}
		name := filepath.Clean(h.Name)
		if !filepath.IsLocal(name) {
			return c.n, fmt.Errorf("%q leaves the folder", h.Name)
		}
		full := filepath.Join(dir, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(full, 0o755); err != nil {
				return c.n, err
			}
			dirs = append(dirs, dt{full, h.ModTime})
			continue
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return c.n, err
			}
			w, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(h.Mode).Perm())
			if err != nil {
				return c.n, err
			}
			if _, err := io.Copy(w, tr); err != nil {
				w.Close()
				return c.n, err
			}
			if err := w.Close(); err != nil {
				return c.n, err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return c.n, err
			}
			if err := os.Symlink(h.Linkname, full); err != nil {
				return c.n, err
			}
			continue
		default:
			continue
		}
		_ = os.Chtimes(full, h.ModTime, h.ModTime)
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Chtimes(dirs[i].p, dirs[i].t, dirs[i].t)
	}
	_, _ = io.Copy(io.Discard, c)
	return c.n, nil
}

// archiveSave packs dir as actions/cache does on Linux, uploads it whole,
// and says how big it was.
func archiveSave(ctx context.Context, b cachebucket.Bucket, c *http.Client, dir, name, work string) (int64, error) {
	out := filepath.Join(work, "archive.tzst")
	defer os.Remove(out)
	cmd := exec.CommandContext(ctx, "sh", "-c", `tar --posix -cf - -C "$1" . | zstd -T0 -q -o "$2" -f`, "sh", dir, out)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return 0, err
	}
	f, err := os.Open(out)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fi, _ := f.Stat()
	l, err := b.Upload(ctx, name, 10*time.Minute)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, l.URL, f)
	if err != nil {
		return 0, err
	}
	for h, v := range l.Headers {
		req.Header.Set(h, v)
	}
	req.ContentLength = fi.Size()
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return 0, fmt.Errorf("PUT archive: %s", resp.Status)
	}
	return fi.Size(), nil
}

// same says how b differs from a: paths, kinds, modes, sizes and contents.
func same(a, b string) error {
	sum := func(root string) (map[string]string, error) {
		out := map[string]string{}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			info, err := d.Info()
			if err != nil {
				return err
			}
			v := info.Mode().String()
			if info.Mode().IsRegular() {
				bs, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				v += fmt.Sprintf(" %x %d", sha256.Sum256(bs), info.ModTime().Unix())
			}
			out[rel] = v
			return nil
		})
		return out, err
	}
	x, err := sum(a)
	if err != nil {
		return err
	}
	y, err := sum(b)
	if err != nil {
		return err
	}
	for k, v := range x {
		if y[k] != v {
			return fmt.Errorf("%s: %q, restored %q", k, v, y[k])
		}
	}
	if len(x) != len(y) {
		return fmt.Errorf("%d paths, restored %d", len(x), len(y))
	}
	return nil
}

type counter struct {
	r io.Reader
	n int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func short(s string) string {
	if i := strings.LastIndex(s, "-"); i > 0 && len(s)-i > 8 {
		return s[:i+9]
	}
	return s
}

func med(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	ys := append([]float64(nil), xs...)
	for i := range ys {
		for j := i + 1; j < len(ys); j++ {
			if ys[j] < ys[i] {
				ys[i], ys[j] = ys[j], ys[i]
			}
		}
	}
	return ys[len(ys)/2]
}

func mb(n int64) float64 { return float64(n) / (1 << 20) }

func must(err error) {
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "fsreplay:", err)
	os.Exit(1)
}

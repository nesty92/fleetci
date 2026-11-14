// Command fsbench times the filestore's paths on one folder, each a few
// times, with a CPU profile of each, to tune them one change at a time.
//
//	fsbench -prefix P -work DIR -n 3 [-prof DIR] <folder>
//
// Each run writes under -prefix/<time>/, which the caller deletes. For the
// folder: a first save into an empty store, a save with nothing
// changed, a restore into an empty folder (cold), a restore next to a copy
// of the folder (warm: every file local), and the stream into the
// system's tar. The bucket is FS_S3_ADDR, FS_BUCKET, AWS_ACCESS_KEY_ID and
// AWS_SECRET_ACCESS_KEY, through signed links as a VM.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/pprof"
	"sort"
	"time"

	"github.com/nesty92/fleetci/internal/cachebucket"
	"github.com/nesty92/fleetci/internal/filestore"
	"gitlab.com/gitlab-org/gitlab-runner/cache/cacheconfig"
)

func main() {
	prefix := flag.String("prefix", "github-lab/fs-bench", "the store's prefix in the bucket")
	work := flag.String("work", "", "a folder for restores")
	n := flag.Int("n", 3, "times each path runs")
	prof := flag.String("prof", "", "a folder for CPU profiles, one per path")
	only := flag.String("only", "", "run only these paths, comma-separated (save,resave,cold,warm,stock)")
	flag.Parse()
	if flag.NArg() != 1 || *work == "" {
		flag.Usage()
		os.Exit(2)
	}
	src := flag.Arg(0)
	ctx := context.Background()
	b, err := cachebucket.New(&cachebucket.Config{Type: "s3", S3: &cacheconfig.CacheS3Config{
		ServerAddress: os.Getenv("FS_S3_ADDR"), BucketName: os.Getenv("FS_BUCKET"), Insecure: true,
		AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"), SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
	}})
	must(err)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
	remote := filestore.Linked{B: b, C: client}
	want := map[string]bool{}
	for _, p := range splitComma(*only) {
		want[p] = true
	}
	on := func(p string) bool { return len(want) == 0 || want[p] }

	type res struct {
		name string
		ts   []float64
		note string
	}
	var out []res
	run := func(name string, setup func(i int), f func(i int) string) {
		if !on(name) {
			return
		}
		r := res{name: name}
		for i := range *n {
			if setup != nil {
				setup(i)
			}
			stop := startProf(*prof, name, i)
			t := time.Now()
			r.note = f(i)
			r.ts = append(r.ts, time.Since(t).Seconds())
			stop()
		}
		out = append(out, r)
		sort.Float64s(r.ts)
		fmt.Printf("%-7s min %5.2fs med %5.2fs  %s\n", name, r.ts[0], r.ts[len(r.ts)/2], r.note)
	}

	var snap *filestore.Snapshot
	runID := time.Now().Format("150405")
	fresh := func(i int) string { return fmt.Sprintf("%s/%s/r%d", *prefix, runID, i) }
	// A first save, each into its own empty store.
	run("save", nil, func(i int) string {
		s := filestore.New(remote, fresh(i))
		var st filestore.Stats
		snap, st, err = s.Save(ctx, src, "s", nil)
		must(err)
		return fmt.Sprintf("%d files %.0fM, up %.0fM in %d puts", st.Files, mb(st.Bytes), mb(st.PutBytes), st.Put)
	})
	if snap == nil {
		s := filestore.New(remote, fresh(0))
		snap, _, err = s.Save(ctx, src, "s", nil)
		must(err)
	}
	base := fresh(0)
	run("resave", nil, func(i int) string {
		s := filestore.New(remote, base)
		var st filestore.Stats
		must(s.Load(ctx, &st))
		_, st, err = s.Save(ctx, src, fmt.Sprintf("re%d", i), snap)
		must(err)
		return fmt.Sprintf("up %.1fM in %d puts", mb(st.PutBytes), st.Put)
	})
	dst := filepath.Join(*work, "dst")
	run("cold", func(int) { must(rmrf(dst)) }, func(int) string {
		s := filestore.New(remote, base)
		var st filestore.Stats
		sn, err := s.Snap(ctx, "s", &st)
		must(err)
		must(s.Restore(ctx, sn, dst, "", nil, &st))
		return fmt.Sprintf("%.0fM in %d gets", mb(st.GetBytes), st.Get)
	})
	run("warm", func(int) { must(rmrf(dst)) }, func(int) string {
		s := filestore.New(remote, base)
		var st filestore.Stats
		sn, err := s.Snap(ctx, "s", &st)
		must(err)
		must(s.Restore(ctx, sn, dst, src, snap, &st))
		return fmt.Sprintf("%.1fM in %d gets, %d local", mb(st.GetBytes), st.Get, st.Local)
	})
	run("stock", func(int) { must(rmrf(dst)); must(os.MkdirAll(dst, 0o755)) }, func(int) string {
		s := filestore.New(remote, base)
		var st filestore.Stats
		sn, err := s.Snap(ctx, "s", &st)
		must(err)
		cmd := exec.CommandContext(ctx, "tar", "-xf", "-", "-P", "-C", dst, "--use-compress-program", "unzstd")
		in, err := cmd.StdinPipe()
		must(err)
		cmd.Stderr = os.Stderr
		must(cmd.Start())
		serr := s.Stream(ctx, sn, in, &st)
		in.Close()
		must(cmd.Wait())
		must(serr)
		return fmt.Sprintf("%.0fM in %d gets", mb(st.GetBytes), st.Get)
	})
	must(rmrf(dst))
}

func startProf(dir, name string, i int) func() {
	if dir == "" {
		return func() {}
	}
	must(os.MkdirAll(dir, 0o755))
	f, err := os.Create(filepath.Join(dir, fmt.Sprintf("%s-%d.pprof", name, i)))
	must(err)
	must(pprof.StartCPUProfile(f))
	return func() { pprof.StopCPUProfile(); f.Close() }
}

func splitComma(s string) []string {
	var out []string
	for len(s) > 0 {
		i := 0
		for i < len(s) && s[i] != ',' {
			i++
		}
		if i > 0 {
			out = append(out, s[:i])
		}
		if i == len(s) {
			break
		}
		s = s[i+1:]
	}
	return out
}

func mb(n int64) float64 { return float64(n) / 1e6 }

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fsbench:", err)
		os.Exit(1)
	}
}

// rmrf removes dir, making its folders writable first, as Go's module
// cache keeps them read-only.
func rmrf(dir string) error {
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

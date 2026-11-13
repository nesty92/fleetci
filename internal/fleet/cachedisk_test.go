package fleet

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCacheDiskValidate(t *testing.T) {
	ok := Class{Plugin: "/usr/bin/fleeting-plugin-fleetingd", MaxInstances: 2,
		PluginConfig: RawConfig(`{"vm_cache_directory": "/var/lib/fleetci/cache"}`), CacheDisk: &CacheDisk{}}
	if err := ok.validate("small"); err != nil {
		t.Fatalf("valid class: %v", err)
	}
	for want, c := range map[string]Class{
		"fleetingd": func() Class { c := ok; c.Plugin = "fleeting-plugin-aws"; return c }(),
		"vm_cache_directory": func() Class {
			c := ok
			c.PluginConfig = RawConfig(`{"vm_cache_directory": "cache"}`)
			return c
		}(),
		"a VM per job": func() Class { c := ok; c.MaxUseCount = 3; return c }(),
		"negative":     func() Class { c := ok; c.CacheDisk = &CacheDisk{Budget: -1}; return c }(),
	} {
		err := c.validate("small")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error about %s, got %v", want, err)
		}
	}
	none := ok
	none.CacheDisk, none.Plugin = nil, "fleeting-plugin-aws"
	if err := none.validate("small"); err != nil {
		t.Fatalf("class without cache_disk: %v", err)
	}
}

func TestCacheDiskConfig(t *testing.T) {
	c := CacheDisk{Budget: 2048, MinFree: 1024}.config()
	if c.Budget != 2<<30 || c.MinFree != 1<<30 || c.MaxPerCache != 0 {
		t.Fatalf("config %+v", c)
	}
}

// TestUnmountKeepsUnder: before the disk is put away, actions/cache's
// archives go, the least recently used first, until the disk holds less
// than it keeps under; then Docker's build cache and old images, only if
// it still holds more.
func TestUnmountKeepsUnder(t *testing.T) {
	dir := t.TempDir()
	disk, bin := filepath.Join(dir, "cache"), filepath.Join(dir, "bin")
	acts := filepath.Join(disk, "actions")
	for _, d := range []string{acts, bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Three 1000-byte archives, a the oldest.
	for i, n := range []string{"a", "b", "c"} {
		p := filepath.Join(acts, n+".blob")
		if err := os.WriteFile(p, make([]byte, 1000), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(acts, n+".json"), nil, 0o644)
		at := time.Now().Add(time.Duration(i-3) * time.Hour)
		_ = os.Chtimes(p, at, at)
	}
	// df's used: the archives' bytes and 100 more; docker notes what it was asked.
	fake := map[string]string{
		"df":     "#!/bin/sh\necho Used\necho $(( $(cat " + acts + "/*.blob 2>/dev/null | wc -c) + 100 ))\n",
		"docker": "#!/bin/sh\necho \"$*\" >> " + filepath.Join(dir, "docker.log") + "\n",
	}
	for n, s := range fake {
		if err := os.WriteFile(filepath.Join(bin, n), []byte(s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := unmountScript(1500)
	script = script[:strings.Index(script, "systemctl stop")]
	script = strings.ReplaceAll(script, "/cache", disk)
	cmd := exec.Command("sh", "-euc", script)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for n, want := range map[string]bool{"a": false, "b": false, "c": true} {
		_, err := os.Stat(filepath.Join(acts, n+".blob"))
		_, jerr := os.Stat(filepath.Join(acts, n+".json"))
		if (err == nil) != want || (jerr == nil) != want {
			t.Errorf("archive %s kept %v, want %v", n, err == nil, want)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "docker.log")); len(b) != 0 {
		t.Errorf("pruned Docker though the archives were enough: %s", b)
	}
	// Still over with no archives left: Docker's caches go.
	script2 := strings.ReplaceAll(strings.ReplaceAll(unmountScript(0), "/cache", disk), "systemctl", "true")
	script2 = script2[:strings.Index(script2, "sync")]
	cmd = exec.Command("sh", "-euc", script2)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	_ = os.WriteFile(filepath.Join(acts, "d.blob"), []byte("x"), 0o644)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "docker.log"))
	if !strings.Contains(string(b), "builder prune") {
		t.Errorf("docker asked %q, want a builder prune", b)
	}
}

// The cache.zip files GitLab's cache: saved or fetched in the job get the
// time it ended, after their upload, so the next job's runner finds them
// up to date. One the job didn't touch keeps its time: the bucket may
// have a newer one.
func TestUnmountDatesTheJobsCacheZips(t *testing.T) {
	dir := t.TempDir()
	disk, run := filepath.Join(dir, "cache"), filepath.Join(dir, "run")
	vol := filepath.Join(disk, "docker", "volumes", "runner-x-cache-1", "_data", "group", "proj")
	for _, d := range []string{filepath.Join(vol, "old-key"), filepath.Join(vol, "new-key"), run} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	old, saved := filepath.Join(vol, "old-key", "cache.zip"), filepath.Join(vol, "new-key", "cache.zip")
	_ = os.WriteFile(old, []byte("old"), 0o644)
	_ = os.Chtimes(old, past, past)
	time.Sleep(20 * time.Millisecond)
	_ = os.WriteFile(filepath.Join(run, "fleetci-cache-mounted"), nil, 0o644) // the disk is mounted
	time.Sleep(20 * time.Millisecond)
	_ = os.WriteFile(saved, []byte("new"), 0o644) // the job's save, archived before its upload
	_ = os.Chtimes(saved, past, past)

	script := unmountScript(1 << 40)
	script = script[strings.Index(script, "if [ -f /run"):strings.Index(script, "systemctl stop")]
	script = strings.ReplaceAll(strings.ReplaceAll(script, "/run", run), "/cache", disk)
	if out, err := exec.Command("sh", "-euc", script).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if fi, _ := os.Stat(saved); time.Since(fi.ModTime()) > time.Minute {
		t.Errorf("the job's cache.zip kept its time %v", fi.ModTime())
	}
	if fi, _ := os.Stat(old); !fi.ModTime().Equal(past) {
		t.Errorf("a cache.zip the job didn't touch was dated %v", fi.ModTime())
	}
}

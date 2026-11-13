package github

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The VM's script in a real shell: which runner it runs, and when it
// downloads one. curl is a fake that serves a runner tarball and notes
// the URL.
func TestRunScriptPicksTheRunner(t *testing.T) {
	if _, err := exec.LookPath("sort"); err != nil {
		t.Skip("no sort")
	}
	runSh := "#!/bin/sh\necho \"ran $(basename \"$PWD\") $*\"\n"

	var tgz bytes.Buffer
	zw := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(zw)
	_ = tw.WriteHeader(&tar.Header{Name: "run.sh", Mode: 0o755, Size: int64(len(runSh))})
	_, _ = tw.Write([]byte(runSh))
	_ = tw.Close()
	_ = zw.Close()

	for name, tc := range map[string]struct {
		asked    string
		image    []string // runners the image has
		ran      string
		download bool
	}{
		"newest in the image":        {image: []string{"2.9.0", "2.10.1", "2.10.0"}, ran: "actions-runner-2.10.1"},
		"none in the image":          {ran: "actions-runner-" + DefaultRunnerVersion, download: true},
		"asked for, in the image":    {asked: "2.9.0", image: []string{"2.9.0", "2.10.1"}, ran: "actions-runner-2.9.0"},
		"asked for, not the image's": {asked: "2.11.0", image: []string{"2.10.1"}, ran: "actions-runner-2.11.0", download: true},
	} {
		t.Run(name, func(t *testing.T) {
			home, bin := t.TempDir(), t.TempDir()
			for _, v := range tc.image {
				dir := filepath.Join(home, "actions-runner-"+v)
				_ = os.MkdirAll(dir, 0o755)
				_ = os.WriteFile(filepath.Join(dir, "run.sh"), []byte(runSh), 0o755)
			}
			// An unpacked one without run.sh is not a runner.
			_ = os.MkdirAll(filepath.Join(home, "actions-runner-9.9.9"), 0o755)
			_ = os.WriteFile(filepath.Join(bin, "runner.tgz"), tgz.Bytes(), 0o600)
			curl := fmt.Sprintf("#!/bin/sh\nfor a; do url=$a; done\necho \"$url\" > %s/curled\ncat %s/runner.tgz\n", bin, bin)
			_ = os.WriteFile(filepath.Join(bin, "curl"), []byte(curl), 0o755)
			// The libicu check: dnf "installs" it.
			_ = os.WriteFile(filepath.Join(bin, "dnf"), []byte("#!/bin/sh\n"), 0o755)
			_ = os.WriteFile(filepath.Join(bin, "sudo"), []byte("#!/bin/sh\nexec \"$@\"\n"), 0o755)

			cmd := exec.Command("sh", "-c", vmScript(tc.asked, CacheStore{}))
			cmd.Env = []string{"HOME=" + home, "PATH=" + bin + ":" + os.Getenv("PATH")}
			cmd.Stdin = strings.NewReader("the-jit\n")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if want := "ran " + tc.ran + " --jitconfig the-jit"; !strings.Contains(string(out), want) {
				t.Errorf("output %q, want %q", out, want)
			}
			curled, _ := os.ReadFile(filepath.Join(bin, "curled"))
			if tc.download != (len(curled) > 0) {
				t.Errorf("downloaded %q", curled)
			}
			if tc.download && !strings.Contains(string(curled), "/v"+strings.TrimPrefix(tc.ran, "actions-runner-")+"/") {
				t.Errorf("downloaded %q for %s", curled, tc.ran)
			}
		})
	}
}

func TestRunnerVersionIsChecked(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tokenFile, []byte("test-token"), 0o600)
	for _, v := range []string{"2.1'; rm -rf ~", "../2.1", "2.1 2.2"} {
		cfg := Config{Repos: []string{"o/r"}, TokenFile: tokenFile, RunnerVersion: v}
		_, err := New(t.Context(), cfg, &fakeBackend{defaultClass: "2vcpu-1gb"}, nil, failureLog(t))
		if err == nil || !strings.Contains(err.Error(), "runner_version") {
			t.Errorf("%q: %v", v, err)
		}
	}
}

// The job-started hook in a real shell, sleep a no-op: it says which run
// the job is in, prints fleetci's answer, and gives up only when fleetci
// has not begun.
func TestJobStartedHook(t *testing.T) {
	for name, tc := range map[string]struct {
		done    string // fleetci's answer, "" for none
		decided bool   // fleetci began before the hook gave up
		out     string
	}{
		"answered":     {done: "Cache disk \"main\" (shared with the project)\n", out: "Cache disk \"main\" (shared with the project)"},
		"no answer":    {out: "No cache disk: fleetci did not answer"},
		"not finished": {decided: true, out: "No cache disk: fleetci did not finish attaching it"},
	} {
		t.Run(name, func(t *testing.T) {
			d, bin := t.TempDir(), t.TempDir()
			_ = os.WriteFile(filepath.Join(bin, "sleep"), []byte("#!/bin/sh\n"), 0o755)
			if tc.done != "" {
				_ = os.WriteFile(filepath.Join(d, "done"), []byte(tc.done), 0o600)
			}
			if tc.decided {
				_ = os.Mkdir(filepath.Join(d, "decided"), 0o700)
			}
			cmd := exec.Command("sh", "-c", strings.ReplaceAll(jobStartedHook, "/run/fleetci", d))
			cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "GITHUB_REPOSITORY=o/r", "GITHUB_RUN_ID=77", "GITHUB_REF_TYPE=branch", "GITHUB_REF_PROTECTED=true"}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if strings.TrimSpace(string(out)) != tc.out {
				t.Errorf("output %q, want %q", out, tc.out)
			}
			if job, _ := os.ReadFile(filepath.Join(d, "job")); string(job) != "o/r 77 branch true\n" {
				t.Errorf("job file %q", job)
			}
			if _, err := os.Stat(filepath.Join(d, "decided")); (err == nil) != (tc.done == "") {
				t.Errorf("decided: %v", err)
			}
		})
	}
}

// The VM's zstd goes into the runner's externals, which job containers
// get at /__e, and the job-started hook puts it on the job's PATH. Without
// zstd on the VM there is neither, and no hook.
func TestRunScriptLendsZstd(t *testing.T) {
	zstd, err := exec.LookPath("zstd")
	if err != nil {
		t.Skip("no zstd")
	}
	if _, err := exec.LookPath("ldd"); err != nil {
		t.Skip("no ldd")
	}
	runSh := "#!/bin/sh\necho \"hook=$ACTIONS_RUNNER_HOOK_JOB_STARTED\"\n"
	for _, has := range []bool{true, false} {
		home, bin := t.TempDir(), t.TempDir()
		dir := filepath.Join(home, "actions-runner-2.9.0")
		_ = os.MkdirAll(filepath.Join(dir, "externals"), 0o755)
		_ = os.WriteFile(filepath.Join(dir, "run.sh"), []byte(runSh), 0o755)
		_ = os.WriteFile(filepath.Join(bin, "dnf"), []byte("#!/bin/sh\n"), 0o755)
		path := bin + ":/usr/bin:/bin"
		if has {
			_ = os.Symlink(zstd, filepath.Join(bin, "zstd"))
		} else {
			// Only the tools the script needs, so the system's zstd isn't found.
			for _, tool := range []string{"sh", "awk", "cp", "rm", "mkdir", "ldd", "chmod", "sort", "uname", "cat", "grep", "ls", "tail", "id", "cut", "printf", "sed"} {
				if p, err := exec.LookPath(tool); err == nil {
					_ = os.Symlink(p, filepath.Join(bin, tool))
				}
			}
			path = bin
		}
		cmd := exec.Command("sh", "-c", vmScript("", CacheStore{}))
		cmd.Env = []string{"HOME=" + home, "PATH=" + path}
		cmd.Stdin = strings.NewReader("the-jit\n")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("zstd %v: %v: %s", has, err, out)
		}
		zd := filepath.Join(dir, "externals", "fleetci-zstd")
		_, serr := os.Stat(filepath.Join(zd, "bin", "zstdmt"))
		if has != (serr == nil) {
			t.Fatalf("zstd %v: %v", has, serr)
		}
		hook := filepath.Join(dir, "fleetci-job-started.sh")
		if !has {
			if !strings.Contains(string(out), "hook=\n") {
				t.Errorf("a hook without zstd: %s", out)
			}
			continue
		}
		if !strings.Contains(string(out), "hook="+hook) {
			t.Fatalf("no hook: %s", out)
		}
		// The copy runs through its own loader and libraries, as in a
		// container without them.
		in := strings.Repeat("fleetci ", 1<<16)
		z := exec.Command(filepath.Join(zd, "ld.so"), "--library-path", filepath.Join(zd, "lib"), filepath.Join(zd, "zstd"), "-T0")
		z.Stdin = strings.NewReader(in)
		packed, err := z.Output()
		if err != nil || len(packed) == 0 || len(packed) >= len(in) {
			t.Fatalf("the copy: %v, %d bytes", err, len(packed))
		}
		w, _ := os.ReadFile(filepath.Join(zd, "bin", "unzstd"))
		if !strings.Contains(string(w), "/__e/fleetci-zstd/zstd -d \"$@\"") {
			t.Errorf("unzstd: %s", w)
		}
		gp := filepath.Join(t.TempDir(), "path")
		h := exec.Command("sh", hook)
		h.Env = []string{"PATH=/usr/bin:/bin", "GITHUB_PATH=" + gp}
		if out, err := h.CombinedOutput(); err != nil {
			t.Fatalf("hook: %v: %s", err, out)
		}
		if got, _ := os.ReadFile(gp); string(got) != "/__e/fleetci-zstd/bin\n" {
			t.Errorf("GITHUB_PATH %q", got)
		}
	}
}

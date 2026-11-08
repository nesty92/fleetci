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

			cmd := exec.Command("sh", "-c", fmt.Sprintf(runScript, tc.asked, "", DefaultRunnerVersion))
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

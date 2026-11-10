package fleet

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckMirror(t *testing.T) {
	for in, want := range map[string]string{
		"http://10.0.1.5:5000":   "http://10.0.1.5:5000",
		"https://mirror.gcr.io/": "https://mirror.gcr.io",
		"ftp://10.0.1.5":         "",
		"10.0.1.5:5000":          "",
		"https://x.dkr.ecr.eu-north-1.amazonaws.com/docker-hub": "",
		"http://a:5000/?x=1":      "",
		"http://u:p@a:5000":       "",
		"http://a:5000/$(reboot)": "",
	} {
		got, err := CheckMirror(in)
		if (err == nil) != (want != "") || got != want {
			t.Errorf("CheckMirror(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// TestMirrorScript runs the script with stand-ins for dockerd, sudo and
// pidof: it adds the mirror to an existing daemon.json, keeps the rest,
// writes buildx's config once, and leaves a second run's daemon.json alone.
func TestMirrorScript(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	hup := filepath.Join(dir, "hup")
	for name, body := range map[string]string{
		"dockerd": "exit 0",
		"sudo":    `[ "$1" = -n ] && shift; exec "$@"`,
		"pidof":   "echo 4242",
		"kill":    `echo "$@" >> ` + hup,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	daemon := filepath.Join(dir, "etc", "docker", "daemon.json")
	if err := os.MkdirAll(filepath.Dir(daemon), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(daemon, []byte(`{"log-driver": "local", "registry-mirrors": ["https://other"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func() {
		t.Helper()
		// Not root: the script runs kill through sudo, so the stand-in's.
		cmd := exec.Command("sh", "-c", fmt.Sprintf(mirrorScript, "http://10.0.1.5:5000"))
		cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "HOME=" + dir, "FLEETCI_DAEMON_JSON=" + daemon}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	run()
	run()

	var c map[string]any
	b, _ := os.ReadFile(daemon)
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(c["registry-mirrors"]); got != "[http://10.0.1.5:5000 https://other]" || c["log-driver"] != "local" {
		t.Errorf("daemon.json = %s", b)
	}
	if b, _ := os.ReadFile(hup); string(b) != "-HUP 4242\n" {
		t.Errorf("dockerd signalled %q, want once", b)
	}
	bk, err := os.ReadFile(filepath.Join(dir, ".docker", "buildx", "buildkitd.default.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`[registry."docker.io"]`, `mirrors = ["10.0.1.5:5000"]`, `[registry."10.0.1.5:5000"]`, `http = true`} {
		if !strings.Contains(string(bk), want) {
			t.Errorf("buildkitd.default.toml lacks %s:\n%s", want, bk)
		}
	}
}

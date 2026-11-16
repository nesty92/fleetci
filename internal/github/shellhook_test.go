package github

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunScriptShellHook: with the shell on, a job ends with the
// completed hook, which, for a job run with debug logging, asks fleetci
// for a shell and prints its answer; without debug logging it asks
// nothing. With the shell off there is no such hook.
func TestRunScriptShellHook(t *testing.T) {
	for _, shell := range []bool{false, true} {
		home, bin := t.TempDir(), t.TempDir()
		dir := filepath.Join(home, "actions-runner-2.9.0")
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\necho \"hook=$ACTIONS_RUNNER_HOOK_JOB_COMPLETED\"\n"), 0o755)
		_ = os.WriteFile(filepath.Join(bin, "dnf"), []byte("#!/bin/sh\n"), 0o755)
		cmd := exec.Command("sh", "-c", vmScript("", CacheStore{}, shell))
		cmd.Env = []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin"}
		cmd.Stdin = strings.NewReader("the-jit\n")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("shell %v: %v: %s", shell, err, out)
		}
		hook := filepath.Join(dir, "fleetci-job-completed.sh")
		if !shell {
			if !strings.Contains(string(out), "hook=\n") {
				t.Errorf("a completed hook with the shell off: %s", out)
			}
			continue
		}
		if !strings.Contains(string(out), "hook="+hook) {
			t.Fatalf("no completed hook: %s", out)
		}
		env := []string{"HOME=" + home, "PATH=/usr/bin:/bin", "GITHUB_REPOSITORY=o/r", "GITHUB_RUN_ID=70", "GITHUB_RUN_ATTEMPT=2"}
		// No debug logging: nothing asked.
		plain := exec.Command("sh", hook)
		plain.Env = env
		if out, err := plain.CombinedOutput(); err != nil || len(out) != 0 {
			t.Fatalf("hook without debug: %v: %q", err, out)
		}
		h := exec.Command("sh", hook)
		h.Env = append(env, "RUNNER_DEBUG=1")
		var hout strings.Builder
		h.Stdout, h.Stderr = &hout, &hout
		if err := h.Start(); err != nil {
			t.Fatal(err)
		}
		// fleetci's side: wait for the ask, then answer.
		wait := exec.Command("sh", "-c", shellWaitScript)
		wait.Env = env
		asked, err := wait.Output()
		if err != nil || string(asked) != "o/r 70 2\n" {
			t.Fatalf("asked %q, %v", asked, err)
		}
		ans := exec.Command("sh", "-c", shellAnswerScript)
		ans.Env = env
		ans.Stdin = strings.NewReader("tailcat ssh the-address\n")
		if out, err := ans.CombinedOutput(); err != nil {
			t.Fatalf("answer: %v: %s", err, out)
		}
		if err := h.Wait(); err != nil || hout.String() != "tailcat ssh the-address\n" {
			t.Fatalf("hook: %v: %q", err, hout.String())
		}
	}
}

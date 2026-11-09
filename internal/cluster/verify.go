package cluster

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// maxBoot is how long a VM may take to come up from its snapshot.
const maxBoot = 2 * time.Second

// verifyAll runs the microVM check on each class, in turn: the worker is
// Ready once one passes, and runs only the classes that passed.
func (w *Worker) verifyAll(ctx context.Context) {
	w.mu.Lock()
	names := slices.Sorted(maps.Keys(w.classes))
	w.mu.Unlock()
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		msg, err := w.verify(ctx, name)
		w.mu.Lock()
		if err != nil {
			w.checks[name] = "FAIL: " + err.Error()
			w.passed[name] = false
		} else {
			w.checks[name] = "ok: " + msg
			w.passed[name] = true
		}
		w.mu.Unlock()
		w.log.Info("microVM check", "class", name, "result", w.checks[name])
		w.poke()
	}
}

// Verify runs the microVM check again on every class, for fleetci verify.
func (w *Worker) Verify(ctx context.Context) map[string]string {
	w.verifyAll(ctx)
	w.mu.Lock()
	defer w.mu.Unlock()
	return maps.Clone(w.checks)
}

// verify boots one VM of the class from its snapshot and checks, over SSH,
// that Docker runs a container, that the VM reaches the forges, and for
// GitHub that actions/runner is there. Then it removes the VM.
func (w *Worker) verify(ctx context.Context, class string) (string, error) {
	w.mu.Lock()
	w.checks[class] = "checking"
	b := w.backend
	reach := w.reach
	gh := w.github
	c := w.classes[class]
	w.mu.Unlock()
	w.poke()
	if b == nil {
		return "", fmt.Errorf("the classes did not start")
	}
	// Room is taken as for a job, so the check never overbooks.
	start := time.Now()
	vm, err := w.createVM(ctx, class, true)
	if err != nil {
		return "", fmt.Errorf("booting a VM: %w", err)
	}
	boot := time.Since(start)
	defer func() { _ = w.delete(context.WithoutCancel(ctx), vm.VM) }()
	w.mu.Lock()
	conn := w.vms[vm.VM].vm.Conn
	w.mu.Unlock()
	client, _, err := conn.Client(ctx)
	if err != nil {
		return "", fmt.Errorf("ssh to the VM: %w", err)
	}
	steps := []struct{ what, cmd string }{
		{"docker runs", "docker info --format '{{.ServerVersion}}'"},
		{"docker run of a small image (the VM's DNS and egress to the registry)", "docker run --rm public.ecr.aws/docker/library/busybox:1.37 true || docker run --rm busybox true"},
	}
	for _, u := range reach {
		steps = append(steps, struct{ what, cmd string }{"reaches " + u, "curl -fsS -o /dev/null -m 15 --retry 2 " + shellQuote(u) + " || curl -sS -o /dev/null -m 15 -w '%{http_code}' " + shellQuote(u) + " | grep -qv '^000'"})
	}
	if gh {
		steps = append(steps, struct{ what, cmd string }{"actions/runner is in the image", "ls -d $HOME/actions-runner-*/bin/Runner.Listener"})
	}
	for _, s := range steps {
		out, err := run(ctx, client, s.cmd)
		if err != nil {
			return "", fmt.Errorf("%s: %v: %s", s.what, err, lastLine(out))
		}
	}
	msg := fmt.Sprintf("%d cpus %s; booted in %s; docker, egress%s", c.CPUs, c.Memory, boot.Round(10*time.Millisecond), map[bool]string{true: ", actions/runner", false: ""}[gh])
	if boot > maxBoot {
		// Slow, not broken: the job still runs, and the time says why.
		msg += fmt.Sprintf(" (slower than %s: is it booting rather than restoring its snapshot?)", maxBoot)
	}
	return msg, nil
}

func run(ctx context.Context, c *ssh.Client, cmd string) (string, error) {
	s, err := c.NewSession()
	if err != nil {
		return "", err
	}
	defer s.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.Close()
		case <-done:
		}
	}()
	out, err := s.CombinedOutput("bash -lc " + shellQuote(cmd))
	return string(out), err
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}

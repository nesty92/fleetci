package vmjob

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/nesty92/fleetci/internal/vmjob/vmjobtest"
)

type vm struct{ *vmjobtest.VM }

func newVM(t *testing.T) *vm { return &vm{vmjobtest.New(t)} }

func (v *vm) drop() { v.Drop() }

func (v *vm) client(t *testing.T) *ssh.Client {
	t.Helper()
	c, err := ssh.Dial("tcp", v.Addr, &ssh.ClientConfig{User: "t", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func (v *vm) job(key, file string) string {
	return filepath.Join(v.Home, ".fleetci", "jobs", key, file)
}

func follow(t *testing.T, c *ssh.Client, key string, from int) (int, []string, error) {
	t.Helper()
	var lines []string
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	status, next, err := Follow(ctx, c, key, from, func(l string) { lines = append(lines, l) })
	if from > 0 && next != from+len(lines) {
		t.Fatalf("from %d, next %d, got %d lines", from, next, len(lines))
	}
	return status, lines, err
}

func TestAJobRunsToItsStatusAndItsWholeLog(t *testing.T) {
	v := newVM(t)
	c := v.client(t)
	script := `read -r secret; echo "got $secret"; echo two; printf 'no newline'; exit 3`
	if err := Start(t.Context(), c, "fleetci-a-1", script, []byte("s3cret\n")); err != nil {
		t.Fatal(err)
	}
	status, lines, err := follow(t, c, "fleetci-a-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if status != 3 || !slices.Equal(lines, []string{"got s3cret", "two", "no newline"}) {
		t.Fatalf("status %d, lines %q", status, lines)
	}
	if _, err := os.Stat(v.job("fleetci-a-1", "stdin")); !os.IsNotExist(err) {
		t.Fatalf("stdin left on the vm: %v", err)
	}
	if fi, err := os.Stat(filepath.Dir(v.job("fleetci-a-1", "run"))); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("job directory: %v %v", fi.Mode(), err)
	}
}

func TestAJobOutlivesTheConnectionThatStartedIt(t *testing.T) {
	v := newVM(t)
	c := v.client(t)
	// It waits for a file the test writes only once the connection is gone.
	script := `echo started; while [ ! -e "$HOME/go" ]; do sleep 0.1; done; echo finished`
	if err := Start(t.Context(), c, "fleetci-a-2", script, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	type cut struct {
		lines []string
		next  int
	}
	got := make(chan cut, 1)
	seen := make(chan struct{}, 1)
	go func() {
		var lines []string
		_, next, _ := Follow(ctx, c, "fleetci-a-2", 1, func(l string) {
			lines = append(lines, l)
			seen <- struct{}{}
		})
		got <- cut{lines, next}
	}()
	select {
	case <-seen:
	case <-time.After(10 * time.Second):
		t.Fatal("nothing logged")
	}
	v.drop() // the network fails, and fleetci with it
	cancel()
	first := <-got
	if !slices.Equal(first.lines, []string{"started"}) || first.next != 2 {
		t.Fatalf("before the cut: %q, next %d", first.lines, first.next)
	}

	if err := os.WriteFile(filepath.Join(v.Home, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The next fleetci: a new connection, going on from what was read.
	status, rest, err := follow(t, v.client(t), "fleetci-a-2", first.next)
	if err != nil {
		t.Fatal(err)
	}
	all := append(first.lines, rest...)
	if status != 0 || !slices.Equal(all, []string{"started", "finished"}) {
		t.Fatalf("status %d, lines %q", status, all)
	}
}

// The next fleetci, which didn't read the log, asks only for what comes.
func TestFollowingFromNowSkipsTheLinesAlreadyWritten(t *testing.T) {
	v := newVM(t)
	c := v.client(t)
	script := `echo old; while [ ! -e "$HOME/go" ]; do sleep 0.1; done; echo new`
	if err := Start(t.Context(), c, "fleetci-a-4", script, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, _ := os.ReadFile(v.job("fleetci-a-4", "log")); string(b) == "old\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never logged")
		}
		time.Sleep(20 * time.Millisecond)
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(v.Home, "go"), nil, 0o600)
	}()
	_, lines, err := follow(t, c, "fleetci-a-4", 0)
	if err != nil || !slices.Equal(lines, []string{"new"}) {
		t.Fatalf("lines %q, err %v", lines, err)
	}
}

func TestFollowingAJobThatIsntThere(t *testing.T) {
	v := newVM(t)
	if _, _, err := follow(t, v.client(t), "fleetci-none", 1); !errors.Is(err, ErrNoJob) {
		t.Fatalf("err %v, want ErrNoJob", err)
	}
}

func TestAJobKilledWithoutAStatusIsLost(t *testing.T) {
	v := newVM(t)
	c := v.client(t)
	if err := Start(t.Context(), c, "fleetci-a-3", "echo up; sleep 30", nil); err != nil {
		t.Fatal(err)
	}
	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for pid == 0 && time.Now().Before(deadline) {
		b, _ := os.ReadFile(v.job("fleetci-a-3", "pid"))
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("no pid")
	}
	// The VM's kernel kills it: no shell is left to write the status.
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if _, _, err := follow(t, c, "fleetci-a-3", 1); !errors.Is(err, ErrLost) {
		t.Fatalf("err %v, want ErrLost", err)
	}
}

func TestAKeyCantLeaveTheJobsDirectory(t *testing.T) {
	v := newVM(t)
	for _, key := range []string{"../x", "a b", "a;rm", ""} {
		if err := Start(t.Context(), v.client(t), key, "true", nil); err == nil {
			t.Errorf("key %q accepted", key)
		}
	}
}

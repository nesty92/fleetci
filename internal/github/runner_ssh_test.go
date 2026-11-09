package github

import (
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"

	"github.com/nesty92/fleetci/internal/proxy"
	"github.com/nesty92/fleetci/internal/source"
	"github.com/nesty92/fleetci/internal/vmjob/vmjobtest"
)

// vmConn is a connection to vm as fleetci's: keepalives fast enough for a
// test.
func vmConn(t *testing.T, addr string) *proxy.Conn {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(nil)
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	c := proxy.NewConn(func(context.Context) (proxy.Target, error) {
		return proxy.Target{Info: provider.ConnectInfo{InternalAddr: addr, ConnectorConfig: provider.ConnectorConfig{
			Username: "u", Key: pem.EncodeToMemory(block), Timeout: time.Second,
		}}}, nil
	}, hclog.NewNullLogger())
	c.SetKeepAlive(50*time.Millisecond, 200*time.Millisecond)
	t.Cleanup(func() { c.Close() })
	return c
}

// stillUp fails unless conn is the same live connection as before.
func stillUp(t *testing.T, conn *proxy.Conn, before *ssh.Client) {
	t.Helper()
	after, lost, err := conn.Client(context.Background())
	if err != nil || after != before {
		t.Fatalf("the vm's connection was replaced or closed: %v", err)
	}
	select {
	case <-lost:
		t.Fatal("the job closed the vm's connection")
	default:
	}
	if _, _, err := after.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatalf("the job closed the vm's connection: %v", err)
	}
}

// exists waits for a file on the vm.
func exists(t *testing.T, path string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("the job ended before %s: %v", path, err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func vmFor(conn *proxy.Conn) VM { return VM{VM: source.VM{ID: "fleetci-c-1"}, Conn: conn} }

// waitRunner is a runner that has started and goes on once the test
// writes $HOME/go.
const waitRunner = `read -r jit; : > "$HOME/running"; while [ ! -e "$HOME/go" ]; do sleep 0.1; done; echo "Running job: build"`

func TestTheRunnerRunsOverTheVMsConnection(t *testing.T) {
	vm := vmjobtest.New(t)
	conn := vmConn(t, vm.Addr)
	shared, _, err := conn.Client(context.Background()) // the reclaim watch's
	if err != nil {
		t.Fatal(err)
	}

	var took atomic.Value
	start := sshStart(`read -r jit; echo "$jit" > "$HOME/jit"; echo "2026-11-05 00:49:12Z: Running job: build"`, hclog.NewNullLogger())
	if err := start(context.Background(), vmFor(conn), "jit-1", func(job string) { took.Store(job) }); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(vm.Path("jit")); strings.TrimSpace(string(b)) != "jit-1" {
		t.Errorf("runner got jit %q", b)
	}
	if took.Load() != "build" {
		t.Errorf("job taken: %v", took.Load())
	}
	if n := vm.Accepted(); n != 1 {
		t.Fatalf("%d connections to the vm, want the shared one only", n)
	}
	stillUp(t, conn, shared)
}

// fleetci stopping ends its follow of the runner, not the runner: the VM
// is then removed, or handed over to the next fleetci.
func TestCancellingAJobLeavesTheRunnerToTheVM(t *testing.T) {
	vm := vmjobtest.New(t)
	conn := vmConn(t, vm.Addr)
	shared, _, err := conn.Client(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sshStart(waitRunner, hclog.NewNullLogger())(ctx, vmFor(conn), "jit", nil) }()
	exists(t, vm.Path("running"), done)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled job kept fleetci waiting")
	}
	waitFor(t, func() bool { return vm.Sessions() == 0 })
	if err := os.WriteFile(vm.Path("go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	exit := vm.Path(".fleetci", "jobs", "fleetci-c-1", "exit")
	exists(t, exit, nil)
	stillUp(t, conn, shared)
}

func TestTheRunnerOutlivesALostConnection(t *testing.T) {
	vm := vmjobtest.New(t)
	conn := vmConn(t, vm.Addr)
	var took atomic.Value
	done := make(chan error, 1)
	go func() {
		done <- sshStart(waitRunner, hclog.NewNullLogger())(context.Background(), vmFor(conn), "jit", func(job string) { took.Store(job) })
	}()
	exists(t, vm.Path("running"), done)
	vm.Drop()
	if err := os.WriteFile(vm.Path("go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a runner whose connection went: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("never followed again")
	}
	if took.Load() != "build" || vm.Accepted() < 2 {
		t.Fatalf("job %v, %d connections", took.Load(), vm.Accepted())
	}
}

func TestAVMThatGoesMidJobIsReportedLost(t *testing.T) {
	vm := vmjobtest.New(t)
	conn := vmConn(t, vm.Addr)
	done := make(chan error, 1)
	go func() {
		done <- sshStart(waitRunner, hclog.NewNullLogger())(context.Background(), vmFor(conn), "jit", nil)
	}()
	exists(t, vm.Path("running"), done)
	vm.Gone()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "lost the vm") {
			t.Fatalf("a vm that went mid-job: %v, want lost the vm", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a job on a vm that went never ended")
	}
}

func TestARunnerThatFailsIsReported(t *testing.T) {
	vm := vmjobtest.New(t)
	err := sshStart("exit 3", hclog.NewNullLogger())(context.Background(), vmFor(vmConn(t, vm.Addr)), "jit", nil)
	if err == nil || !strings.Contains(err.Error(), "runner exited: status 3") {
		t.Fatalf("a runner that exited 3: %v", err)
	}
}

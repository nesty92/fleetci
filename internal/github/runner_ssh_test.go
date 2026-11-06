package github

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"

	"github.com/nesty92/fleetci/internal/proxy"
	"github.com/nesty92/fleetci/internal/source"
)

// runnerVM is a VM's SSH server whose exec requests run job: it gets the
// session's stdin and stdout and returns the exit status, or -1 to hang
// until the session goes.
type runnerVM struct {
	addr     string
	accepted atomic.Int32
	ended    chan struct{} // a session closed by fleetci
	mu       sync.Mutex
	conns    []net.Conn
}

func newRunnerVM(t *testing.T, job func(stdin *bufio.Reader, stdout ssh.Channel) int) *runnerVM {
	t.Helper()
	_, hostKey, _ := ed25519.GenerateKey(nil)
	signer, _ := ssh.NewSignerFromKey(hostKey)
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	v := &runnerVM{addr: ln.Addr().String(), ended: make(chan struct{}, 10)}
	t.Cleanup(func() { ln.Close(); v.drop() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			v.accepted.Add(1)
			v.mu.Lock()
			v.conns = append(v.conns, c)
			v.mu.Unlock()
			go v.serve(c, cfg, job)
		}
	}()
	return v
}

func (v *runnerVM) serve(c net.Conn, cfg *ssh.ServerConfig, job func(*bufio.Reader, ssh.Channel) int) {
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, reqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			for r := range reqs {
				if r.Type != "exec" {
					_ = r.Reply(false, nil)
					continue
				}
				_ = r.Reply(true, nil)
				go func() {
					status := job(bufio.NewReader(ch), ch)
					if status < 0 {
						// Hang: the session ends when fleetci closes it.
						for range reqs {
						}
						v.ended <- struct{}{}
						return
					}
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
					_ = ch.Close()
				}()
			}
		}()
	}
}

// drop cuts every connection, as a VM the cloud took does.
func (v *runnerVM) drop() {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, c := range v.conns {
		c.Close()
	}
}

func (v *runnerVM) conn(t *testing.T) *proxy.Conn {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(nil)
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	c := proxy.NewConn(func(context.Context) (proxy.Target, error) {
		return proxy.Target{Info: provider.ConnectInfo{InternalAddr: v.addr, ConnectorConfig: provider.ConnectorConfig{
			Username: "u", Key: pem.EncodeToMemory(block), Timeout: 5 * time.Second,
		}}}, nil
	}, hclog.NewNullLogger())
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

// started waits for the job to run on the vm, or fails with why it did not.
func started(t *testing.T, running <-chan struct{}, done <-chan error) {
	t.Helper()
	select {
	case <-running:
	case err := <-done:
		t.Fatalf("the job never ran: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the job never ran")
	}
}

func TestTheRunnerRunsOverTheVMsConnection(t *testing.T) {
	jit := make(chan string, 1)
	vm := newRunnerVM(t, func(stdin *bufio.Reader, stdout ssh.Channel) int {
		line, _ := stdin.ReadString('\n')
		jit <- strings.TrimSpace(line)
		_, _ = stdout.Write([]byte("2026-11-05 00:49:12Z: Running job: build\n"))
		return 0
	})
	conn := vm.conn(t)
	shared, _, err := conn.Client(context.Background()) // the reclaim watch's
	if err != nil {
		t.Fatal(err)
	}

	var took atomic.Value
	start := SSHStart("2.0.0", "", hclog.NewNullLogger())
	err = start(context.Background(), VM{VM: source.VM{ID: "vm-1"}, Conn: conn}, "jit-1", func(job string) { took.Store(job) })
	if err != nil {
		t.Fatal(err)
	}
	if got := <-jit; got != "jit-1" {
		t.Errorf("runner got jit %q", got)
	}
	waitFor(t, func() bool { return took.Load() == "build" })
	if n := vm.accepted.Load(); n != 1 {
		t.Fatalf("%d connections to the vm, want the shared one only", n)
	}
	stillUp(t, conn, shared)
}

func TestCancellingAJobEndsOnlyItsSession(t *testing.T) {
	running := make(chan struct{})
	vm := newRunnerVM(t, func(*bufio.Reader, ssh.Channel) int { close(running); return -1 })
	conn := vm.conn(t)
	shared, _, err := conn.Client(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- SSHStart("2.0.0", "", hclog.NewNullLogger())(ctx, VM{VM: source.VM{ID: "vm-1"}, Conn: conn}, "jit", nil)
	}()
	started(t, running, done)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled job kept running")
	}
	select {
	case <-vm.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled job's session was left open on the vm")
	}
	stillUp(t, conn, shared)
}

func TestAVMThatGoesMidJobIsReportedLost(t *testing.T) {
	running := make(chan struct{})
	vm := newRunnerVM(t, func(*bufio.Reader, ssh.Channel) int { close(running); return -1 })
	conn := vm.conn(t)

	done := make(chan error, 1)
	go func() {
		done <- SSHStart("2.0.0", "", hclog.NewNullLogger())(context.Background(), VM{VM: source.VM{ID: "vm-1"}, Conn: conn}, "jit", nil)
	}()
	started(t, running, done)
	vm.drop()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "lost the vm") {
			t.Fatalf("a vm that went mid-job: %v, want lost the vm", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a job on a vm that went never ended")
	}
}

func TestARunnerThatFailsIsReported(t *testing.T) {
	vm := newRunnerVM(t, func(*bufio.Reader, ssh.Channel) int { return 3 })
	err := SSHStart("2.0.0", "", hclog.NewNullLogger())(context.Background(), VM{VM: source.VM{ID: "vm-1"}, Conn: vm.conn(t)}, "jit", nil)
	if err == nil || !strings.Contains(err.Error(), "runner exited") {
		t.Fatalf("a runner that exited 3: %v", err)
	}
}

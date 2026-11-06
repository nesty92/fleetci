package proxy

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"
)

// runner connects to v as gitlab-runner does and opens a session, which v
// forwards to the VM. The test VM refuses every channel with Prohibited, so
// that reason is the sign the channel reached it.
func runner(t *testing.T, v *VM) error {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(nil)
	signer, _ := ssh.NewSignerFromKey(key)
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if ours, err := ln.Accept(); err == nil {
			v.Serve(context.Background(), ours, cfg)
		}
	}()
	theirs, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { theirs.Close() })

	c, chans, reqs, err := ssh.NewClientConn(theirs, "vm", &ssh.ClientConfig{
		User: "runner", HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		return err
	}
	client := ssh.NewClient(c, chans, reqs)
	t.Cleanup(func() { client.Close() })
	_, _, err = client.OpenChannel("session", nil)
	return err
}

func reachedTheVM(t *testing.T, err error) {
	t.Helper()
	var refused *ssh.OpenChannelError
	if !errors.As(err, &refused) || refused.Reason != ssh.Prohibited {
		t.Fatalf("the runner's channel did not reach the vm: %v", err)
	}
}

func TestTheRunnerUsesTheVMsConnection(t *testing.T) {
	s := newVMServer(t)
	var calls atomic.Int32
	conn := NewConn(connectTo(t, s, &calls), hclog.NewNullLogger())
	defer conn.Close()
	shared, _, err := conn.Client(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	v := NewVM("vm-1", conn, hclog.NewNullLogger())
	for range 2 {
		reachedTheVM(t, runner(t, v))
	}
	if n := s.accepted.Load(); n != 1 {
		t.Fatalf("%d connections to the vm, want the shared one only", n)
	}

	// The job ends; the machine's connection stays for whoever comes next.
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	after, lost, err := conn.Client(context.Background())
	if err != nil || after != shared || isClosed(lost) {
		t.Fatalf("closing the job's vm closed the machine's connection: %v", err)
	}
	if _, _, err := after.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatalf("closing the job's vm closed the machine's connection: %v", err)
	}
}

func TestAClosedVMTakesNoRunner(t *testing.T) {
	s := newVMServer(t)
	var calls atomic.Int32
	conn := NewConn(connectTo(t, s, &calls), hclog.NewNullLogger())
	defer conn.Close()

	v := NewVM("vm-1", conn, hclog.NewNullLogger())
	_ = v.Close()
	var refused *ssh.OpenChannelError
	if err := runner(t, v); err == nil || errors.As(err, &refused) {
		t.Fatalf("a closed vm forwarded the runner: %v", err)
	}
	if n := s.accepted.Load(); n != 0 {
		t.Fatalf("a closed vm dialed the machine: %d connections", n)
	}
}

func TestTheRunnerReachesAVMWhoseConnectionWasLost(t *testing.T) {
	s := newVMServer(t)
	var calls atomic.Int32
	conn := NewConn(connectTo(t, s, &calls), hclog.NewNullLogger())
	defer conn.Close()
	_, lost, err := conn.Client(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.drop()
	lostWithin(t, lost, "the dropped connection was never noticed")

	v := NewVM("vm-1", conn, hclog.NewNullLogger())
	reachedTheVM(t, runner(t, v))
	if n := s.accepted.Load(); n != 2 {
		t.Fatalf("%d connections, want the lost one and a new one", n)
	}
}

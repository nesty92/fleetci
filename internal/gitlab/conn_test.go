package gitlab

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"

	fleetingprovider "gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/executors"

	"github.com/nesty92/fleetci/internal/proxy"
	"github.com/nesty92/fleetci/internal/source"
)

// jobVM is a VM's SSH server. Its exec requests run cmd, which gets the
// command, stdin and stdout and returns the exit status, or -1 to hang until
// the session goes. Its direct-tcpip channels reach the address asked for.
type jobVM struct {
	addr     string
	accepted atomic.Int32
	ended    chan struct{} // a hung session closed by fleetci
	inside   string        // where the vm-only host "registry.vm" is
	mu       sync.Mutex
	conns    []net.Conn
}

type command func(cmd string, stdin *bufio.Reader, stdout io.Writer) int

func newJobVM(t *testing.T, cmd command) *jobVM {
	t.Helper()
	_, hostKey, _ := ed25519.GenerateKey(nil)
	signer, _ := ssh.NewSignerFromKey(hostKey)
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	v := &jobVM{addr: ln.Addr().String(), ended: make(chan struct{}, 10)}
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
			go v.serve(c, cfg, cmd)
		}
	}()
	return v
}

func (v *jobVM) serve(c net.Conn, cfg *ssh.ServerConfig, cmd command) {
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		switch nc.ChannelType() {
		case "direct-tcpip":
			var to struct {
				Host       string
				Port       uint32
				OriginHost string
				OriginPort uint32
			}
			_ = ssh.Unmarshal(nc.ExtraData(), &to)
			target := net.JoinHostPort(to.Host, itoa(to.Port))
			if to.Host == "registry.vm" {
				target = v.inside
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				_ = nc.Reject(ssh.ConnectionFailed, err.Error())
				continue
			}
			ch, reqs, err := nc.Accept()
			if err != nil {
				up.Close()
				continue
			}
			go ssh.DiscardRequests(reqs)
			go func() { _, _ = io.Copy(ch, up); ch.Close() }()
			go func() { _, _ = io.Copy(up, ch); up.Close() }()
		case "session":
			ch, reqs, err := nc.Accept()
			if err != nil {
				continue
			}
			go v.session(ch, reqs, cmd)
		default:
			_ = nc.Reject(ssh.UnknownChannelType, "no")
		}
	}
}

func (v *jobVM) session(ch ssh.Channel, reqs <-chan *ssh.Request, cmd command) {
	for r := range reqs {
		if r.Type != "exec" {
			_ = r.Reply(false, nil)
			continue
		}
		var exec struct{ Command string }
		_ = ssh.Unmarshal(r.Payload, &exec)
		_ = r.Reply(true, nil)
		go func() {
			status := cmd(exec.Command, bufio.NewReader(ch), ch)
			if status < 0 {
				for range reqs {
				}
				v.ended <- struct{}{}
				return
			}
			_ = ch.CloseWrite()
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
			_ = ch.Close()
		}()
	}
}

func itoa(n uint32) string {
	var b [10]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + n%10)
		if n /= 10; n == 0 {
			return string(b[i:])
		}
	}
}

// drop cuts every connection, as a VM the cloud took does.
func (v *jobVM) drop() {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, c := range v.conns {
		c.Close()
	}
}

func (v *jobVM) conn(t *testing.T) *proxy.Conn {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(nil)
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	c := proxy.NewConn(func(context.Context) (proxy.Target, error) {
		return proxy.Target{Info: fleetingprovider.ConnectInfo{ID: "i-1", InternalAddr: v.addr, ConnectorConfig: fleetingprovider.ConnectorConfig{
			Username: "u", Key: pem.EncodeToMemory(block), Timeout: 5 * time.Second,
		}}}, nil
	}, hclog.NewNullLogger())
	t.Cleanup(func() { c.Close() })
	return c
}

// prepare gives the job its client on the VM behind conn, as the Docker
// executor gets it.
func prepare(t *testing.T, conn *proxy.Conn) executors.Client {
	t.Helper()
	ref := &acquisitionRef{vm: &source.VM{ID: "vm-1", Instance: "i-1"},
		conn: func(id string) (*proxy.Conn, error) {
			if id != "vm-1" {
				return nil, errors.New("no such vm")
			}
			return conn, nil
		}}
	job := jobOf("")
	c, err := ref.Prepare(context.Background(), job.BuildLogger, job)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

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

func TestAJobRunsOverTheVMsConnection(t *testing.T) {
	vm := newJobVM(t, func(cmd string, stdin *bufio.Reader, stdout io.Writer) int {
		in, _ := io.ReadAll(stdin)
		_, _ = io.WriteString(stdout, cmd+":"+string(in))
		return 0
	})
	conn := vm.conn(t)
	shared, _, err := conn.Client(context.Background()) // the reclaim watch's
	if err != nil {
		t.Fatal(err)
	}

	c := prepare(t, conn)
	for range 2 {
		var out bytes.Buffer
		err := c.Run(context.Background(), executors.RunOptions{Command: "docker info", Stdin: strings.NewReader("in"), Stdout: &out})
		if err != nil || out.String() != "docker info:in" {
			t.Fatalf("run: %q %v", out.String(), err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if n := vm.accepted.Load(); n != 1 {
		t.Fatalf("%d connections to the vm, want the shared one only", n)
	}
	stillUp(t, conn, shared)
}

func TestACommandsExitCodeReachesTheBuild(t *testing.T) {
	vm := newJobVM(t, func(string, *bufio.Reader, io.Writer) int { return 3 })
	err := prepare(t, vm.conn(t)).Run(context.Background(), executors.RunOptions{Command: "false"})
	var build *common.BuildError
	if !errors.As(err, &build) || build.ExitCode != 3 {
		t.Fatalf("a command that exited 3: %v", err)
	}
}

func TestCancellingACommandEndsOnlyItsSession(t *testing.T) {
	running := make(chan struct{})
	vm := newJobVM(t, func(string, *bufio.Reader, io.Writer) int { close(running); return -1 })
	conn := vm.conn(t)
	c := prepare(t, conn)
	shared, _, err := conn.Client(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, executors.RunOptions{Command: "sleep"}) }()
	started(t, running, done)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled command: %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled command kept running")
	}
	select {
	case <-vm.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled command's session was left open on the vm")
	}
	stillUp(t, conn, shared)
}

func TestACommandOnAVMThatGoesSaysSo(t *testing.T) {
	running := make(chan struct{})
	vm := newJobVM(t, func(string, *bufio.Reader, io.Writer) int { close(running); return -1 })
	c := prepare(t, vm.conn(t))

	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background(), executors.RunOptions{Command: "sleep"}) }()
	started(t, running, done)
	vm.drop()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "lost the vm") {
			t.Fatalf("a command on a vm that went: %v, want lost the vm", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a command on a vm that went never ended")
	}
}

func TestDialRunIsAStreamToTheCommand(t *testing.T) {
	vm := newJobVM(t, func(cmd string, stdin *bufio.Reader, stdout io.Writer) int {
		line, _ := stdin.ReadString('\n')
		_, _ = io.WriteString(stdout, cmd+" got "+line)
		return 0
	})
	s, err := prepare(t, vm.conn(t)).DialRun(context.Background(), "docker system dial-stdio")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := io.WriteString(s, "ping\n"); err != nil {
		t.Fatal(err)
	}
	got, _ := bufio.NewReader(s).ReadString('\n')
	if got != "docker system dial-stdio got ping\n" {
		t.Fatalf("over DialRun: %q", got)
	}
}

func TestDialReachesAnAddressFromTheVM(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			_, _ = io.WriteString(c, "hello from the vm's side\n")
			c.Close()
		}
	}()
	vm := newJobVM(t, func(string, *bufio.Reader, io.Writer) int { return 0 })
	vm.inside = ln.Addr().String()
	conn := vm.conn(t)

	// Only the vm knows the name.
	s, err := prepare(t, conn).Dial("tcp", "registry.vm:5000")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, _ := bufio.NewReader(s).ReadString('\n')
	if got != "hello from the vm's side\n" {
		t.Fatalf("over Dial: %q", got)
	}
	if n := vm.accepted.Load(); n != 1 {
		t.Fatalf("%d connections to the vm, want one", n)
	}
}

// started waits for the command to run on the vm, or fails with why it did
// not.
func started(t *testing.T, running <-chan struct{}, done <-chan error) {
	t.Helper()
	select {
	case <-running:
	case err := <-done:
		t.Fatalf("the command never ran: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the command never ran")
	}
}

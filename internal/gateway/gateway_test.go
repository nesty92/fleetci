package gateway_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"

	"github.com/nesty92/fleetci/internal/gateway"
	"github.com/nesty92/fleetci/internal/proxy"
	"github.com/nesty92/fleetci/internal/sshx"
)

type vmMap map[string]*proxy.VM

func (m vmMap) Get(id string) (*proxy.VM, bool) { v, ok := m[id]; return v, ok }

// TestRunnerPath walks the path gitlab-runner takes: gateway with an issued
// key, then a VM connection with a placeholder password, then a command and
// a Unix socket stream on the VM.
func TestRunnerPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := hclog.NewNullLogger()

	nestingSock := echoUnix(t, filepath.Join(t.TempDir(), "nesting.sock"))
	vmAddr, vmKey, vmHostKey := fakeVM(t)

	target := proxy.Target{
		Info: provider.ConnectInfo{
			ConnectorConfig: provider.ConnectorConfig{Username: "fleetci", Key: vmKey, Timeout: 5 * time.Second},
			InternalAddr:    vmAddr,
		},
		HostKey: vmHostKey,
	}
	conn := proxy.NewConn(func(context.Context) (proxy.Target, error) { return target, nil }, log)
	defer conn.Close()
	vm := proxy.NewVM("fleetci-vm-1", conn, log)
	defer vm.Close()

	gw, err := gateway.New(gateway.Config{
		Listen:        "127.0.0.1:0",
		NestingPath:   "/fleetci/nesting.sock",
		NestingSocket: nestingSock,
		VMs:           vmMap{"fleetci-vm-1": vm},
		Log:           log,
	})
	if err != nil {
		t.Fatal(err)
	}
	go gw.Serve(ctx)

	// A key the gateway did not issue is refused.
	_, stranger, _ := sshx.NewKey()
	if _, err := dialKey(gw.Addr(), stranger); err == nil {
		t.Fatal("gateway accepted a key it did not issue")
	}

	pemKey, err := gw.IssueKey(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := ssh.ParsePrivateKey(pemKey)
	hop1, err := dialKey(gw.Addr(), signer)
	if err != nil {
		t.Fatal(err)
	}
	defer hop1.Close()

	// The runner's health check passes; any other command does not.
	if err := run(hop1, "exit 0"); err != nil {
		t.Fatalf("health check: %v", err)
	}
	if err := run(hop1, "id"); err == nil {
		t.Fatal("gateway ran an arbitrary command")
	}

	// Only the nesting socket is reachable.
	expectEcho(t, hop1, "unix", "/fleetci/nesting.sock")
	if _, err := hop1.Dial("unix", "/var/run/docker.sock"); err == nil {
		t.Fatal("gateway opened a socket other than nesting")
	}
	if _, err := hop1.Dial("tcp", "fleetci-vm-2:22"); err == nil {
		t.Fatal("gateway opened an unknown vm")
	}

	// Hop 2: the runner's VM connection, with the placeholder password.
	raw, err := hop1.Dial("tcp", "fleetci-vm-1:22")
	if err != nil {
		t.Fatal(err)
	}
	c, chans, reqs, err := ssh.NewClientConn(raw, "fleetci-vm-1:22", &ssh.ClientConfig{
		User:            "fleetci",
		Auth:            []ssh.AuthMethod{ssh.Password("unused")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	hop2 := ssh.NewClient(c, chans, reqs)
	defer hop2.Close()

	// A command on the VM keeps its output and exit code, as
	// `docker system dial-stdio` needs.
	sess, err := hop2.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	out, err := sess.Output("hello")
	var exitErr *ssh.ExitError
	if string(out) != "vm says hello\n" || !errors.As(err, &exitErr) || exitErr.ExitStatus() != 3 {
		t.Fatalf("command on vm: out=%q err=%v", out, err)
	}

	// A Unix socket on the VM, as the Docker socket is reached.
	expectEcho(t, hop2, "unix", "/var/run/docker.sock")
}

func dialKey(addr string, signer ssh.Signer) (*ssh.Client, error) {
	return ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            gateway.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
}

func run(c *ssh.Client, cmd string) error {
	s, err := c.NewSession()
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Run(cmd)
}

func expectEcho(t *testing.T, c *ssh.Client, network, addr string) {
	t.Helper()
	conn, err := c.Dial(network, addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo through %s: %q %v", addr, buf, err)
	}
}

func echoUnix(t *testing.T, path string) string {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return path
}

// fakeVM is an SSH server standing in for a job VM. It accepts one key, runs
// a canned command, and echoes on any Unix socket stream.
func fakeVM(t *testing.T) (addr string, clientKey []byte, hostKey ssh.PublicKey) {
	t.Helper()
	clientKey, clientSigner, _ := sshx.NewKey()
	_, hostSigner, _ := sshx.NewKey()

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if string(k.Marshal()) != string(clientSigner.PublicKey().Marshal()) {
				return nil, errors.New("wrong key")
			}
			return nil, nil
		},
	}
	cfg.AddHostKey(hostSigner)
	// A second host key type, as real sshd has: the proxy must still ask
	// for the one it pinned.
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecSigner, _ := ssh.NewSignerFromKey(ecKey)
	cfg.AddHostKey(ecSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveFakeVM(conn, cfg)
		}
	}()
	return ln.Addr().String(), clientKey, hostSigner.PublicKey()
}

func serveFakeVM(conn net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		switch nc.ChannelType() {
		case "session":
			ch, creqs, _ := nc.Accept()
			go func() {
				for r := range creqs {
					var p struct{ Command string }
					_ = ssh.Unmarshal(r.Payload, &p)
					_ = r.Reply(r.Type == "exec", nil)
					if r.Type == "exec" {
						_, _ = io.WriteString(ch, "vm says "+strings.TrimSpace(p.Command)+"\n")
						_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ S uint32 }{3}))
						_ = ch.Close()
						return
					}
				}
			}()
		case "direct-streamlocal@openssh.com":
			ch, creqs, _ := nc.Accept()
			go ssh.DiscardRequests(creqs)
			go func() { defer ch.Close(); _, _ = io.Copy(ch, ch) }()
		default:
			_ = nc.Reject(ssh.UnknownChannelType, "no")
		}
	}
}

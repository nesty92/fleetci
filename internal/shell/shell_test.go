package shell

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"
)

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// fakeVM is an SSH server like a VM's sshd: an exec prints what it was
// asked, a line on stderr, and exits 3; a direct-tcpip dials its target.
func fakeVM(t *testing.T) *ssh.Client {
	t.Helper()
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(newSigner(t))
	vmEnd, fleetciEnd := connPair(t)
	go func() {
		_, chans, reqs, err := ssh.NewServerConn(vmEnd, cfg)
		if err != nil {
			return
		}
		go ssh.DiscardRequests(reqs)
		for nc := range chans {
			switch nc.ChannelType() {
			case "session":
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
						cmd := string(r.Payload[4:])
						_, _ = io.WriteString(ch, "ran: "+cmd+"\n")
						_, _ = io.WriteString(ch.Stderr(), "on stderr\n")
						_, _ = ch.SendRequest("exit-status", false, binary.BigEndian.AppendUint32(nil, 3))
						_ = ch.Close()
					}
				}()
			case "direct-tcpip":
				var p struct {
					Host       string
					Port       uint32
					OriginHost string
					OriginPort uint32
				}
				if err := ssh.Unmarshal(nc.ExtraData(), &p); err != nil {
					_ = nc.Reject(ssh.ConnectionFailed, err.Error())
					continue
				}
				c, err := net.Dial("tcp", net.JoinHostPort(p.Host, itoa(p.Port)))
				if err != nil {
					_ = nc.Reject(ssh.ConnectionFailed, err.Error())
					continue
				}
				ch, reqs, err := nc.Accept()
				if err != nil {
					c.Close()
					continue
				}
				go ssh.DiscardRequests(reqs)
				go func() { _, _ = io.Copy(ch, c); _ = ch.Close() }()
				go func() { _, _ = io.Copy(c, ch); _ = c.Close() }()
			default:
				_ = nc.Reject(ssh.UnknownChannelType, "no")
			}
		}
	}()
	cc, chans, reqs, err := ssh.NewClientConn(fleetciEnd, "vm", &ssh.ClientConfig{
		User:            "runner",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	client := ssh.NewClient(cc, chans, reqs)
	t.Cleanup(func() { client.Close() })
	return client
}

// connPair is two ends of a loopback TCP connection: unlike net.Pipe's,
// writes are buffered, as SSH's version exchange needs.
func connPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	a, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

func itoa(n uint32) string {
	var b [10]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			return string(b[i:])
		}
	}
}

// proxied connects a client with key through Proxy to vm.
func proxied(t *testing.T, vm *ssh.Client, allowed []ssh.PublicKey, key ssh.Signer) (*ssh.Client, error) {
	t.Helper()
	cfg, err := ServerConfig(allowed)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	userEnd, proxyEnd := connPair(t)
	go Proxy(ctx, proxyEnd, cfg, func(context.Context) (*ssh.Client, error) { return vm, nil }, hclog.NewNullLogger())
	cc, chans, reqs, err := ssh.NewClientConn(userEnd, "tailcat", &ssh.ClientConfig{
		User:            "anyone",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(key)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		userEnd.Close()
		return nil, err
	}
	c := ssh.NewClient(cc, chans, reqs)
	t.Cleanup(func() { c.Close() })
	return c, nil
}

// TestProxySession: a command run through the proxy runs on the VM, and
// its output, stderr and exit status come back whole.
func TestProxySession(t *testing.T) {
	vm := fakeVM(t)
	key := newSigner(t)
	c, err := proxied(t, vm, []ssh.PublicKey{key.PublicKey()}, key)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	s.Stdout, s.Stderr = &out, &errOut
	err = s.Run("uname -a")
	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 3 {
		t.Fatalf("Run: %v, want exit status 3", err)
	}
	if out.String() != "ran: uname -a\n" || errOut.String() != "on stderr\n" {
		t.Fatalf("stdout %q, stderr %q", out.String(), errOut.String())
	}
}

// TestProxyForward: ssh -L reaches a port on the VM's side.
func TestProxyForward(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = io.WriteString(c, "from the vm's port\n")
		c.Close()
	}()
	vm := fakeVM(t)
	key := newSigner(t)
	c, err := proxied(t, vm, []ssh.PublicKey{key.PublicKey()}, key)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := c.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(conn)
	if err != nil || string(b) != "from the vm's port\n" {
		t.Fatalf("read %q, %v", b, err)
	}
}

// TestProxyRefusesOtherKeys: only the keys given log in.
func TestProxyRefusesOtherKeys(t *testing.T) {
	vm := fakeVM(t)
	allowed := newSigner(t)
	if _, err := proxied(t, vm, []ssh.PublicKey{allowed.PublicKey()}, newSigner(t)); err == nil {
		t.Fatal("a key not given logged in")
	}
}

func TestParseKeys(t *testing.T) {
	k := string(ssh.MarshalAuthorizedKey(newSigner(t).PublicKey()))
	keys, err := ParseKeys("# a comment\n\n" + k + k)
	if err != nil || len(keys) != 2 {
		t.Fatalf("ParseKeys: %d keys, %v", len(keys), err)
	}
	if _, err := ParseKeys(""); err == nil {
		t.Fatal("no keys: want an error")
	}
	if _, err := ParseKeys(`command="x" ` + k); err == nil || !strings.Contains(err.Error(), "options") {
		t.Fatalf("a key with options: %v", err)
	}
}

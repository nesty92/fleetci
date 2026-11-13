// Package vmjobtest is an SSH server that runs each exec request with sh,
// as a VM's sshd does, for tests of jobs run with vmjob.
package vmjobtest

import (
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"golang.org/x/crypto/ssh"
)

// VM is the server. Its sessions run in Home, each in a process group of
// its own that is killed when its client goes, as sshd ends a session.
type VM struct {
	Home string
	Addr string

	accepted atomic.Int32
	open     atomic.Int32
	ln       net.Listener
	mu       sync.Mutex
	conns    []net.Conn
}

// New starts a VM that accepts any client; the test's cleanup stops it.
func New(t *testing.T) *VM {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(nil)
	signer, _ := ssh.NewSignerFromKey(key)
	cfg := &ssh.ServerConfig{
		NoClientAuth:      true,
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil },
	}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	v := &VM{Home: t.TempDir(), Addr: ln.Addr().String(), ln: ln}
	t.Cleanup(func() { ln.Close(); v.Drop() })
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
			go v.serve(c, cfg)
		}
	}()
	return v
}

// Accepted is how many connections the VM has taken.
func (v *VM) Accepted() int { return int(v.accepted.Load()) }

// Sessions is how many commands run now.
func (v *VM) Sessions() int { return int(v.open.Load()) }

// Drop cuts every connection, as a network that fails does.
func (v *VM) Drop() {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, c := range v.conns {
		c.Close()
	}
	v.conns = nil
}

// Gone cuts every connection and takes no more, as a VM the cloud took.
// Its jobs' processes go on, as the test's.
func (v *VM) Gone() {
	v.ln.Close()
	v.Drop()
}

// Path is a file in the VM's home.
func (v *VM) Path(elem ...string) string { return filepath.Join(append([]string{v.Home}, elem...)...) }

func (v *VM) serve(c net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go v.forwards(sc, reqs)
	for nc := range chans {
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go v.session(ch, creqs)
	}
}

// forwards serves the client's remote forwards ("ssh -R"), as sshd does:
// a port on the VM's loopback whose connections come to the client.
func (v *VM) forwards(sc *ssh.ServerConn, reqs <-chan *ssh.Request) {
	var lns []net.Listener
	defer func() {
		for _, ln := range lns {
			ln.Close()
		}
	}()
	for r := range reqs {
		if r.Type != "tcpip-forward" {
			if r.WantReply {
				_ = r.Reply(false, nil)
			}
			continue
		}
		var at struct {
			Addr string
			Port uint32
		}
		if ssh.Unmarshal(r.Payload, &at) != nil {
			_ = r.Reply(false, nil)
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(at.Addr, strconv.Itoa(int(at.Port))))
		if err != nil {
			_ = r.Reply(false, nil)
			continue
		}
		lns = append(lns, ln)
		_ = r.Reply(true, nil)
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer c.Close()
					from := c.RemoteAddr().(*net.TCPAddr)
					ch, creqs, err := sc.OpenChannel("forwarded-tcpip", ssh.Marshal(struct {
						Addr     string
						Port     uint32
						OrigAddr string
						OrigPort uint32
					}{at.Addr, at.Port, from.IP.String(), uint32(from.Port)}))
					if err != nil {
						return
					}
					go ssh.DiscardRequests(creqs)
					defer ch.Close()
					go func() { _, _ = io.Copy(ch, c); _ = ch.CloseWrite() }()
					_, _ = io.Copy(c, ch)
				}()
			}
		}()
	}
}

func (v *VM) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for r := range reqs {
		if r.Type != "exec" || len(r.Payload) < 4 {
			_ = r.Reply(false, nil)
			continue
		}
		n := binary.BigEndian.Uint32(r.Payload)
		if int(n) > len(r.Payload)-4 {
			_ = r.Reply(false, nil)
			continue
		}
		cmd := exec.Command("sh", "-c", string(r.Payload[4:4+n]))
		cmd.Env = append(os.Environ(), "HOME="+v.Home)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = ch, ch, ch.Stderr()
		_ = r.Reply(true, nil)
		// The session's process group, as sshd gives each session, so the
		// end of the session can end it.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		status := 255
		if err := cmd.Start(); err == nil {
			v.open.Add(1)
			done := make(chan struct{})
			gone := make(chan struct{})
			go func() {
				// The requests end once the client is gone.
				for range reqs {
				}
				close(gone)
			}()
			go func() {
				select {
				case <-done:
				case <-gone:
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				}
			}()
			err = cmd.Wait()
			close(done)
			v.open.Add(-1)
			status = 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				status = ee.ExitCode()
			}
		}
		_ = ch.CloseWrite()
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
		return
	}
}

// Package gateway is the SSH server gitlab-runner connects to as its fleeting
// instance. It runs inside fleetci's process on loopback and allows three
// things only:
//
//   - a stream to the nesting socket, for Create and Delete;
//   - a stream to a job VM by its id, which the proxy ends and carries on;
//   - the runner's health check, the command "exit 0".
//
// It accepts only keys it issued itself, each for a short time.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"

	"github.com/nesty92/fleetci/internal/proxy"
	"github.com/nesty92/fleetci/internal/sshx"
)

// User is the only login name the gateway accepts.
const User = "fleetci"

// VMs finds a job VM by the id the nesting API returned.
type VMs interface {
	Get(id string) (*proxy.VM, bool)
}

type Config struct {
	// Listen is the loopback address to serve on, such as 127.0.0.1:0.
	Listen string
	// NestingPath is the socket path the runner asks for (nesting_host).
	NestingPath string
	// NestingSocket is the real, private socket the nesting API serves on.
	NestingSocket string
	VMs           VMs
	Log           hclog.Logger
}

type Server struct {
	cfg   Config
	ln    net.Listener
	keys  keyRing
	ssh   *ssh.ServerConfig
	vmSSH *ssh.ServerConfig
	wg    sync.WaitGroup
}

func New(cfg Config) (*Server, error) {
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, err
	}
	if !ln.Addr().(*net.TCPAddr).IP.IsLoopback() {
		_ = ln.Close()
		return nil, fmt.Errorf("gateway must listen on loopback, not %s", ln.Addr())
	}

	_, hostKey, err := sshx.NewKey()
	if err != nil {
		_ = ln.Close()
		return nil, err
	}

	s := &Server{cfg: cfg, ln: ln, keys: keyRing{keys: map[string]time.Time{}}}

	s.ssh = &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if meta.User() != User || !s.keys.valid(key) {
				return nil, errors.New("key not issued by fleetci")
			}
			return nil, nil
		},
	}
	s.ssh.AddHostKey(hostKey)

	// The runner's connection to a VM arrives inside an authenticated
	// gateway connection and never touches a port, so its password is a
	// placeholder.
	s.vmSSH = &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, nil },
	}
	s.vmSSH.AddHostKey(hostKey)

	return s, nil
}

// Addr is the host:port the runner connects to.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// IssueKey returns a new private key that can open connections for ttl.
// Connections opened with it stay up after it expires.
func (s *Server) IssueKey(ttl time.Duration) ([]byte, error) {
	pemKey, signer, err := sshx.NewKey()
	if err != nil {
		return nil, err
	}
	s.keys.add(signer.PublicKey(), time.Now().Add(ttl))
	return pemKey, nil
}

// Serve accepts connections until ctx ends.
func (s *Server) Serve(ctx context.Context) {
	go func() { <-ctx.Done(); _ = s.ln.Close() }()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			s.wg.Wait()
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(ctx, conn)
		}()
	}
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	sc, chans, reqs, err := ssh.NewServerConn(conn, s.ssh)
	if err != nil {
		s.cfg.Log.Debug("gateway handshake failed", "err", err)
		return
	}
	defer sc.Close()
	go sshx.DiscardGlobal(reqs)
	go func() { <-ctx.Done(); _ = sc.Close() }()

	for nc := range chans {
		switch nc.ChannelType() {
		case "session":
			go s.session(nc)
		case "direct-streamlocal@openssh.com":
			go s.streamLocal(nc)
		case "direct-tcpip":
			go s.toVM(ctx, nc)
		default:
			_ = nc.Reject(ssh.UnknownChannelType, "not allowed")
		}
	}
}

// session runs the runner's health check and nothing else.
func (s *Server) session(nc ssh.NewChannel) {
	ch, reqs, err := nc.Accept()
	if err != nil {
		return
	}
	defer ch.Close()

	for req := range reqs {
		if req.Type != "exec" {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}

		var exec struct{ Command string }
		status := uint32(0)
		if err := ssh.Unmarshal(req.Payload, &exec); err != nil || exec.Command != "exit 0" {
			status = 126
			_, _ = fmt.Fprintln(ch.Stderr(), "fleetci: commands do not run on the gateway")
		}
		if req.WantReply {
			_ = req.Reply(true, nil)
		}
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
		return
	}
}

func (s *Server) streamLocal(nc ssh.NewChannel) {
	var req struct {
		SocketPath string
		Reserved0  string
		Reserved1  uint32
	}
	if err := ssh.Unmarshal(nc.ExtraData(), &req); err != nil || req.SocketPath != s.cfg.NestingPath {
		_ = nc.Reject(ssh.Prohibited, "only the nesting socket is reachable")
		return
	}

	conn, err := net.Dial("unix", s.cfg.NestingSocket)
	if err != nil {
		_ = nc.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		_ = conn.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	sshx.Pipe(ch, conn)
}

func (s *Server) toVM(ctx context.Context, nc ssh.NewChannel) {
	var req struct {
		Host       string
		Port       uint32
		OriginHost string
		OriginPort uint32
	}
	if err := ssh.Unmarshal(nc.ExtraData(), &req); err != nil {
		_ = nc.Reject(ssh.Prohibited, "bad request")
		return
	}
	vm, ok := s.cfg.VMs.Get(req.Host)
	if !ok {
		_ = nc.Reject(ssh.Prohibited, "unknown vm")
		return
	}

	ch, reqs, err := nc.Accept()
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	vm.Serve(ctx, sshx.Conn{Channel: ch}, s.vmSSH)
}

type keyRing struct {
	mu   sync.Mutex
	keys map[string]time.Time
}

func (r *keyRing) add(key ssh.PublicKey, until time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for k, exp := range r.keys {
		if now.After(exp) {
			delete(r.keys, k)
		}
	}
	r.keys[string(key.Marshal())] = until
}

func (r *keyRing) valid(key ssh.PublicKey) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	exp, ok := r.keys[string(key.Marshal())]
	return ok && time.Now().Before(exp)
}

// Package shell lets a person into a job's VM over SSH, to debug it.
//
// The VM is reached at a tailcat address (github.com/tailscale/tailcat):
// WireGuard to fleetci, through a DERP relay or, after hole punching,
// directly, with nothing to open on the network. fleetci answers SSH there
// itself, takes only the keys it was given, and passes each session, and
// each port forward the client asks for, to the VM over fleetci's own
// connection to it. The VM's sshd and its keys stay as they are, and
// nothing the job left on the VM can let anyone else in.
//
// A tailcat address is a WireGuard key: it identifies fleetci to the
// client, so a client needs no host key (tailcat ssh checks none). It
// travels in job logs, which may be public: the SSH keys are what keep
// others out.
package shell

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/tailscale/tailcat"
	"golang.org/x/crypto/ssh"
)

// handshakeTimeout is how long a client has to get in.
var handshakeTimeout = 30 * time.Second

// Options are what Start serves.
type Options struct {
	// Keys are the SSH public keys that may log in, in authorized_keys
	// form, one per line.
	Keys string
	// Who is whose keys they are, for the log.
	Who string
	// VM is fleetci's connection to the VM, which sessions are opened on.
	VM func(ctx context.Context) (*ssh.Client, error)
	// DERPMapURL is where tailcat finds its relays; empty, tailcat.dev's.
	DERPMapURL string
	Log        hclog.Logger
}

// Server is a VM served at a tailcat address until Close.
type Server struct {
	ts     *tailcat.Server
	ln     net.Listener
	addr   string
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
	err    error
}

// ParseKeys reads authorized_keys lines, skipping blanks and comments.
// Keys with options are refused: they would mean restrictions the proxy
// doesn't keep.
func ParseKeys(text string) ([]ssh.PublicKey, error) {
	var keys []ssh.PublicKey
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, _, opts, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("key on line %d: %w", i+1, err)
		}
		if len(opts) > 0 {
			return nil, fmt.Errorf("key on line %d: options are not supported", i+1)
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil, errors.New("no SSH public keys")
	}
	return keys, nil
}

// ServerConfig is SSH as Proxy answers it: only keys, with a host key
// made for this server alone.
func ServerConfig(keys []ssh.PublicKey) (*ssh.ServerConfig, error) {
	allowed := make(map[string]bool, len(keys))
	for _, k := range keys {
		allowed[string(k.Marshal())] = true
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if allowed[string(k.Marshal())] {
				return &ssh.Permissions{Extensions: map[string]string{"key": ssh.FingerprintSHA256(k)}}, nil
			}
			return nil, errors.New("key not allowed")
		},
		ServerVersion: "SSH-2.0-fleetci",
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	cfg.AddHostKey(signer)
	return cfg, nil
}

// Start serves the VM at a new tailcat address, until ctx ends or Close.
func Start(ctx context.Context, o Options) (*Server, error) {
	keys, err := ParseKeys(o.Keys)
	if err != nil {
		return nil, err
	}
	cfg, err := ServerConfig(keys)
	if err != nil {
		return nil, err
	}
	log := o.Log
	if log == nil {
		log = hclog.NewNullLogger()
	}
	tlog := log.Named("tailcat")
	ts := &tailcat.Server{
		DERPMapURL: o.DERPMapURL,
		Logf:       func(f string, a ...any) { tlog.Trace(fmt.Sprintf(f, a...)) },
	}
	ln, err := ts.Listen(ctx, "tcp", ":22")
	if err != nil {
		_ = ts.Close()
		return nil, fmt.Errorf("tailcat: %w", err)
	}
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &Server{ts: ts, ln: ln, addr: string(ts.TailcatAddr()), cancel: cancel}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				Proxy(sctx, c, cfg, o.VM, log.With("for", o.Who))
			}()
		}
	}()
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()
	return s, nil
}

// Addr is the server's tailcat address.
func (s *Server) Addr() string { return s.addr }

// Close ends the server and every session through it.
func (s *Server) Close() error {
	s.once.Do(func() {
		s.cancel()
		s.err = s.ln.Close()
		if err := s.ts.Close(); s.err == nil {
			s.err = err
		}
	})
	return s.err
}

// Proxy answers SSH on c, as cfg says, and passes the client's sessions
// and port forwards (ssh -L) to the VM, until either end goes or ctx ends.
// Forwards from the VM back to the client (ssh -R) are refused.
func Proxy(ctx context.Context, c net.Conn, cfg *ssh.ServerConfig, vm func(context.Context) (*ssh.Client, error), log hclog.Logger) {
	defer c.Close()
	// A client that doesn't get in within handshakeTimeout is let go, so
	// nobody holds connections open without a key.
	_ = c.SetDeadline(time.Now().Add(handshakeTimeout))
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	_ = c.SetDeadline(time.Time{})
	if err != nil {
		log.Info("shell: refused a connection", "err", err)
		return
	}
	defer sc.Close()
	key := sc.Permissions.Extensions["key"]
	log.Info("shell: connected", "key", key)
	defer log.Info("shell: disconnected", "key", key)
	go ssh.DiscardRequests(reqs)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = sc.Close()
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	for nc := range chans {
		switch nc.ChannelType() {
		case "session", "direct-tcpip":
		default:
			_ = nc.Reject(ssh.UnknownChannelType, "not supported")
			continue
		}
		client, err := vm(ctx)
		if err != nil {
			_ = nc.Reject(ssh.ConnectionFailed, "the vm is gone")
			continue
		}
		to, toReqs, err := client.OpenChannel(nc.ChannelType(), nc.ExtraData())
		if err != nil {
			var oc *ssh.OpenChannelError
			if errors.As(err, &oc) {
				_ = nc.Reject(oc.Reason, oc.Message)
			} else {
				_ = nc.Reject(ssh.ConnectionFailed, err.Error())
			}
			continue
		}
		from, fromReqs, err := nc.Accept()
		if err != nil {
			_ = to.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			pipe(from, fromReqs, to, toReqs)
		}()
	}
}

// pipe joins two channels: data, stderr, requests both ways (a pty, a
// window's size, the exit status), and each end's EOF and close.
func pipe(a ssh.Channel, aReqs <-chan *ssh.Request, b ssh.Channel, bReqs <-chan *ssh.Request) {
	toA, toB := copyAll(a, b), copyAll(b, a)
	aGone, bGone := make(chan struct{}), make(chan struct{})
	go func() { forward(b, aReqs); close(aGone) }()
	go func() { forward(a, bReqs); close(bGone) }()
	// The first end to close takes the other with it: a VM's session that
	// ends, once all it wrote has reached the client, or a client that
	// hangs up.
	select {
	case <-bGone:
		<-toA
	case <-aGone:
	}
	_ = a.Close()
	_ = b.Close()
	<-aGone
	<-bGone
	<-toA
	<-toB
}

// copyAll copies src's data and stderr to dst, then ends dst's writing;
// the channel it returns closes then.
func copyAll(dst, src ssh.Channel) <-chan struct{} {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(dst, src) }()
	go func() { defer wg.Done(); _, _ = io.Copy(dst.Stderr(), src.Stderr()) }()
	go func() {
		wg.Wait()
		_ = dst.CloseWrite()
		close(done)
	}()
	return done
}

// forward passes requests to dst, and dst's answers back, until reqs ends.
func forward(dst ssh.Channel, reqs <-chan *ssh.Request) {
	for r := range reqs {
		ok, err := dst.SendRequest(r.Type, r.WantReply, r.Payload)
		if r.WantReply {
			_ = r.Reply(ok && err == nil, nil)
		}
	}
}

// Package sshx holds small SSH helpers shared by the gateway and the proxy.
package sshx

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Conn adapts an SSH channel to net.Conn, so an SSH server can run on top of
// a channel of another SSH connection.
type Conn struct {
	ssh.Channel
}

var zeroAddr = &net.TCPAddr{IP: net.IPv4zero}

func (Conn) LocalAddr() net.Addr              { return zeroAddr }
func (Conn) RemoteAddr() net.Addr             { return zeroAddr }
func (Conn) SetDeadline(time.Time) error      { return nil }
func (Conn) SetReadDeadline(time.Time) error  { return nil }
func (Conn) SetWriteDeadline(time.Time) error { return nil }

// Pipe copies a channel and a plain connection into each other until both
// directions end, then closes both.
func Pipe(ch ssh.Channel, conn net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(conn, ch)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = conn.Close()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(ch, conn)
		_ = ch.CloseWrite()
	}()
	wg.Wait()
	_ = ch.Close()
	_ = conn.Close()
}

// ForwardChannel opens the same channel type on upstream and joins the two:
// data, stderr and channel requests (exec, env, exit-status, signals) flow
// both ways. The runner's channels are forwarded as they are, so a command
// session, a Unix socket stream and a TCP stream all work the same way.
func ForwardChannel(nc ssh.NewChannel, upstream ssh.Conn) {
	upCh, upReqs, err := upstream.OpenChannel(nc.ChannelType(), nc.ExtraData())
	if err != nil {
		var oe *ssh.OpenChannelError
		if errors.As(err, &oe) {
			_ = nc.Reject(oe.Reason, oe.Message)
		} else {
			_ = nc.Reject(ssh.ConnectionFailed, err.Error())
		}
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		_ = upCh.Close()
		return
	}

	// Requests from the runner go up. A request holds replying until the
	// VM has answered it; the channel is not closed while one is open, or a
	// fast command (exit before the "exec" reply arrives) would reach the
	// runner as EOF. When the runner closes its side, the request stream
	// ends and the upstream channel is closed with it.
	var replying sync.Mutex
	go func() {
		for req := range reqs {
			replying.Lock()
			ok, err := upCh.SendRequest(req.Type, req.WantReply, req.Payload)
			if req.WantReply {
				_ = req.Reply(ok && err == nil, nil)
			}
			replying.Unlock()
		}
		_ = upCh.Close()
	}()

	upDone := make(chan struct{})
	go func() {
		forwardRequests(upReqs, ch)
		close(upDone)
	}()

	go func() {
		_, _ = io.Copy(upCh, ch)
		_ = upCh.CloseWrite()
	}()

	var out sync.WaitGroup
	out.Add(2)
	go func() { defer out.Done(); _, _ = io.Copy(ch, upCh) }()
	go func() { defer out.Done(); _, _ = io.Copy(ch.Stderr(), upCh.Stderr()) }()
	out.Wait()

	// All output is through. Wait for the upstream requests, which carry
	// exit-status, before closing, so the runner sees the real exit code.
	_ = ch.CloseWrite()
	select {
	case <-upDone:
	case <-time.After(10 * time.Second):
	}
	replying.Lock()
	_ = ch.Close()
	replying.Unlock()
	_ = upCh.Close()
}

func forwardRequests(in <-chan *ssh.Request, out ssh.Channel) {
	for req := range in {
		ok, err := out.SendRequest(req.Type, req.WantReply, req.Payload)
		if req.WantReply {
			_ = req.Reply(ok && err == nil, nil)
		}
	}
}

// DiscardGlobal answers global requests (such as keepalives) without
// forwarding them.
func DiscardGlobal(reqs <-chan *ssh.Request) {
	for req := range reqs {
		if req.WantReply {
			_ = req.Reply(false, nil)
		}
	}
}

// NewKey returns a fresh ed25519 key as a PEM private key and its signer.
func NewKey() ([]byte, ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(block), signer, nil
}

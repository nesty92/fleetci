// Package proxy ends gitlab-runner's SSH connection to a job VM inside
// fleetci and carries it on over fleetci's own connection to the real VM.
//
// gitlab-runner's vm_isolation can only log in to a VM with a fixed
// password from config.toml. That connection never leaves fleetci's process,
// so the password is a placeholder. The connection that reaches the VM uses
// the key its backend issued for that VM.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"

	"github.com/nesty92/fleetci/internal/sshx"
)

// Target is how to reach one VM.
type Target struct {
	Info provider.ConnectInfo
	// HostKey pins the VM's SSH host key. Nil skips the check.
	HostKey ssh.PublicKey
}

// ConnectFunc returns fresh connection details for a VM. Backends whose keys
// expire (such as EC2 Instance Connect) issue a new key on every call.
type ConnectFunc func(ctx context.Context) (Target, error)

// VM is one job VM behind the proxy. The runner's connections to it go
// over conn, the VM's one connection, which the VM shares with everything
// else that talks to the machine and leaves open when it goes.
type VM struct {
	ID   string
	conn *Conn
	log  hclog.Logger

	mu     sync.Mutex
	closed bool
}

func NewVM(id string, conn *Conn, log hclog.Logger) *VM {
	return &VM{ID: id, conn: conn, log: log.With("vm", id)}
}

// Serve ends one runner connection to this VM on conn and forwards its
// channels to the VM. It returns when the runner disconnects.
func (v *VM) Serve(ctx context.Context, conn net.Conn, cfg *ssh.ServerConfig) {
	defer conn.Close()

	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		v.log.Warn("runner handshake failed", "err", err)
		return
	}
	defer sc.Close()
	go sshx.DiscardGlobal(reqs)

	up, err := v.upstream(ctx)
	if err != nil {
		v.log.Error("connecting to vm", "err", err)
		return
	}

	for nc := range chans {
		go sshx.ForwardChannel(nc, up)
	}
}

// Close refuses further runner connections. The VM's connection stays: it
// is the machine's, not this job's.
func (v *VM) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.closed = true
	return nil
}

func (v *VM) upstream(ctx context.Context) (*ssh.Client, error) {
	v.mu.Lock()
	closed := v.closed
	v.mu.Unlock()
	if closed {
		return nil, errors.New("vm is gone")
	}
	client, _, err := v.conn.Client(ctx)
	return client, err
}

// How often a VM connection is checked, and how long a reply may take. A
// VM that vanished answers nothing, and TCP alone gives up only after
// minutes (about 15 on EC2), while the job looks alive.
const (
	KeepAliveInterval = 15 * time.Second
	KeepAliveTimeout  = 15 * time.Second
)

// KeepAlive checks c every interval and closes it when a check fails or
// gets no reply within timeout, so whatever waits on c ends. The channel
// closes when c is closed, by this or by anyone else.
func KeepAlive(c *ssh.Client, interval, timeout time.Duration) <-chan struct{} {
	done := make(chan struct{})
	closed := make(chan struct{})
	go func() { _ = c.Wait(); close(closed) }()
	go func() {
		defer close(done)
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-closed:
				return
			case <-tick.C:
			}
			reply := make(chan error, 1)
			go func() {
				_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
				reply <- err
			}()
			select {
			case err := <-reply:
				if err == nil {
					continue
				}
			case <-closed:
				return
			case <-time.After(timeout):
			}
			_ = c.Close()
			<-closed
			return
		}
	}()
	return done
}

// Dial opens an SSH connection to a VM, retrying until ctx or the target's
// timeout ends.
func Dial(ctx context.Context, t Target) (*ssh.Client, error) {
	signer, err := ssh.ParsePrivateKey(t.Info.Key)
	if err != nil {
		return nil, fmt.Errorf("parsing vm key: %w", err)
	}
	cfg := &ssh.ClientConfig{
		User:            t.Info.Username,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
	if t.HostKey != nil {
		// Ask for the pinned key's type, or the server may present another
		// of its host keys and fail the check.
		cfg.HostKeyCallback = ssh.FixedHostKey(t.HostKey)
		cfg.HostKeyAlgorithms = hostKeyAlgorithms(t.HostKey.Type())
	}

	port := t.Info.ProtocolPort
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(t.Info.InternalAddr, strconv.Itoa(port))
	if _, _, err := net.SplitHostPort(t.Info.InternalAddr); err == nil {
		addr = t.Info.InternalAddr
	}

	timeout := t.Info.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		client, err := dialOnce(ctx, addr, cfg)
		if err == nil {
			return client, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("dialing %s: %w", addr, err)
		case <-time.After(time.Second):
		}
	}
}

// hostKeyAlgorithms lists the signature algorithms for a key type. An RSA
// key signs with rsa-sha2-*, never with its type name alone.
func hostKeyAlgorithms(keyType string) []string {
	if keyType == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}
	return []string{keyType}
}

func dialOnce(ctx context.Context, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	d := net.Dialer{Timeout: cfg.Timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	// The handshake has the same bounds as the dial: a VM that takes the
	// connection and then says nothing (gone, or sshd stuck) would
	// otherwise hold it, and everyone waiting for the connection, forever.
	_ = conn.SetDeadline(time.Now().Add(cfg.Timeout))
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if !stop() && err == nil {
		err = ctx.Err() // the deadline is in the past now; the connection is spoilt
		_ = c.Close()
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

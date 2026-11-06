package proxy

import (
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/chaos"
	"golang.org/x/crypto/ssh"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

// vmServer is a VM's SSH server that takes any key and counts connections.
type vmServer struct {
	addr     string
	accepted atomic.Int32
	ended    atomic.Int32 // connections whose client went away
	mu       sync.Mutex
	conns    []*freezable
}

func newVMServer(t *testing.T) *vmServer {
	t.Helper()
	_, hostKey, _ := ed25519.GenerateKey(nil)
	signer, _ := ssh.NewSignerFromKey(hostKey)
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &vmServer{addr: ln.Addr().String()}
	t.Cleanup(func() { ln.Close(); s.drop() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.accepted.Add(1)
			f := &freezable{Conn: c, block: make(chan struct{})}
			s.mu.Lock()
			s.conns = append(s.conns, f)
			s.mu.Unlock()
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(f, cfg)
				if err != nil {
					return
				}
				go func() {
					for r := range reqs {
						if r.WantReply {
							_ = r.Reply(true, nil)
						}
					}
				}()
				for nc := range chans {
					_ = nc.Reject(ssh.Prohibited, "no")
				}
				s.ended.Add(1)
			}()
		}
	}()
	return s
}

// drop cuts every connection, as a VM that rebooted does.
func (s *vmServer) drop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.conns {
		if f.frozen.CompareAndSwap(true, false) {
			close(f.block)
		}
		f.Close()
	}
	s.conns = nil
}

// freeze stops answering on every connection, as a VM that vanished does.
func (s *vmServer) freeze() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.conns {
		f.frozen.Store(true)
	}
}

// connectTo hands out the server's details, counting the calls: each one
// is a fresh key.
func connectTo(t *testing.T, s *vmServer, calls *atomic.Int32) ConnectFunc {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(nil)
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(block)
	return func(context.Context) (Target, error) {
		calls.Add(1)
		return Target{Info: provider.ConnectInfo{
			ConnectorConfig: provider.ConnectorConfig{Username: "u", Key: pemKey, Timeout: 5 * time.Second},
			InternalAddr:    s.addr,
		}}, nil
	}
}

func lostWithin(t *testing.T, lost <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-lost:
	case <-time.After(5 * time.Second):
		t.Fatal(what)
	}
}

func TestEveryoneSharesOneConnection(t *testing.T) {
	s := newVMServer(t)
	var calls atomic.Int32
	c := NewConn(connectTo(t, s, &calls), hclog.NewNullLogger())
	defer c.Close()

	var wg sync.WaitGroup
	clients := make([]*ssh.Client, 10)
	for i := range clients {
		wg.Go(func() {
			cl, _, err := c.Client(context.Background())
			if err != nil {
				t.Error(err)
			}
			clients[i] = cl
		})
	}
	wg.Wait()
	for _, cl := range clients[1:] {
		if cl != clients[0] {
			t.Fatal("callers got different connections")
		}
	}
	if n, k := s.accepted.Load(), calls.Load(); n != 1 || k != 1 {
		t.Fatalf("%d connections from %d connect calls, want one of each", n, k)
	}
}

func TestADeadConnectionIsDialedAgainWithFreshDetails(t *testing.T) {
	s := newVMServer(t)
	var calls atomic.Int32
	c := NewConn(connectTo(t, s, &calls), hclog.NewNullLogger())
	defer c.Close()

	first, lost, err := c.Client(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.drop()
	lostWithin(t, lost, "lost never closed after the vm dropped the connection")

	second, _, err := c.Client(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("handed out the dead connection")
	}
	if n, k := s.accepted.Load(), calls.Load(); n != 2 || k != 2 {
		t.Fatalf("%d connections from %d connect calls, want two of each", n, k)
	}
}

func TestAConnectionThatStopsAnsweringIsLost(t *testing.T) {
	s := newVMServer(t)
	var calls atomic.Int32
	c := NewConn(connectTo(t, s, &calls), hclog.NewNullLogger())
	c.interval, c.timeout = 50*time.Millisecond, 100*time.Millisecond
	defer c.Close()

	_, lost, err := c.Client(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.freeze()
	lostWithin(t, lost, "a vm that stopped answering was never given up")
}

func TestAClosedConnRefusesAndDropsItsConnection(t *testing.T) {
	s := newVMServer(t)
	var calls atomic.Int32
	c := NewConn(connectTo(t, s, &calls), hclog.NewNullLogger())

	_, lost, err := c.Client(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	lostWithin(t, lost, "close left the connection open")
	if _, _, err := c.Client(context.Background()); !errors.Is(err, errConnClosed) {
		t.Fatalf("after close: %v, want errConnClosed", err)
	}
	if n := s.accepted.Load(); n != 1 {
		t.Fatalf("%d connections, want no dial after close", n)
	}
}

func TestCloseDuringADialDropsWhatTheDialBrings(t *testing.T) {
	s := newVMServer(t)
	var calls atomic.Int32
	inner := connectTo(t, s, &calls)
	release := make(chan struct{})
	c := NewConn(func(ctx context.Context) (Target, error) {
		<-release
		return inner(ctx)
	}, hclog.NewNullLogger())

	got := make(chan error, 1)
	go func() {
		_, _, err := c.Client(context.Background())
		got <- err
	}()
	time.Sleep(50 * time.Millisecond) // the dial is under way
	closed := make(chan struct{})
	go func() { c.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close waited for the dial")
	}
	close(release)

	if err := <-got; !errors.Is(err, errConnClosed) {
		t.Fatalf("dial that finished after close: %v, want errConnClosed", err)
	}
	// The connection the late dial made is dropped, not left open.
	deadline := time.Now().Add(5 * time.Second)
	for s.accepted.Load() != 1 || s.ended.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("%d connections, %d ended: want the late one dropped", s.accepted.Load(), s.ended.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// silent takes connections and never says a word, as a VM whose sshd is
// stuck does.
func silent(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	return ln.Addr().String()
}

func TestADialToAVMThatNeverAnswersEnds(t *testing.T) {
	var calls atomic.Int32
	connect := connectTo(t, &vmServer{addr: silent(t)}, &calls)
	for _, tc := range []struct {
		name    string
		ctx     time.Duration // the caller's deadline
		timeout time.Duration // the target's
	}{{"the caller gives up", 200 * time.Millisecond, time.Minute}, {"the target's timeout", time.Minute, 300 * time.Millisecond}} {
		target, _ := connect(context.Background())
		target.Info.Timeout = tc.timeout
		ctx, cancel := context.WithTimeout(context.Background(), tc.ctx)
		done := make(chan error, 1)
		go func() { _, err := Dial(ctx, target); done <- err }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("%s: dialed a vm that never answered", tc.name)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: the handshake with a silent vm never ended", tc.name)
		}
		cancel()
	}
}

func TestAConnLosesASilentVMWithinItsKeepAlive(t *testing.T) {
	s := newVMServer(t)
	// The link goes silent 100ms in, and stays open.
	x := chaos.NewProxy(t, chaos.NewPlan([]byte{byte(chaos.HoleLater), 25}), s.addr)
	var calls atomic.Int32
	c := NewConn(connectTo(t, &vmServer{addr: x.Addr}, &calls), hclog.NewNullLogger())
	c.SetKeepAlive(30*time.Millisecond, 60*time.Millisecond)
	defer c.Close()

	_, lost, err := c.Client(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-lost:
	case <-time.After(2 * time.Second):
		t.Fatal("a silent vm was not lost within its keepalive")
	}
}

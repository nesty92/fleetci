package proxy

import (
	"crypto/ed25519"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// freezable is a server-side connection that can stop reading, as a VM
// that vanished does: nothing answers, nothing closes.
type freezable struct {
	net.Conn
	frozen atomic.Bool
	block  chan struct{}
}

func (f *freezable) Read(b []byte) (int, error) {
	if f.frozen.Load() {
		<-f.block
	}
	return f.Conn.Read(b)
}

func sshPair(t *testing.T) (*ssh.Client, *freezable) {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(nil)
	signer, _ := ssh.NewSignerFromKey(key)
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	server := make(chan *freezable, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		f := &freezable{Conn: c, block: make(chan struct{})}
		t.Cleanup(func() { close(f.block); c.Close() })
		_, chans, reqs, err := ssh.NewServerConn(f, cfg)
		if err != nil {
			return
		}
		server <- f
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
	}()
	client, err := ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{
		User: "t", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, <-server
}

func TestKeepAliveClosesAConnectionThatStopsAnswering(t *testing.T) {
	client, server := sshPair(t)
	done := KeepAlive(client, 50*time.Millisecond, 200*time.Millisecond)

	select {
	case <-done:
		t.Fatal("closed a connection that answers")
	case <-time.After(300 * time.Millisecond):
	}

	server.frozen.Store(true)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("still open after the vm stopped answering")
	}
	if _, _, err := client.SendRequest("x", true, nil); err == nil {
		t.Fatal("client still usable")
	}
}

func TestKeepAliveEndsWhenTheClientCloses(t *testing.T) {
	client, _ := sshPair(t)
	done := KeepAlive(client, time.Hour, time.Hour)
	client.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("keepalive outlived its client")
	}
}

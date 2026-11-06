package proxy

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"
)

// errConnClosed is what a closed Conn answers.
var errConnClosed = errors.New("the vm's connection is closed")

// Conn is the one SSH connection to a VM, shared by everything that talks
// to it: the jobs on the VM and fleetci's own watch of it. It dials on
// first use, with fresh details from connect (EC2 Instance Connect keys
// last 60 seconds), and again once the connection dies. Users open
// channels on it and never close it; Close is for when the VM goes.
type Conn struct {
	connect  ConnectFunc
	log      hclog.Logger
	interval time.Duration // keepalive; tests shorten them
	timeout  time.Duration

	mu      sync.Mutex
	client  *ssh.Client
	lost    <-chan struct{}
	dialing chan struct{} // closed when the dial under way ends
	closed  bool
}

func NewConn(connect ConnectFunc, log hclog.Logger) *Conn {
	return &Conn{connect: connect, log: log, interval: KeepAliveInterval, timeout: KeepAliveTimeout}
}

// Client returns the connection, dialing it if there is none, and a
// channel that closes once that connection is gone. Callers that arrive
// during a dial wait for it.
func (c *Conn) Client(ctx context.Context) (*ssh.Client, <-chan struct{}, error) {
	for {
		c.mu.Lock()
		switch {
		case c.closed:
			c.mu.Unlock()
			return nil, nil, errConnClosed
		case c.client != nil && !isClosed(c.lost):
			client, lost := c.client, c.lost
			c.mu.Unlock()
			return client, lost, nil
		case c.dialing != nil:
			wait := c.dialing
			c.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		c.client, c.lost = nil, nil
		dialing := make(chan struct{})
		c.dialing = dialing
		c.mu.Unlock()

		client, err := c.dial(ctx)

		c.mu.Lock()
		c.dialing = nil
		close(dialing)
		if err != nil {
			c.mu.Unlock()
			return nil, nil, err
		}
		if c.closed {
			c.mu.Unlock()
			_ = client.Close()
			return nil, nil, errConnClosed
		}
		c.client, c.lost = client, KeepAlive(client, c.interval, c.timeout)
		client, lost := c.client, c.lost
		c.mu.Unlock()
		return client, lost, nil
	}
}

func (c *Conn) dial(ctx context.Context) (*ssh.Client, error) {
	t, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	client, err := Dial(ctx, t)
	if err != nil {
		return nil, err
	}
	c.log.Debug("connected to vm", "addr", t.Info.InternalAddr)
	return client, nil
}

// Close drops the connection for good. A dial under way is dropped when it
// ends.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}

func isClosed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

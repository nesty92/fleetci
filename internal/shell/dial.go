package shell

import (
	"context"
	"fmt"
	"net"

	"github.com/tailscale/tailcat"
)

// Dial connects to the SSH port of the tailcat address, for a client:
// fleetci ssh's ProxyCommand.
func Dial(ctx context.Context, addr, derpMapURL string, logf func(string, ...any)) (net.Conn, error) {
	if _, err := tailcat.ParseAddr(tailcat.Addr(addr)); err != nil {
		return nil, fmt.Errorf("not a tailcat address: %w", err)
	}
	c := &tailcat.Client{Server: tailcat.Addr(addr), DERPMapURL: derpMapURL, Logf: logf}
	conn, err := c.DialTCPPort(ctx, 22)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	return &clientConn{Conn: conn, c: c}, nil
}

// clientConn closes its tailcat client with it.
type clientConn struct {
	net.Conn
	c *tailcat.Client
}

func (c *clientConn) Close() error {
	err := c.Conn.Close()
	_ = c.c.Close()
	return err
}

package fleet

import (
	"context"
	"sync"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/proxy"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

// vmConns holds one shared SSH connection per instance of a pool: the
// jobs on the instance and the reclaim watch all use it.
type vmConns struct {
	group    provider.InstanceGroup
	external bool
	log      hclog.Logger

	mu     sync.Mutex
	conns  map[string]*proxy.Conn
	closed bool
}

func newVMConns(group provider.InstanceGroup, external bool, log hclog.Logger) *vmConns {
	return &vmConns{group: group, external: external, log: log, conns: map[string]*proxy.Conn{}}
}

// get is the connection to instance id, made on first use.
func (v *vmConns) get(id string) *proxy.Conn {
	v.mu.Lock()
	defer v.mu.Unlock()
	if c, ok := v.conns[id]; ok {
		return c
	}
	c := proxy.NewConn(v.connect(id), v.log.With("instance", id))
	if v.closed {
		_ = c.Close()
		return c
	}
	v.conns[id] = c
	return c
}

// drop closes the connection to id, once the instance is gone.
func (v *vmConns) drop(id string) {
	v.mu.Lock()
	c, ok := v.conns[id]
	delete(v.conns, id)
	v.mu.Unlock()
	if ok {
		_ = c.Close()
	}
}

// close drops every connection; get then hands out closed ones.
func (v *vmConns) close() {
	v.mu.Lock()
	v.closed = true
	conns := v.conns
	v.conns = map[string]*proxy.Conn{}
	v.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// connect is the fresh connection details for id: the plugin issues a new
// key on each call where keys are short-lived.
func (v *vmConns) connect(id string) proxy.ConnectFunc {
	return func(ctx context.Context) (proxy.Target, error) {
		info, err := v.group.ConnectInfo(ctx, id)
		if err != nil {
			return proxy.Target{}, err
		}
		return target(info, v.external), nil
	}
}

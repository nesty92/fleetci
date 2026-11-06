package proxy

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/chaos"
)

// chaosConn is a Conn to a VM behind a chaos proxy, with keepalives fast
// enough for a test, and the most dials it had under way at once.
func chaosConn(t *testing.T, p *chaos.Plan) (*Conn, *chaos.Proxy, *atomic.Int32) {
	t.Helper()
	s := newVMServer(t)
	x := chaos.NewProxy(t, p, s.addr)
	var calls, dialing, maxDialing atomic.Int32
	inner := connectTo(t, &vmServer{addr: x.Addr}, &calls)
	c := NewConn(func(ctx context.Context) (Target, error) {
		n := dialing.Add(1)
		defer dialing.Add(-1)
		for m := maxDialing.Load(); n > m && !maxDialing.CompareAndSwap(m, n); m = maxDialing.Load() {
		}
		t, err := inner(ctx)
		t.Info.Timeout = time.Second
		return t, err
	}, hclog.NewNullLogger())
	c.interval, c.timeout = 30*time.Millisecond, 60*time.Millisecond
	return c, x, &maxDialing
}

// users share c for d, as jobs and the reclaim watch do, each call bounded
// by its own deadline; a call that outlives the bound fails t.
func users(t *testing.T, c *Conn, d time.Duration) {
	t.Helper()
	const bound = 3 * time.Second
	var wg sync.WaitGroup
	stop := time.Now().Add(d)
	for range 4 {
		wg.Go(func() {
			for time.Now().Before(stop) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				returned := make(chan struct{})
				go func() {
					defer close(returned)
					if client, _, err := c.Client(ctx); err == nil {
						_, _, _ = client.SendRequest("keepalive@openssh.com", true, nil)
					}
				}()
				select {
				case <-returned:
					cancel()
				case <-time.After(bound):
					cancel()
					t.Errorf("a call hung past %v", bound)
					return
				}
			}
		})
	}
	wg.Wait()
}

func noneOpen(t *testing.T, x *chaos.Proxy) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); x.Open() != 0; {
		if time.Now().After(deadline) {
			t.Fatalf("%d connections left open after close", x.Open())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// FuzzConnUnderChaos has several users share one Conn while the network to
// the VM refuses, cuts, black-holes, slows and chops connections, then
// checks what must hold whatever the network did: one dial at a time, no
// call past its bound, the VM reachable again once the network is good,
// and nothing left open after close.
func FuzzConnUnderChaos(f *testing.F) {
	chaos.Seeds(f,
		nil,
		[]byte{byte(chaos.Refuse), 0, byte(chaos.Refuse), 0},
		[]byte{byte(chaos.Cut), 3, byte(chaos.Cut), 40},
		[]byte{byte(chaos.BlackHole), 0, byte(chaos.BlackHole), 2},
		[]byte{byte(chaos.BlackHole), 80, byte(chaos.Slow), 9, byte(chaos.Chop), 1},
		[]byte{byte(chaos.HoleLater), 20, byte(chaos.HoleLater), 5},
		[]byte{byte(chaos.CutLater), 30, byte(chaos.CutLater), 10, byte(chaos.Slow), 15},
	)
	f.Fuzz(func(t *testing.T, plan []byte) {
		p := chaos.NewPlan(plan)
		p.Report(t)
		c, x, maxDialing := chaosConn(t, p)

		users(t, c, 600*time.Millisecond)
		if n := maxDialing.Load(); n > 1 {
			t.Errorf("%d dials at once, want one at a time", n)
		}
		if t.Failed() {
			return
		}

		// Once the network is good again, the VM is reachable again.
		p.Calm()
		deadline := time.Now().Add(5 * time.Second)
		for {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			client, _, err := c.Client(ctx)
			if err == nil {
				_, _, err = client.SendRequest("keepalive@openssh.com", true, nil)
			}
			cancel()
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("never reached the vm once the network was good: %v", err)
			}
			time.Sleep(20 * time.Millisecond)
		}

		// Closed, it leaves nothing open.
		c.Close()
		noneOpen(t, x)
	})
}

// FuzzConnClosedUnderChaos closes a Conn while its users, and its dials,
// are under way on a bad network: it must refuse everyone after, and leave
// no connection open, even one a dial brings in late.
func FuzzConnClosedUnderChaos(f *testing.F) {
	chaos.Seeds(f,
		[]byte{50},
		[]byte{10, byte(chaos.Slow), 15, byte(chaos.Slow), 15},
		[]byte{100, byte(chaos.HoleLater), 30, byte(chaos.Chop), 0},
		[]byte{5, byte(chaos.Slow), 12, byte(chaos.CutLater), 2},
	)
	f.Fuzz(func(t *testing.T, plan []byte) {
		p := chaos.NewPlan(plan)
		p.Report(t)
		at := time.Duration(p.Pick(256)) * 2 * time.Millisecond
		c, x, _ := chaosConn(t, p)

		closed := time.AfterFunc(at, func() { p.Step("the connection closes"); c.Close() })
		defer closed.Stop()
		users(t, c, at+100*time.Millisecond)
		if t.Failed() {
			return
		}
		p.Calm()
		if _, _, err := c.Client(context.Background()); !errors.Is(err, errConnClosed) {
			t.Fatalf("after close: %v, want errConnClosed", err)
		}
		noneOpen(t, x)
	})
}

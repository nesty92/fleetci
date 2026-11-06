package github

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"

	"github.com/nesty92/fleetci/internal/chaos"
	"github.com/nesty92/fleetci/internal/proxy"
	"github.com/nesty92/fleetci/internal/source"
)

// FuzzRunnersUnderChaos starts runners on a VM, several at once on its one
// connection, while the network to it refuses, cuts, black-holes, slows and
// chops, then checks what must hold whatever the network did: a runner
// that started got its own jit config whole and its job is known, none
// outlives its deadline, runners start again once the network is good, and
// nothing is left open after the VM's connection closes.
func FuzzRunnersUnderChaos(f *testing.F) {
	chaos.Seeds(f,
		nil,
		[]byte{byte(chaos.Cut), 20, byte(chaos.Cut), 90},
		[]byte{byte(chaos.BlackHole), 30, byte(chaos.BlackHole), 120},
		[]byte{byte(chaos.CutLater), 10, byte(chaos.HoleLater), 25},
		[]byte{byte(chaos.Chop), 0, byte(chaos.Slow), 3},
	)
	f.Fuzz(func(t *testing.T, plan []byte) {
		p := chaos.NewPlan(plan)
		p.Report(t)
		var mu sync.Mutex
		got := map[string]bool{} // jit configs the vm received whole
		vm := newRunnerVM(t, func(stdin *bufio.Reader, stdout ssh.Channel) int {
			line, err := stdin.ReadString('\n')
			if err != nil {
				return 1
			}
			jit := strings.TrimSpace(line)
			mu.Lock()
			got[jit] = true
			mu.Unlock()
			_, _ = fmt.Fprintf(stdout, "2026-11-05 00:49:12Z: Running job: job-for-%s\n", jit)
			return 0
		})
		x := chaos.NewProxy(t, p, vm.addr)
		conn := chaosConn(t, x.Addr)
		start := SSHStart("2.0.0", "", hclog.NewNullLogger())

		run := func(ctx context.Context, jit string) error {
			var took atomic.Value
			err := start(ctx, VM{VM: source.VM{ID: "vm-1"}, Conn: conn}, jit, func(job string) { took.Store(job) })
			if err != nil {
				return err
			}
			mu.Lock()
			whole := got[jit]
			mu.Unlock()
			if !whole {
				t.Errorf("runner %s started without its jit config", jit)
			}
			for deadline := time.Now().Add(time.Second); took.Load() != "job-for-"+jit; time.Sleep(5 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Errorf("runner %s started, its job never known (%v)", jit, took.Load())
					break
				}
			}
			return nil
		}

		const bound = 4 * time.Second
		var wg sync.WaitGroup
		stop := time.Now().Add(600 * time.Millisecond)
		for r := range 3 {
			wg.Go(func() {
				for n := 0; time.Now().Before(stop); n++ {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					done := make(chan struct{})
					go func() { defer close(done); _ = run(ctx, fmt.Sprintf("jit-%d-%d", r, n)) }()
					select {
					case <-done:
						cancel()
					case <-time.After(bound):
						cancel()
						t.Errorf("a runner hung past %v", bound)
						return
					}
				}
			})
		}
		wg.Wait()
		if t.Failed() {
			return
		}

		// Once the network is good again, runners start again.
		p.Calm()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := run(ctx, "jit-after")
			cancel()
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("a runner never started once the network was good: %v", err)
			}
		}

		conn.Close()
		for deadline := time.Now().Add(5 * time.Second); x.Open() != 0; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%d connections to the vm left open after close", x.Open())
			}
		}
	})
}

// chaosConn is a VM's connection reached at addr, with keepalives fast
// enough for a test.
func chaosConn(t *testing.T, addr string) *proxy.Conn {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(nil)
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	c := proxy.NewConn(func(context.Context) (proxy.Target, error) {
		return proxy.Target{Info: provider.ConnectInfo{InternalAddr: addr, ConnectorConfig: provider.ConnectorConfig{
			Username: "u", Key: pem.EncodeToMemory(block), Timeout: time.Second,
		}}}, nil
	}, hclog.NewNullLogger())
	c.SetKeepAlive(30*time.Millisecond, 60*time.Millisecond)
	t.Cleanup(func() { c.Close() })
	return c
}

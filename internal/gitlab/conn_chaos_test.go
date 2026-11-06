package gitlab

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"

	"gitlab.com/gitlab-org/fleeting/fleeting/connector"
	fleetingprovider "gitlab.com/gitlab-org/fleeting/fleeting/provider"

	"github.com/nesty92/fleetci/internal/chaos"
	"github.com/nesty92/fleetci/internal/proxy"
)

// FuzzJobCommandsUnderChaos runs a job's commands on a VM, several at once
// on its one connection, while the network to it refuses, cuts,
// black-holes, slows and chops, then checks what must hold whatever the
// network did: a command that says it succeeded gave all its output, none
// outlives its deadline, commands run again once the network is good, and
// nothing is left open after the VM's connection closes.
func FuzzJobCommandsUnderChaos(f *testing.F) {
	chaos.Seeds(f,
		nil,
		[]byte{byte(chaos.Cut), 40, byte(chaos.Cut), 200},
		[]byte{byte(chaos.BlackHole), 30, byte(chaos.BlackHole), 120},
		[]byte{byte(chaos.CutLater), 10, byte(chaos.HoleLater), 25},
		[]byte{byte(chaos.Chop), 0, byte(chaos.Slow), 3},
	)
	f.Fuzz(func(t *testing.T, plan []byte) {
		p := chaos.NewPlan(plan)
		p.Report(t)
		vm := newJobVM(t, func(cmd string, stdin *bufio.Reader, stdout io.Writer) int {
			in, _ := io.ReadAll(stdin)
			_, _ = io.WriteString(stdout, cmd+":")
			_, _ = stdout.Write(in)
			return 0
		})
		x := chaos.NewProxy(t, p, vm.addr)
		conn := chaosConn(t, x.Addr)
		c := sharedClient{conn}

		run := func(ctx context.Context, n int) error {
			in := bytes.Repeat([]byte(fmt.Sprintf("line %d of the job's script\n", n)), 1000)
			var out bytes.Buffer
			err := c.Run(ctx, connector.RunOptions{Command: "sh", Stdin: bytes.NewReader(in), Stdout: &out})
			if err == nil && out.String() != "sh:"+string(in) {
				t.Errorf("a command that succeeded gave %d of %d bytes", out.Len(), len("sh:")+len(in))
			}
			return err
		}

		const bound = 4 * time.Second
		var wg sync.WaitGroup
		stop := time.Now().Add(600 * time.Millisecond)
		for job := range 3 {
			wg.Go(func() {
				for n := 0; time.Now().Before(stop); n++ {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					done := make(chan struct{})
					go func() { defer close(done); _ = run(ctx, job*1000+n) }()
					select {
					case <-done:
						cancel()
					case <-time.After(bound):
						cancel()
						t.Errorf("a command hung past %v", bound)
						return
					}
				}
			})
		}
		wg.Wait()
		if t.Failed() {
			return
		}

		// Once the network is good again, commands run again.
		p.Calm()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := run(ctx, -1)
			cancel()
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("a command never ran once the network was good: %v", err)
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
		return proxy.Target{Info: fleetingprovider.ConnectInfo{ID: "i-1", InternalAddr: addr, ConnectorConfig: fleetingprovider.ConnectorConfig{
			Username: "u", Key: pem.EncodeToMemory(block), Timeout: time.Second,
		}}}, nil
	}, hclog.NewNullLogger())
	c.SetKeepAlive(30*time.Millisecond, 60*time.Millisecond)
	t.Cleanup(func() { c.Close() })
	return c
}

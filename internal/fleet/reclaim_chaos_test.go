package fleet

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/nesty92/fleetci/internal/chaos"
	"github.com/nesty92/fleetci/internal/proxy"
)

// flakyIMDS answers some requests the way a metadata service under load or
// mid-restart does, as the plan picks: an error, throttled, a token it no
// longer knows, or late (under 256ms).
type flakyIMDS struct {
	*fakeIMDS
	plan *chaos.Plan
}

func (f flakyIMDS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch f.plan.Pick(5) {
	case 1:
		f.plan.Note("metadata service: answered 500")
		http.Error(w, "", http.StatusInternalServerError)
		return
	case 2:
		f.plan.Note("metadata service: answered 429, throttled")
		http.Error(w, "", http.StatusTooManyRequests)
		return
	case 3:
		f.plan.Note("metadata service: forgot its token")
		f.expire()
	case 4:
		d := time.Duration(f.plan.Pick(256)) * time.Millisecond
		f.plan.Note("metadata service: answered after %v", d)
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
	}
	f.fakeIMDS.ServeHTTP(w, r)
}

// FuzzReclaimWatchUnderChaos watches a VM while both the network to it and
// its metadata service misbehave, then checks what must hold whatever they
// did: no notice is made up, a notice that comes once things are good again
// is seen, and closing the watch ends it and leaves nothing open.
func FuzzReclaimWatchUnderChaos(f *testing.F) {
	chaos.Seeds(f,
		nil,
		[]byte{byte(chaos.Pass), 0, 1, 1, 1, 1, 1, 1}, // the service fails from the start
		[]byte{byte(chaos.Pass), 0, 2, 3, 2, 3, 2, 3}, // throttled, tokens lost
		[]byte{byte(chaos.Pass), 0, 4, 250, 4, 250, 4, 250},
		[]byte{byte(chaos.Refuse), 0, byte(chaos.Refuse), 0, byte(chaos.Refuse), 0},
		[]byte{byte(chaos.Cut), 2, byte(chaos.Cut), 30, byte(chaos.Cut), 60},
		[]byte{byte(chaos.BlackHole), 0, byte(chaos.BlackHole), 60},
		[]byte{byte(chaos.HoleLater), 20, byte(chaos.CutLater), 10, byte(chaos.Slow), 9},
	)
	f.Fuzz(func(t *testing.T, plan []byte) {
		p := chaos.NewPlan(plan)
		p.Report(t)
		imds := &fakeIMDS{notices: map[string]string{}}
		flaky := httptest.NewServer(flakyIMDS{imds, p})
		t.Cleanup(flaky.Close)
		vm := newVMSSH(t, flaky.Listener.Addr().String(), 0)
		x := chaos.NewProxy(t, p, vm.addr)

		w := fastWatch(func(context.Context, string) (tunnel, error) {
			conn, err := net.DialTimeout("tcp", x.Addr, time.Second)
			if err != nil {
				return tunnel{}, err
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			c, chans, reqs, err := ssh.NewClientConn(conn, x.Addr, &ssh.ClientConfig{
				User: "t", HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			})
			if err != nil {
				conn.Close()
				return tunnel{}, err
			}
			_ = conn.SetDeadline(time.Time{})
			client := ssh.NewClient(c, chans, reqs)
			return tunnel{
				dial:  client.DialContext,
				lost:  proxy.KeepAlive(client, 50*time.Millisecond, 100*time.Millisecond),
				close: func() { _ = client.Close() },
			}, nil
		}, false)
		// Slower than this is a service that does not answer, which the
		// watch rightly leaves alone (TestAVMWhoseMetadataNeverAnswersIsLeftAlone).
		w.timeout = 300 * time.Millisecond
		w.start("i-1")

		for stop := time.Now().Add(600 * time.Millisecond); time.Now().Before(stop); time.Sleep(5 * time.Millisecond) {
			if n := w.notice("i-1"); n != "" {
				t.Fatalf("a notice nobody sent: %q", n)
			}
		}

		// Once things are good again, a notice is seen.
		p.Calm()
		imds.set("spot/instance-action", `{"action": "terminate"}`)
		for deadline := time.Now().Add(5 * time.Second); w.notice("i-1") == ""; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the notice was never seen once things were good again; the watch gave up")
			}
		}

		closed := make(chan struct{})
		go func() { w.close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(3 * time.Second):
			t.Fatal("closing the watch hung")
		}
		for deadline := time.Now().Add(5 * time.Second); x.Open() != 0; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%d connections to the vm left open after the watch closed", x.Open())
			}
		}
	})
}

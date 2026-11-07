package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nesty92/fleetci/internal/chaos"
	"github.com/nesty92/fleetci/internal/source"
)

// flakyForge answers some requests the way GitHub does on a bad day, as the
// plan picks: an error, a rate limit, an answer lost after GitHub acted, a
// late one, or one cut off halfway. Polls, many and alike, draw on the plan
// one time in eight, so it lasts until runners are registered and removed.
func flakyForge(f *forge, p *chaos.Plan) func(w http.ResponseWriter, r *http.Request) bool {
	var polls atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) bool {
		path := strings.TrimPrefix(r.URL.Path, "/repos/o/r/actions/")
		if strings.HasPrefix(path, "runs") && polls.Add(1)%8 != 0 {
			return false
		}
		what := r.Method + " " + path
		switch p.Pick(8) {
		case 1:
			p.Note("github: %s answered 502", what)
			http.Error(w, "", http.StatusBadGateway)
		case 2:
			p.Note("github: %s rate limited, retry after 1s", what)
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		case 3:
			p.Note("github: %s rate limit spent", what)
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Unix()+1, 10))
			w.WriteHeader(http.StatusForbidden)
		case 4:
			p.Note("github: %s done, answer lost", what)
			f.lose(w, r)
		case 5:
			d := time.Duration(p.Pick(256)) * 2 * time.Millisecond
			p.Note("github: %s done after %v", what, d)
			time.Sleep(d) // done whether or not anyone still waits
			f.route(w, r)
		case 6:
			p.Note("github: %s done, answer cut halfway", what)
			rec := httptest.NewRecorder()
			f.route(rec, r)
			body := rec.Body.Bytes()
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(rec.Code)
			_, _ = w.Write(body[:len(body)/2])
			http.NewResponseController(w).Flush()
			hangUp(w)
		default:
			return false
		}
		return true
	}
}

// liveBackend is a fakeBackend that counts the VMs alive at once.
type liveBackend struct {
	*fakeBackend
	mu         sync.Mutex
	live, most int
}

func (b *liveBackend) Create(ctx context.Context, class string) (source.VM, error) {
	vm, err := b.fakeBackend.Create(ctx, class)
	b.mu.Lock()
	b.live++
	b.most = max(b.most, b.live)
	b.mu.Unlock()
	return vm, err
}

func (b *liveBackend) Delete(ctx context.Context, id string) error {
	b.mu.Lock()
	b.live--
	b.mu.Unlock()
	return b.fakeBackend.Delete(ctx, id)
}

// FuzzDispatcherUnderChaos serves queued jobs while both the network to
// GitHub and GitHub itself misbehave, then checks what must hold whatever
// they did: never more VMs than jobs, and once things are good again every
// job runs once, no runner is left registered, every VM is released, and
// the dispatcher stops when told.
func FuzzDispatcherUnderChaos(f *testing.F) {
	chaos.Seeds(f,
		nil,
		[]byte{byte(chaos.Pass), 0, 4, 4, 4, 4, 4, 4, 4, 4},   // every answer lost
		[]byte{byte(chaos.Pass), 0, 2, 3, 2, 3},               // rate limited
		[]byte{byte(chaos.Pass), 0, 5, 250, 5, 250, 5, 200},   // late, past the timeout
		[]byte{byte(chaos.Pass), 0, 6, 6, 6, 6, 1, 1, 6, 6},   // cut halfway, errors
		[]byte{byte(chaos.Cut), 10, byte(chaos.Refuse), 0, 4}, // the network too
		[]byte{byte(chaos.BlackHole), 20, byte(chaos.HoleLater), 25},
		[]byte{byte(chaos.Slow), 9, 5, 120, byte(chaos.Chop), 1, 4},
		// Registrations and removals fail, over and over.
		[]byte{byte(chaos.Pass), 0, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1},
		[]byte{byte(chaos.Pass), 0, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4},
		[]byte{byte(chaos.Pass), 0, 6, 1, 6, 1, 6, 1, 6, 1, 6, 1, 6, 1},
	)
	f.Fuzz(func(t *testing.T, plan []byte) {
		p := chaos.NewPlan(plan)
		p.Report(t)
		forge := newForge(t, queuedJob(1, 7), queuedJob(2, 7), queuedJob(3, 8))
		forge.keep = true // removing runners is the dispatcher's to get right
		forge.fault = flakyForge(forge, p)
		srv := httptest.NewServer(forge)
		t.Cleanup(srv.Close)
		x := chaos.NewProxy(t, p, srv.Listener.Addr().String())
		backend := &liveBackend{fakeBackend: &fakeBackend{defaultClass: "2vcpu-1gb"}}
		_, stop := dispatch(t, forge, "http://"+x.Addr, backend)

		time.Sleep(600 * time.Millisecond)

		// Once things are good again, every job runs and nothing is left.
		p.Calm()
		for deadline := time.Now().Add(10 * time.Second); !forge.settled(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("never settled once things were good again: %v", forge)
			}
		}
		stop()
		backend.mu.Lock()
		defer backend.mu.Unlock()
		if backend.live != 0 {
			t.Errorf("%d vms never released", backend.live)
		}
		if backend.most > 3 {
			t.Errorf("%d vms at once for 3 jobs", backend.most)
		}
	})
}

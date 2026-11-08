package gitlab

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/nesty92/fleetci/internal/proxy"
	"github.com/nesty92/fleetci/internal/source"
)

// room is a host of classes() with room for everything; it counts the
// reservations and VMs held.
type room struct {
	mu       sync.Mutex
	next     int
	reserved map[string]bool
	vms      map[string]bool
	reserves int
}

func newRoom() *room { return &room{reserved: map[string]bool{}, vms: map[string]bool{}} }

func (r *room) Classes(context.Context) ([]source.Class, string, error) {
	return classes(), "1vcpu-2gb", nil
}

func (r *room) Reserve(_ context.Context, class string) (source.Reservation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	r.reserves++
	id := fmt.Sprintf("r-%d", r.next)
	r.reserved[id] = true
	return source.Reservation{ID: id, Class: class}, nil
}

func (r *room) Acquire(_ context.Context, res source.Reservation) (source.VM, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.reserved[res.ID] {
		return source.VM{}, errors.New("no such reservation")
	}
	delete(r.reserved, res.ID)
	id := "vm-" + res.ID
	r.vms[id] = true
	return source.VM{ID: id, Class: res.Class}, nil
}

func (r *room) Release(_ context.Context, res source.Reservation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.reserved[res.ID] {
		return errors.New("no such reservation")
	}
	delete(r.reserved, res.ID)
	return nil
}

func (r *room) Create(context.Context, string) (source.VM, error) {
	return source.VM{}, errors.New("not here")
}
func (r *room) HandOver(context.Context, string) bool { return false }
func (r *room) Conn(string) (*proxy.Conn, error)      { return nil, errors.New("no vms") }
func (r *room) Delete(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.vms, id)
	return nil
}

func (r *room) held() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reserved), len(r.vms)
}

// jobNetwork hands out a job, or none, as a test says, and answers the
// calls that follow as it says.
type jobNetwork struct {
	common.Network
	request func(ctx context.Context) (*spec.Job, bool)
	process error
	update  common.UpdateJobResult
}

func (n *jobNetwork) RequestJob(ctx context.Context, _ common.RunnerConfig, _ *common.SessionInfo) (*spec.Job, bool) {
	return n.request(ctx)
}

func (n *jobNetwork) ProcessJob(common.RunnerConfig, *common.JobCredentials) (common.JobTrace, error) {
	if n.process != nil {
		return nil, n.process
	}
	return &quietTrace{}, nil
}

func (n *jobNetwork) UpdateJob(common.RunnerConfig, *common.JobCredentials, common.UpdateJobInfo) common.UpdateJobResult {
	return n.update
}

type quietTrace struct{ common.JobTrace }

func (*quietTrace) Write(p []byte) (int, error)             { return len(p), nil }
func (*quietTrace) Finish()                                 {}
func (*quietTrace) Fail(error, common.JobFailureData) error { return nil }

func aJob(context.Context) (*spec.Job, bool) {
	return &spec.Job{ID: 1, Token: "job-token"}, true
}

// Whatever comes of asking GitLab for a job, short of running one, the
// room reserved for it is given back.
func TestNoJobLeavesNoRoomReserved(t *testing.T) {
	for _, tc := range []struct {
		name      string
		net       *jobNetwork
		interrupt bool
	}{
		{"no job", &jobNetwork{request: func(context.Context) (*spec.Job, bool) { return nil, true }}, false},
		{"gitlab unreachable", &jobNetwork{request: func(context.Context) (*spec.Job, bool) { return nil, false }}, false},
		{"request cut short by shutdown", &jobNetwork{request: func(ctx context.Context) (*spec.Job, bool) {
			<-ctx.Done()
			return nil, false
		}}, true},
		{"job, its trace refused", &jobNetwork{request: aJob, process: errors.New("409 conflict")}, false},
		{"job, gitlab aborts it", &jobNetwork{request: aJob, update: common.UpdateJobResult{State: common.UpdateAbort}}, false},
		{"job, cancelled at once", &jobNetwork{request: aJob, update: common.UpdateJobResult{State: common.UpdateSucceeded, CancelRequested: true}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vms := newRoom()
			l, err := New(t.Context(), Config{Runners: []Runner{{Class: "1vcpu-2gb", TokenFile: tokenFile(t)}}},
				vms, hclog.NewNullLogger())
			if err != nil {
				t.Fatal(err)
			}
			l.mr.network = tc.net
			if tc.interrupt {
				go func() {
					select {
					case l.mr.runInterruptSignal <- interruptSignal{}:
					case <-time.After(5 * time.Second):
					}
				}()
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = l.mr.processRunner(1, l.mr.config.Runners[0], make(chan *common.RunnerConfig, 1))
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("processRunner hung")
			}
			reserved, running := vms.held()
			if vms.reserves != 1 || reserved != 0 || running != 0 {
				t.Fatalf("%d reserved, %d reservations held, %d vms after; want 1, 0, 0", vms.reserves, reserved, running)
			}
		})
	}
}

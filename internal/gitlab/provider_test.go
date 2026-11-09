package gitlab

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/sirupsen/logrus"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/buildlogger"
	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/nesty92/fleetci/internal/proxy"
	"github.com/nesty92/fleetci/internal/source"
)

// fakeHost has free[class] VMs of room per class, and records what jobs
// did with it.
type fakeHost struct {
	mu      sync.Mutex
	free    map[string]int
	created []string // classes Create made a VM of
}

type fakeRes struct {
	h        *fakeHost
	class    string
	acquired bool
	released bool
}

func (r *fakeRes) Class() string { return r.class }
func (r *fakeRes) Acquire(context.Context) (source.VM, error) {
	if r.acquired || r.released {
		return source.VM{}, errors.New("reservation already used")
	}
	r.acquired = true
	return source.VM{ID: "vm-" + r.class, Class: r.class}, nil
}
func (r *fakeRes) Release() {
	if r.acquired || r.released {
		return
	}
	r.released = true
	r.h.mu.Lock()
	r.h.free[r.class]++
	r.h.mu.Unlock()
}

func (h *fakeHost) Reserve(class string) (reservation, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.free[class] == 0 {
		return nil, source.ErrNoRoom
	}
	h.free[class]--
	return &fakeRes{h: h, class: class}, nil
}

func (h *fakeHost) Create(_ context.Context, class string) (source.VM, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.created = append(h.created, class)
	return source.VM{ID: "vm-" + class, Class: class}, nil
}

func (*fakeHost) HandOver(context.Context, string) bool { return false }
func (*fakeHost) Conn(string) (*proxy.Conn, error) {
	return nil, errors.New("fake vm")
}

func (*fakeHost) Delete(context.Context, string) error { return nil }

func byJobProvider(h *fakeHost) *provider {
	return newProvider(nil, h, func(*common.RunnerConfig) string { return "" },
		[]string{"1vcpu-2gb", "2vcpu-4gb", "4vcpu-8gb"}, "2vcpu-4gb")
}

func jobOf(class string) common.ExecutorPrepareOptions {
	b := &common.Build{Runner: &common.RunnerConfig{}}
	if class != "" {
		b.Variables = spec.Variables{{Key: classVariable, Value: class}}
	}
	log := buildlogger.New(nil, logrus.NewEntry(logrus.New()), buildlogger.Options{})
	return common.ExecutorPrepareOptions{Build: b, BuildLogger: log, Context: context.Background()}
}

func TestAJobOfNoClassHoldsRoomInTheFirstClassWithSome(t *testing.T) {
	h := &fakeHost{free: map[string]int{"1vcpu-2gb": 1, "2vcpu-4gb": 1}}
	p := byJobProvider(h)

	for _, want := range []string{"2vcpu-4gb", "1vcpu-2gb"} {
		data, err := p.Acquire(&common.RunnerConfig{})
		if err != nil {
			t.Fatal(err)
		}
		if got := data.(*acquisitionRef).res.Class(); got != want {
			t.Fatalf("reserved %s, want %s (the default class first)", got, want)
		}
	}
	_, err := p.Acquire(&common.RunnerConfig{})
	var noFree *common.NoFreeExecutorError
	if !errors.As(err, &noFree) {
		t.Fatalf("host full: %v, want NoFreeExecutorError", err)
	}
}

func TestPrepareByJob(t *testing.T) {
	for _, tc := range []struct {
		name, job  string
		wantVM     string
		wantCreate bool
		wantErr    bool
	}{
		{"the reserved class", "2vcpu-4gb", "2vcpu-4gb", false, false},
		{"FLEETCI_CLASS unset: the default", "", "2vcpu-4gb", false, false},
		{"another class waits for its own room", "4vcpu-8gb", "4vcpu-8gb", true, false},
		{"an unknown class", "nope", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &fakeHost{free: map[string]int{"2vcpu-4gb": 1}}
			p := byJobProvider(h)
			data, err := p.Acquire(&common.RunnerConfig{})
			if err != nil {
				t.Fatal(err)
			}
			ref := data.(*acquisitionRef)
			e := &executor{p: p}

			vm, err := e.prepareByJob(jobOf(tc.job), ref.res)
			if (err != nil) != tc.wantErr || vm.Class != tc.wantVM {
				t.Fatalf("vm %q, err %v; want %q", vm.Class, err, tc.wantVM)
			}
			if got := len(h.created) > 0; got != tc.wantCreate {
				t.Fatalf("created %v", h.created)
			}
			// Room not used goes back at once.
			if want := map[bool]int{true: 0, false: 1}[tc.wantVM == "2vcpu-4gb"]; h.free["2vcpu-4gb"] != want {
				t.Fatalf("2vcpu-4gb room %d, want %d", h.free["2vcpu-4gb"], want)
			}
			p.Release(nil, ref) // after any of these, a no-op
			if h.free["2vcpu-4gb"] > 1 {
				t.Fatal("room given back twice")
			}
		})
	}
}

func TestARetriedPrepareWaitsForItsClass(t *testing.T) {
	h := &fakeHost{free: map[string]int{"2vcpu-4gb": 1, "4vcpu-8gb": 1}}

	// A class's own runner: the class is the reservation's.
	p := newProvider(nil, h, func(*common.RunnerConfig) string { return "4vcpu-8gb" }, nil, "")
	data, _ := p.Acquire(&common.RunnerConfig{})
	ref := data.(*acquisitionRef)
	_, _ = ref.res.Acquire(context.Background()) // the first attempt spent it
	ref.tried = true
	if vm, err := (&executor{p: p}).retry(jobOf(""), ref); err != nil || vm.Class != "4vcpu-8gb" {
		t.Fatalf("class runner's retry: %v %v", vm.Class, err)
	}

	// A config.toml runner: the job's class.
	p = byJobProvider(h)
	data, _ = p.Acquire(&common.RunnerConfig{})
	ref = data.(*acquisitionRef)
	ref.res.Release()
	if vm, err := (&executor{p: p}).retry(jobOf("1vcpu-2gb"), ref); err != nil || vm.Class != "1vcpu-2gb" {
		t.Fatalf("config.toml runner's retry: %v %v", vm.Class, err)
	}
}

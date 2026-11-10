package cluster

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/fleet"
)

func TestFitClasses(t *testing.T) {
	classes := map[string]*ClassSpec{
		"small": {CPUs: 1, Memory: 2048, Arch: "amd64"},
		"big":   {CPUs: 4, Memory: 8192, Arch: "amd64"},
		"arm":   {CPUs: 1, Memory: 2048, Arch: "arm64"},
		"gpu":   {CPUs: 1, Memory: 2048, Arch: "amd64", On: map[string]string{"gpu": "yes"}},
	}
	f := Facts{Arch: "amd64", CPUs: 2, MemoryMB: 4096, DiskMB: 100 << 10}
	fit := FitClasses(classes, f)
	if got := strings.Join(classNames(fit.Classes), ","); got != "small" {
		t.Fatalf("classes %s, want small", got)
	}
	for _, name := range []string{"big", "arm", "gpu"} {
		if fit.Refused[name] == "" {
			t.Errorf("%s not refused", name)
		}
	}
	f.KVM = "no /dev/kvm"
	if fit := FitClasses(classes, f); len(fit.Classes) != 0 {
		t.Errorf("without KVM it runs %v", classNames(fit.Classes))
	}
}

func TestJoinPKI(t *testing.T) {
	ctl := PKI{Dir: t.TempDir()}
	if _, err := ctl.InitCA(); err != nil {
		t.Fatal(err)
	}
	if err := ctl.SignSelf(RoleController, "ctl-1", []string{"ctl-1"}); err != nil {
		t.Fatal(err)
	}
	ca, _ := ctl.CA()
	tok := NewToken(CAHash(ca))
	parsed, err := ParseToken(tok.String())
	if err != nil || parsed != tok {
		t.Fatalf("token round trip: %v %+v", err, parsed)
	}
	if !SecretMatches(tok.Secret, HashSecret(tok.Secret)) || SecretMatches("x", HashSecret(tok.Secret)) {
		t.Fatal("secret check")
	}

	w := PKI{Dir: t.TempDir()}
	csr, err := w.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ctl.Sign(csr, RoleWorker, "w1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Install(cert, ca.Raw); err != nil {
		t.Fatal(err)
	}
	c, err := w.Cert()
	if err != nil {
		t.Fatal(err)
	}
	id, err := IdentityOf(c)
	if err != nil || id.ID != "w1" || id.Role != RoleWorker {
		t.Fatalf("identity %+v %v", id, err)
	}
}

func TestGroupName(t *testing.T) {
	a, b := groupName("w1", "1vcpu-2gb"), groupName("w1", "4vcpu-8gb")
	if a == b || len(a) > 10 {
		t.Fatalf("%s %s", a, b)
	}
}

func TestElect(t *testing.T) {
	now := time.Now()
	started := now.Add(-time.Minute)
	live := func(leader string) peerState { return peerState{seen: now, leader: leader} }
	dead := peerState{seen: now.Add(-time.Minute), leader: "ctl-0"}
	cases := []struct {
		name, self, current, want string
		peers                     map[string]peerState
		started                   time.Time
	}{
		{"alone", "ctl-1", "", "ctl-1", nil, started},
		{"lowest when nobody leads", "ctl-2", "", "ctl-1", map[string]peerState{"ctl-1": live("")}, started},
		{"a leader keeps it", "ctl-1", "", "ctl-2", map[string]peerState{"ctl-2": live("ctl-2")}, started},
		{"and keeps it itself", "ctl-2", "ctl-2", "ctl-2", map[string]peerState{"ctl-1": live("ctl-2")}, started},
		{"two leaders: the lower", "ctl-2", "ctl-2", "ctl-1", map[string]peerState{"ctl-1": live("ctl-1")}, started},
		{"a dead leader does not count", "ctl-2", "", "ctl-2", map[string]peerState{"ctl-0": dead}, started},
		{"just started: wait", "ctl-1", "", "", map[string]peerState{"ctl-2": live("")}, now},
		{"just started: a leader is a leader", "ctl-1", "", "ctl-2", map[string]peerState{"ctl-2": live("ctl-2")}, now},
	}
	for _, c := range cases {
		if got := elect(c.self, c.current, c.peers, c.started, now); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestTaskReportable(t *testing.T) {
	now := time.Now()
	running := &task{status: AssignmentStatus{Phase: PhaseRunning}}
	if !running.reportable(now) {
		t.Error("a running task is not reported")
	}
	done := &task{status: AssignmentStatus{Phase: PhaseSucceeded}, done: now.Add(-time.Hour), listed: now.Add(-time.Second)}
	if !done.reportable(now) {
		t.Error("a finished task a controller still lists is not reported")
	}
	done.listed = now.Add(-time.Hour)
	if done.reportable(now) {
		t.Error("a finished task nobody lists is still reported")
	}
}

// TestReconcileTakesRoom: no report has a new assignment's task without
// the room its VM takes, or the controllers place another VM in that room.
func TestReconcileTakesRoom(t *testing.T) {
	const class = "1vcpu-2gb"
	worker := func() *Worker {
		return &Worker{log: hclog.NewNullLogger(), backend: &fleet.Backend{},
			budget:   fleet.Resources{CPUs: 2, MemoryMB: 4096},
			classes:  map[string]Class{class: {Name: class, ClassSpec: ClassSpec{CPUs: 1, Memory: 2048}}},
			fclasses: map[string]fleet.Class{class: {MaxInstances: 2}},
			passed:   map[string]bool{class: true}, checks: map[string]string{}, count: map[string]int{},
			vms: map[string]*workerVM{}, tasks: map[string]*task{}, changed: make(chan struct{}, 1)}
	}
	// The zero backend fails each boot, which gives its room back: a
	// report comes before or after that, never in between.
	w := worker()
	w.reconcile(context.Background(), []Assignment{{ID: "a1", Class: class}, {ID: "a2", Class: class}})
	r := w.report()
	pending := 0
	for _, s := range r.Assignments {
		if s.Phase == PhasePending {
			pending++
		}
	}
	if pending+r.Room[class] > 2 {
		t.Fatalf("%d pending and room for %d more, in a budget for 2", pending, r.Room[class])
	}
	// One past the budget is refused there and then.
	w = worker()
	w.used, w.count[class] = w.budget, 2
	w.reconcile(context.Background(), []Assignment{{ID: "a3", Class: class}})
	if st := w.tasks["a3"].status; st.Phase != PhaseFailed || st.Reason != ReasonNoRoom {
		t.Fatalf("one past the budget: %+v", st)
	}
}

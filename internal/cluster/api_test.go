package cluster

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
)

// testFleet is a controller served over mutual TLS, and a worker's client
// to it.
type testFleet struct {
	c      *Controller
	worker *client
	other  *client // another worker's
}

func newTestFleet(t *testing.T) *testFleet {
	t.Helper()
	ctlPKI := PKI{Dir: t.TempDir()}
	if _, err := ctlPKI.InitCA(); err != nil {
		t.Fatal(err)
	}
	if err := ctlPKI.SignSelf(RoleController, "ctl-1", []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	cfg := &ControllerConfig{
		Controller: ControllerTable{ID: "ctl-1", PKIDir: ctlPKI.Dir, StateDir: t.TempDir()},
		Classes:    map[string]*ClassSpec{"small": {CPUs: 1, Memory: 1024, Disk: 10 << 10, Arch: "amd64"}},
	}
	c, err := NewController(cfg, "test", hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := serverTLS(ctlPKI)
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: c.handler(), TLSConfig: tlsCfg}
	go func() { _ = srv.ServeTLS(lis, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	addr := lis.Addr().String()

	join := func(id string) *client {
		p := PKI{Dir: t.TempDir()}
		csr, err := p.NewKey()
		if err != nil {
			t.Fatal(err)
		}
		cert, err := ctlPKI.Sign(csr, RoleWorker, id, nil)
		if err != nil {
			t.Fatal(err)
		}
		ca, _ := ctlPKI.CA()
		if err := p.Install(cert, ca.Raw); err != nil {
			t.Fatal(err)
		}
		pool, _ := p.Pool()
		cl := newClient(addr, clientTLS(p, pool))
		t.Cleanup(cl.close)
		return cl
	}
	return &testFleet{c: c, worker: join("w1"), other: join("w2")}
}

func (f *testFleet) report(t *testing.T, r Report) StatusResponse {
	t.Helper()
	var resp StatusResponse
	up := StatusUpdate{Facts: Facts{Arch: "amd64", CPUs: 4, MemoryMB: 8192, DiskMB: 100 << 10}, Report: r}
	if err := f.worker.do(context.Background(), http.MethodPut, workerPath("w1", "/status"), up, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func (f *testFleet) watch(t *testing.T, rev string) AssignmentList {
	t.Helper()
	var l AssignmentList
	ctx, cancel := context.WithTimeout(context.Background(), watchTimeout+5*time.Second)
	defer cancel()
	if err := f.worker.do(ctx, http.MethodGet, workerPath("w1", "/assignments?rev="+rev), nil, &l); err != nil {
		t.Fatal(err)
	}
	return l
}

// TestAssignmentFlow walks an assignment through the API as a worker
// would: the leader writes it, the worker's watch sees it, its reports take
// it to Ready, the leader sets the job, and the worker reports it done.
func TestAssignmentFlow(t *testing.T) {
	f := newTestFleet(t)
	ready := Report{State: Ready, Room: map[string]int{"small": 2}}
	if resp := f.report(t, ready); len(resp.Fit.Classes) != 1 {
		t.Fatalf("fit %+v", resp.Fit)
	}
	l := f.watch(t, "")
	if len(l.Assignments) != 0 {
		t.Fatalf("assignments before any job: %+v", l)
	}

	type created struct {
		id  string
		err error
	}
	cc := make(chan created, 1)
	go func() {
		vm, err := f.c.Capacity().Create(context.Background(), "small")
		cc <- created{vm.ID, err}
	}()
	l = f.watch(t, l.Rev) // wakes on the new assignment
	if len(l.Assignments) != 1 || l.Assignments[0].Class != "small" || l.Assignments[0].Run != nil {
		t.Fatalf("watch: %+v", l)
	}
	a := l.Assignments[0]
	st := AssignmentStatus{ID: a.ID, Class: "small", Phase: PhasePending}
	ready.Assignments = []AssignmentStatus{st}
	f.report(t, ready)
	st.Phase, st.VM, st.Instance = PhaseReady, "vm-1", "10.0.0.2"
	ready.Assignments = []AssignmentStatus{st}
	f.report(t, ready)
	r := <-cc
	if r.err != nil || r.id != "w1/"+a.ID {
		t.Fatalf("create: %+v", r)
	}

	done := make(chan error, 1)
	took := make(chan string, 1)
	go func() {
		done <- f.c.runOn(context.Background(), r.id, RunSpec{GitHub: &GitHubRun{JITConfig: "jit"}}, func(j string) { took <- j })
	}()
	l = f.watch(t, l.Rev)
	if len(l.Assignments) != 1 || l.Assignments[0].Run == nil || l.Assignments[0].Run.GitHub.JITConfig != "jit" {
		t.Fatalf("watch after run: %+v", l)
	}
	st.Phase, st.Took = PhaseRunning, "job-7"
	ready.Assignments = []AssignmentStatus{st}
	f.report(t, ready)
	if j := <-took; j != "job-7" {
		t.Fatalf("took %q", j)
	}
	st.Phase, st.Err = PhaseFailed, "exit 1"
	ready.Assignments = []AssignmentStatus{st}
	f.report(t, ready)
	if err := <-done; err == nil || err.Error() != "exit 1" {
		t.Fatalf("run: %v", err)
	}
	// Over and followed by no one: it is dropped, and the watch sees that.
	l = f.watch(t, l.Rev)
	if len(l.Assignments) != 0 {
		t.Fatalf("after the job: %+v", l)
	}
}

// TestCreateCancelled: a job that goes while its VM boots cancels the
// assignment, and a worker that has no room makes Create try again.
func TestCreateCancelled(t *testing.T) {
	f := newTestFleet(t)
	ready := Report{State: Ready, Room: map[string]int{"small": 1}}
	f.report(t, ready)
	l := f.watch(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := f.c.Capacity().Create(ctx, "small")
		errc <- err
	}()
	l = f.watch(t, l.Rev)
	if len(l.Assignments) != 1 {
		t.Fatalf("%+v", l)
	}
	// The one slot is taken by the unreported assignment.
	if n := len(f.c.candidates("small")); n != 0 {
		t.Fatalf("%d candidates with the slot assigned", n)
	}
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("create: %v", err)
	}
	l = f.watch(t, l.Rev)
	if len(l.Assignments) != 1 || !l.Assignments[0].Cancel {
		t.Fatalf("not cancelled: %+v", l)
	}
}

// TestWorkerPaths: a worker reaches only its own paths, and none of a
// controller's.
func TestWorkerPaths(t *testing.T) {
	f := newTestFleet(t)
	var l AssignmentList
	err := f.other.do(context.Background(), http.MethodGet, workerPath("w1", "/assignments"), nil, &l)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusForbidden {
		t.Fatalf("w2 reading w1's assignments: %v", err)
	}
	err = f.worker.do(context.Background(), http.MethodGet, "/v1/status", nil, nil)
	if !errors.As(err, &se) || se.Code != http.StatusForbidden || !strings.Contains(se.Msg, "controller") {
		t.Fatalf("a worker reading the status: %v", err)
	}
}

// TestDrainShared: a drain said on one controller reaches another by its
// pings, and the later of two wins.
func TestDrainShared(t *testing.T) {
	a, b := newTestFleet(t), newTestFleet(t)
	a.report(t, Report{State: Ready})
	if err := a.c.workerOp("drain", "w1"); err != nil {
		t.Fatal(err)
	}
	from := func(c *Controller, id string) pingMsg { p := c.pingMsg(); p.ID = id; return p }
	b.c.merge(from(a.c, "ctl-a"))
	b.report(t, Report{State: Ready})
	if resp := b.report(t, Report{State: Ready}); !resp.Draining {
		t.Fatal("the drain did not reach the other controller")
	}
	if err := b.c.workerOp("undrain", "w1"); err != nil {
		t.Fatal(err)
	}
	a.c.merge(from(b.c, "ctl-b"))
	if resp := a.report(t, Report{State: Ready}); resp.Draining {
		t.Fatal("the later undrain lost")
	}
}

// TestMissingAssignment: an assignment the worker had and stops reporting
// is lost only after forgetMissing, not at the first report without it.
func TestMissingAssignment(t *testing.T) {
	f := newTestFleet(t)
	w := Report{State: Ready, Room: map[string]int{"small": 1}}
	f.report(t, w)
	f.c.mu.Lock()
	wc := f.c.workers["w1"]
	f.c.mu.Unlock()
	a := f.c.assign(wc, "small")
	w.Assignments = []AssignmentStatus{{ID: a.ID, Class: "small", Phase: PhaseRunning}}
	f.report(t, w)
	w.Assignments = nil
	f.report(t, w)
	f.c.mu.Lock()
	phase := a.status.Phase
	a.missing = time.Now().Add(-forgetMissing - time.Second)
	f.c.mu.Unlock()
	if phase != PhaseRunning {
		t.Fatalf("lost at once: %s", phase)
	}
	f.report(t, w)
	f.c.mu.Lock()
	defer f.c.mu.Unlock()
	if a.status.Phase != PhaseFailed {
		t.Fatalf("still %s after forgetMissing", a.status.Phase)
	}
}

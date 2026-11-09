package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/fleet"
	"github.com/nesty92/fleetci/internal/github"
	"github.com/nesty92/fleetci/internal/source"
)

// Worker runs the VMs the controllers place on this machine. It dials every
// controller, watches its assignments there, makes them so and reports
// their status with its own; it holds no forge credentials, and nothing
// dials it.
type Worker struct {
	cfg     *WorkerConfig
	pki     PKI
	log     hclog.Logger
	version string

	// GitLab runs a GitLab job on a VM; set by fleetci, which links the
	// GitLab side in.
	GitLab func(ctx context.Context, vm fleet.VM, req GitLabJob) error

	mu       sync.Mutex
	facts    Facts
	budget   fleet.Resources
	classes  map[string]Class
	fclasses map[string]fleet.Class
	backend  *fleet.Backend
	building bool
	builtAt  time.Time // the last build's start
	checks   map[string]string
	passed   map[string]bool
	used     fleet.Resources
	count    map[string]int
	vms      map[string]*workerVM
	draining bool
	drainBy  map[string]bool // controller address → whether it drains this worker
	reach    []string
	github   bool
	clients  map[string]*client // controller address → its client
	tasks    map[string]*task   // assignment id → what this worker made of it
	changed  chan struct{}
}

// task is an assignment as this worker carries it out.
type task struct {
	a       Assignment
	status  AssignmentStatus
	started bool
	cancel  context.CancelFunc // the job's, once it runs
	done    time.Time
	listed  time.Time // when a controller last listed it
}

// reportable says whether a task goes in the reports: until it is over,
// and then while a controller lists it, which is how the leader says it
// has seen the end: it drops the assignment then.
func (t *task) reportable(now time.Time) bool {
	if !t.status.terminal() {
		return true
	}
	last := t.done
	if t.listed.After(last) {
		last = t.listed
	}
	return now.Sub(last) < reportDone
}

// GitLabJob is a GitLab job a controller placed on this worker.
type GitLabJob struct {
	Job    json.RawMessage // as GitLab gave it
	Runner json.RawMessage // the [[runners]] table, without its token
}

type workerVM struct {
	vm         fleet.VM
	size       fleet.Resources
	since      time.Time
	job        string
	took       string
	assignment string
	cancel     context.CancelFunc
}

// NewWorker reads the worker's config and certificate.
func NewWorker(cfg *WorkerConfig, version string, log hclog.Logger) (*Worker, error) {
	p := PKI{Dir: cfg.PKIDir}
	if !p.HasCert() {
		return nil, fmt.Errorf("%s: no certificate; run fleetci join first", p.Dir)
	}
	w := &Worker{cfg: cfg, pki: p, log: log, version: version,
		checks: map[string]string{}, passed: map[string]bool{}, count: map[string]int{},
		vms: map[string]*workerVM{}, drainBy: map[string]bool{}, clients: map[string]*client{},
		tasks: map[string]*task{}, changed: make(chan struct{}, 1)}
	f, err := GatherFacts(cfg, version)
	if err != nil {
		return nil, err
	}
	w.facts = f
	w.budget = fleet.Resources{CPUs: f.CPUs, MemoryMB: f.MemoryMB}
	return w, nil
}

// GatherFacts reads what this machine has.
func GatherFacts(cfg *WorkerConfig, version string) (Facts, error) {
	f := Facts{Arch: runtime.GOARCH, Labels: cfg.Labels, Only: cfg.Classes, Version: version}
	// The budget as fleetci serve works it out, for a class on the host.
	probe := map[string]fleet.Class{"probe": {Plugin: "fleeting-plugin-fleetingd", CPUs: 1, Memory: 1, MaxInstances: 1}}
	b, on, err := fleet.HostBudget(cfg.Host, probe)
	if err != nil {
		return f, err
	}
	if !on {
		return f, errors.New("the worker needs a host budget: [host] enabled can't be false")
	}
	f.CPUs, f.MemoryMB = b.CPUs, b.MemoryMB
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return f, err
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(cfg.StateDir, &st); err == nil {
		f.DiskMB = int64(st.Bavail) * st.Bsize / (1 << 20)
	}
	f.KVM = kvmProblem()
	return f, nil
}

func kvmProblem() string {
	k, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "/dev/kvm does not exist (in a cloud VM, turn on nested virtualisation)"
		}
		return err.Error()
	}
	k.Close()
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		return "/dev/net/tun: " + err.Error()
	}
	return ""
}

func (w *Worker) poke() {
	select {
	case w.changed <- struct{}{}:
	default:
	}
}

// Run watches every controller and reports to each until ctx ends, then
// removes the worker's VMs.
func (w *Worker) Run(ctx context.Context) error {
	pool, err := w.pki.Pool()
	if err != nil {
		return err
	}
	tlsCfg := clientTLS(w.pki, pool)
	for _, addr := range w.cfg.Controllers {
		w.clients[addr] = newClient(addr, tlsCfg)
	}
	var wg sync.WaitGroup
	for _, cl := range w.clients {
		wg.Go(func() { w.watchLoop(ctx, cl) })
	}
	wg.Go(func() { w.reportLoop(ctx) })
	wg.Go(func() { w.renewLoop(ctx) })
	wg.Go(func() { w.sweepLoop(ctx) })
	<-ctx.Done()
	wg.Wait()
	for _, cl := range w.clients {
		cl.close()
	}
	w.mu.Lock()
	b := w.backend
	w.mu.Unlock()
	if b != nil {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		w.log.Info("stopping: removing this worker's VMs")
		_ = b.Shutdown(sctx)
	}
	return nil
}

// watchLoop watches this worker's assignments on one controller and
// carries them out.
func (w *Worker) watchLoop(ctx context.Context, cl *client) {
	rev, backoff, up, warned := "", time.Second, false, false
	path := workerPath(w.cfg.ID, "/assignments")
	for ctx.Err() == nil {
		var list AssignmentList
		wctx, cancel := context.WithTimeout(ctx, watchTimeout+15*time.Second)
		err := cl.do(wctx, http.MethodGet, path+"?rev="+url.QueryEscape(rev), nil, &list)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if !warned {
				w.log.Warn("controller unreachable; trying again", "controller", cl.addr, "err", err)
			}
			up, rev, warned = false, "", true
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		if !up {
			w.log.Info("watching the controller's assignments", "controller", cl.addr)
		}
		up, rev, backoff, warned = true, list.Rev, time.Second, false
		w.reconcile(ctx, list.Assignments)
	}
}

// configure builds the worker's classes from the controllers' fit, the
// first time, and runs the microVM check on each.
func (w *Worker) configure(ctx context.Context, fit Fit) {
	w.mu.Lock()
	if w.backend != nil || w.building || time.Since(w.builtAt) < 5*time.Minute {
		// Another controller's fit, the same again, or too soon after a
		// failed build.
		w.mu.Unlock()
		return
	}
	w.building, w.builtAt = true, time.Now()
	w.mu.Unlock()
	go func() {
		err := w.build(ctx, fit)
		w.mu.Lock()
		w.building = false
		w.mu.Unlock()
		if err != nil {
			w.log.Error("building the classes", "err", err)
			w.mu.Lock()
			for _, c := range fit.Classes {
				w.checks[c.Name] = "FAIL: " + err.Error()
			}
			w.mu.Unlock()
			w.poke()
			return
		}
		w.verifyAll(ctx)
	}()
}

func (w *Worker) build(ctx context.Context, fit Fit) error {
	if len(fit.Classes) == 0 {
		return errors.New("the controllers gave this worker no class: " + fmt.Sprint(fit.Refused))
	}
	classes := map[string]Class{}
	fclasses := map[string]fleet.Class{}
	subnets, err := freeSubnets(len(fit.Classes))
	if err != nil {
		return err
	}
	egress := w.cfg.EgressInterface
	if egress == "" {
		if egress, err = defaultInterface(); err != nil {
			return err
		}
	}
	images := filepath.Join(w.cfg.StateDir, "images")
	for i, c := range fit.Classes {
		fc, err := w.fleetClass(c, egress, images, subnets[i])
		if err != nil {
			return err
		}
		classes[c.Name] = c
		fclasses[c.Name] = fc
		w.mu.Lock()
		w.checks[c.Name] = "building: downloading the image and making the snapshot"
		w.mu.Unlock()
	}
	w.mu.Lock()
	w.classes = classes
	w.fclasses = fclasses
	w.reach = fit.Reach
	w.github = fit.GitHub
	w.mu.Unlock()
	w.poke()
	w.log.Info("starting the classes; the first start downloads the image", "classes", slices.Sorted(mapsKeys(classes)))
	bctx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()
	b, err := fleet.New(bctx, fclasses, w.cfg.Host, w.log.Named("fleet"))
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.backend = b
	w.mu.Unlock()
	return nil
}

func mapsKeys[M ~map[string]V, V any](m M) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// fleetClass is a class as fleetingd runs it on this machine.
func (w *Worker) fleetClass(c Class, egress, images, subnet string) (fleet.Class, error) {
	disks := filepath.Join(w.cfg.StateDir, "vms-"+c.Name)
	for _, d := range []string{disks, images} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fleet.Class{}, err
		}
	}
	pc := map[string]any{
		"name":                     groupName(w.cfg.ID, c.Name),
		"egress_interface":         egress,
		"vm_disk_directory":        disks,
		"vm_image_directory":       images,
		"vm_subnet":                subnet,
		"vm_num_cpu_cores":         c.CPUs,
		"vm_memory_mb":             int64(c.Memory),
		"vm_disk_size_gb":          max(int64(c.Disk)/1024, 1),
		"vm_enable_virtio_console": true,
		"vm_package_upgrade":       false,
		"vm_prebuild_on_start":     true,
		"vm_snapshot":              true,
		"vm_image_url":             c.Image,
	}
	raw, err := json.Marshal(pc)
	if err != nil {
		return fleet.Class{}, err
	}
	n := min(w.budget.CPUs/c.CPUs, int(w.budget.MemoryMB/int64(c.Memory)))
	if c.MaxInstances > 0 {
		n = min(n, c.MaxInstances)
	}
	yes := true
	return fleet.Class{
		Plugin:       w.cfg.FleetingdPlugin,
		PluginConfig: fleet.RawConfig(raw),
		Connector:    fleet.Connector{Username: "ubuntu", Timeout: fleet.Duration(5 * time.Minute)},
		MaxInstances: max(n, 1),
		Host:         &yes,
		CPUs:         c.CPUs,
		Memory:       c.Memory,
	}, nil
}

// groupName is a class's fleetingd group on this worker: fleetingd wants
// 1-10 lowercase letters and digits.
func groupName(worker, class string) string {
	return fmt.Sprintf("fc%08x", crc32.ChecksumIEEE([]byte(worker+"/"+class)))
}

// freeSubnets picks /24s for the classes' VMs that clash with no route on
// this machine, in fleetingd's form "172.16.N.".
func freeSubnets(n int) ([]string, error) {
	out, err := exec.Command("ip", "-4", "route", "show", "table", "all").Output()
	if err != nil {
		return nil, fmt.Errorf("ip route: %w", err)
	}
	routes := string(out)
	var subnets []string
	for i := 200; i < 250 && len(subnets) < n; i++ {
		s := fmt.Sprintf("172.16.%d.", i)
		if !strings.Contains(routes, s) {
			subnets = append(subnets, s)
		}
	}
	if len(subnets) < n {
		return nil, errors.New("no free 172.16.200-249 subnet for the VMs")
	}
	return subnets, nil
}

// defaultInterface is the default route's interface.
func defaultInterface() (string, error) {
	out, err := exec.Command("ip", "-4", "route", "get", "1.1.1.1").Output()
	if err != nil {
		return "", fmt.Errorf("ip route get: %w", err)
	}
	f := strings.Fields(string(out))
	for i, x := range f {
		if x == "dev" && i+1 < len(f) {
			return f[i+1], nil
		}
	}
	return "", errors.New("no default route: set egress_interface in worker.toml")
}

// room is how many more VMs of each class fit now.
func (w *Worker) roomLocked() map[string]int {
	room := map[string]int{}
	for name := range w.classes {
		room[name] = 0
		if w.passed[name] && !w.draining {
			room[name] = w.fitsLocked(name)
		}
	}
	return room
}

// fitsLocked is how many more VMs of a class the budget holds now.
func (w *Worker) fitsLocked(name string) int {
	c, ok := w.classes[name]
	if !ok || w.backend == nil {
		return 0
	}
	free := fleet.Resources{CPUs: w.budget.CPUs - w.used.CPUs, MemoryMB: w.budget.MemoryMB - w.used.MemoryMB}
	n := min(free.CPUs/c.CPUs, int(free.MemoryMB/int64(c.Memory)))
	n = min(n, w.fclasses[name].MaxInstances-w.count[name])
	return max(n, 0)
}

func (w *Worker) report() Report {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := Report{Room: w.roomLocked(), UsedCPUs: w.used.CPUs, UsedMemory: w.used.MemoryMB, Checks: map[string]string{}}
	for k, v := range w.checks {
		r.Checks[k] = v
	}
	for id, v := range w.vms {
		r.VMs = append(r.VMs, VMInfo{ID: id, Class: v.vm.Class, Since: v.since, Job: v.job, Took: v.took, Assignment: v.assignment})
	}
	slices.SortFunc(r.VMs, func(a, b VMInfo) int { return strings.Compare(a.ID, b.ID) })
	now := time.Now()
	for _, t := range w.tasks {
		if t.reportable(now) {
			r.Assignments = append(r.Assignments, t.status)
		}
	}
	slices.SortFunc(r.Assignments, func(a, b AssignmentStatus) int { return strings.Compare(a.ID, b.ID) })
	anyPassed := false
	for _, ok := range w.passed {
		anyPassed = anyPassed || ok
	}
	switch {
	case w.draining:
		r.State = Draining
	case w.backend == nil:
		r.State, r.Reason = NotReady, "starting its classes"
		if len(w.classes) == 0 {
			r.Reason = "waiting for the controllers' classes"
		}
	case !anyPassed:
		r.State, r.Reason = NotReady, "the microVM check has not passed"
	default:
		r.State = Ready
	}
	return r
}

// reportLoop sends the worker's status to every controller when it
// changes and every 5 s: that is also its heartbeat. The answers carry the
// worker's classes and whether it is drained.
func (w *Worker) reportLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		up := StatusUpdate{Facts: w.facts, Report: w.report()}
		var wg sync.WaitGroup
		for addr, cl := range w.clients {
			wg.Go(func() {
				rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				var resp StatusResponse
				if err := cl.do(rctx, http.MethodPut, workerPath(w.cfg.ID, "/status"), up, &resp); err != nil {
					w.setDrain(addr, nil) // what it said no longer counts
					return
				}
				w.configure(ctx, resp.Fit)
				w.setDrain(addr, &resp.Draining)
			})
		}
		wg.Wait()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.changed:
		}
	}
}

// setDrain records whether a controller drains this worker, nil for one
// out of reach: it is drained while any does. The controllers share
// drains, so they disagree only for a ping or two.
func (w *Worker) setDrain(addr string, d *bool) {
	w.mu.Lock()
	if d == nil {
		delete(w.drainBy, addr)
	} else {
		w.drainBy[addr] = *d
	}
	was := w.draining
	w.draining = false
	for _, x := range w.drainBy {
		w.draining = w.draining || x
	}
	changed := was != w.draining
	w.mu.Unlock()
	if changed {
		w.log.Info("draining", "draining", !was)
		w.poke()
	}
}

// reconcile carries out a controller's list of this worker's assignments:
// a new one boots its VM, a job set runs on it, a cancel stops it. One no
// longer listed goes on: its controller may be gone.
func (w *Worker) reconcile(ctx context.Context, list []Assignment) {
	for _, a := range list {
		w.mu.Lock()
		t := w.tasks[a.ID]
		if t != nil {
			t.listed = time.Now()
		}
		if t == nil {
			if a.Cancel {
				w.mu.Unlock()
				continue
			}
			t = &task{a: a, status: AssignmentStatus{ID: a.ID, Class: a.Class, Phase: PhasePending, Since: time.Now()}, listed: time.Now()}
			w.tasks[a.ID] = t
			w.mu.Unlock()
			w.poke()
			go w.boot(ctx, t)
			continue
		}
		t.a.Cancel = t.a.Cancel || a.Cancel
		if t.a.Run == nil && a.Run != nil {
			t.a.Run = a.Run
		}
		w.mu.Unlock()
		w.advance(ctx, t)
	}
}

// boot takes room for the task's VM and boots it.
func (w *Worker) boot(ctx context.Context, t *task) {
	vm, err := w.createVM(ctx, t.a.Class, false)
	if err != nil {
		reason := ""
		if errors.Is(err, ErrNoRoom) {
			reason = ReasonNoRoom
		}
		w.finish(t, reason, err)
		return
	}
	w.mu.Lock()
	if v := w.vms[vm.VM]; v != nil {
		v.assignment = t.a.ID
	}
	t.status.Phase, t.status.VM, t.status.Instance, t.status.Class, t.status.Since = PhaseReady, vm.VM, vm.Instance, vm.Class, time.Now()
	w.mu.Unlock()
	w.poke()
	w.advance(ctx, t)
}

// advance moves a task on: a cancel stops it, a job given runs.
func (w *Worker) advance(ctx context.Context, t *task) {
	w.mu.Lock()
	phase, cancelled := t.status.Phase, t.a.Cancel
	switch {
	case t.status.terminal(), phase == PhasePending:
		w.mu.Unlock() // boot advances it when the VM is up
	case cancelled && phase == PhaseReady:
		vm := t.status.VM
		w.mu.Unlock()
		_ = w.delete(context.WithoutCancel(ctx), vm)
		w.finish(t, "", errors.New("cancelled by the controllers"))
	case cancelled && phase == PhaseRunning:
		cancel := t.cancel
		w.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	case phase == PhaseReady && t.a.Run != nil && !t.started:
		t.started = true
		w.mu.Unlock()
		go w.run(ctx, t)
	default:
		w.mu.Unlock()
	}
}

// finish ends a task: Succeeded with no error, else Failed.
func (w *Worker) finish(t *task, reason string, err error) {
	w.mu.Lock()
	if t.status.terminal() {
		w.mu.Unlock()
		return
	}
	t.status.Phase, t.status.Since, t.done = PhaseSucceeded, time.Now(), time.Now()
	if err != nil {
		t.status.Phase, t.status.Reason, t.status.Err = PhaseFailed, reason, err.Error()
	}
	w.mu.Unlock()
	w.poke()
}

// run runs a task's job on its VM, detached from any controller: the
// worker follows it and removes the VM when it ends.
func (w *Worker) run(ctx context.Context, t *task) {
	w.mu.Lock()
	v := w.vms[t.status.VM]
	if v == nil {
		w.mu.Unlock()
		w.finish(t, "", fmt.Errorf("its vm %s is gone", t.status.VM))
		return
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	run := *t.a.Run
	v.cancel, t.cancel = cancel, cancel
	t.status.Phase, t.status.Since = PhaseRunning, time.Now()
	w.mu.Unlock()
	w.poke()
	log := w.log.With("vm", v.vm.ID, "assignment", t.a.ID)
	var err error
	switch {
	case run.GitHub != nil:
		w.setJob(v, "github")
		err = w.runGitHub(rctx, cancel, v, *run.GitHub, log, func(job string) {
			w.mu.Lock()
			v.took, t.status.Took = job, job
			w.mu.Unlock()
			w.poke()
		})
	case run.GitLab != nil && w.GitLab != nil:
		w.setJob(v, "gitlab")
		err = w.GitLab(rctx, v.vm, GitLabJob{Job: run.GitLab.Job, Runner: run.GitLab.Runner})
	case run.GitLab != nil:
		err = errors.New("this worker can't run GitLab jobs")
	default:
		err = errors.New("the assignment's job is empty")
	}
	if ctx.Err() != nil {
		return // the worker is stopping; Run removes the VMs
	}
	w.mu.Lock()
	if t.a.Cancel && err != nil {
		err = fmt.Errorf("cancelled by the controllers: %w", err)
	}
	w.mu.Unlock()
	log.Info("job done; removing its vm", "err", err)
	_ = w.delete(context.WithoutCancel(ctx), v.vm.ID)
	w.finish(t, "", err)
}

func (w *Worker) setJob(v *workerVM, job string) {
	w.mu.Lock()
	v.job = job
	w.mu.Unlock()
	w.poke()
}

// bootedVM is a VM createVM booted.
type bootedVM struct {
	VM, Class, Instance string
}

// createVM boots a VM of the class; check, for the microVM check, takes
// room in a class not yet passed.
func (w *Worker) createVM(ctx context.Context, class string, check bool) (bootedVM, error) {
	w.mu.Lock()
	c, ok := w.classes[class]
	if !ok {
		w.mu.Unlock()
		return bootedVM{}, fmt.Errorf("this worker has no class %q", class)
	}
	room := w.roomLocked()[class]
	if check {
		room = w.fitsLocked(class)
	}
	if room < 1 {
		w.mu.Unlock()
		return bootedVM{}, ErrNoRoom
	}
	size := fleet.Resources{CPUs: c.CPUs, MemoryMB: int64(c.Memory)}
	w.used = addRes(w.used, size, 1)
	w.count[class]++
	b := w.backend
	w.mu.Unlock()
	undo := func() {
		w.mu.Lock()
		w.used = addRes(w.used, size, -1)
		w.count[class]--
		w.mu.Unlock()
		w.poke()
	}
	w.poke()
	r, err := b.Reserve(class)
	if err != nil {
		undo()
		if errors.Is(err, fleet.ErrNoRoom) {
			return bootedVM{}, ErrNoRoom
		}
		return bootedVM{}, err
	}
	actx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	vm, err := r.Acquire(actx)
	if err != nil {
		undo()
		return bootedVM{}, err
	}
	w.mu.Lock()
	w.vms[vm.ID] = &workerVM{vm: vm, size: size, since: time.Now()}
	w.mu.Unlock()
	w.poke()
	return bootedVM{VM: vm.ID, Class: vm.Class, Instance: vm.Instance}, nil
}

func addRes(a, b fleet.Resources, n int) fleet.Resources {
	return fleet.Resources{CPUs: a.CPUs + n*b.CPUs, MemoryMB: a.MemoryMB + int64(n)*b.MemoryMB}
}

// delete stops what runs on a VM and removes it. A VM already gone is no
// error.
func (w *Worker) delete(ctx context.Context, id string) error {
	w.mu.Lock()
	v, ok := w.vms[id]
	if ok {
		delete(w.vms, id)
		w.used = addRes(w.used, v.size, -1)
		w.count[v.vm.Class]--
	}
	b := w.backend
	w.mu.Unlock()
	if !ok {
		return nil
	}
	if v.cancel != nil {
		v.cancel()
	}
	err := b.Delete(context.WithoutCancel(ctx), id)
	w.poke()
	return err
}

// runGitHub runs a just-in-time runner on a VM until it exits. A runner
// that takes no job is stopped, as the leader's idle watch would; the
// leader may be gone.
func (w *Worker) runGitHub(ctx context.Context, cancel context.CancelFunc, v *workerVM, req GitHubRun, log hclog.Logger, took func(string)) error {
	start := github.SSHStart(req.RunnerVersion, req.CacheURL, log)
	tookc := make(chan struct{})
	go func() {
		idle := req.IdleTimeout + time.Minute
		if req.IdleTimeout == 0 {
			idle = 11 * time.Minute
		}
		select {
		case <-tookc:
		case <-ctx.Done():
		case <-time.After(idle):
			log.Warn("runner got no job; stopping it")
			cancel()
		}
	}()
	var once sync.Once
	return start(ctx, github.VM{VM: source.VM{ID: v.vm.ID, Class: v.vm.Class, Instance: v.vm.Instance}, Conn: v.vm.Conn}, req.JITConfig, func(job string) {
		once.Do(func() { close(tookc) })
		took(job)
	})
}

// idleVM is how long a VM may wait for the job it was made for; one a
// leader made and then lost is removed after it.
const idleVM = 15 * time.Minute

// A finished assignment stays in the reports while a controller lists it,
// and for reportDone after, longer than a watch takes to answer; it is
// remembered for forgetDone, so a controller that lists it again, after a
// partition, is not obeyed twice but told how it ended.
const (
	reportDone = 2 * watchTimeout
	forgetDone = time.Hour
)

// sweepLoop removes VMs that never got their job.
func (w *Worker) sweepLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var idle []string
		var orphans []*task
		w.mu.Lock()
		for id, v := range w.vms {
			if v.job == "" && time.Since(v.since) > idleVM {
				idle = append(idle, id)
				if t := w.tasks[v.assignment]; t != nil {
					orphans = append(orphans, t)
				}
			}
		}
		for id, t := range w.tasks {
			if t.status.terminal() && !t.reportable(time.Now()) && time.Since(t.done) > forgetDone {
				delete(w.tasks, id)
			}
		}
		w.mu.Unlock()
		for _, id := range idle {
			w.log.Warn("removing a vm no job came for", "vm", id, "after", idleVM.String())
			_ = w.delete(ctx, id)
		}
		for _, t := range orphans {
			w.finish(t, "", fmt.Errorf("no job came for its vm in %s", idleVM))
		}
	}
}

// renewLoop renews the worker's certificate after two thirds of its life,
// with any controller.
func (w *Worker) renewLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if err := w.renewIfDue(ctx); err != nil {
			w.log.Warn("renewing the certificate", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *Worker) renewIfDue(ctx context.Context) error {
	c, err := w.pki.Cert()
	if err != nil {
		return err
	}
	life := c.NotAfter.Sub(c.NotBefore)
	if time.Since(c.NotBefore) < life*2/3 {
		return nil
	}
	csr, err := w.pki.NewKey()
	if err != nil {
		return err
	}
	err = errors.New("no controller to renew it with")
	for _, cl := range w.clients {
		var resp renewResp
		rctx, cancel := context.WithTimeout(ctx, time.Minute)
		err = cl.do(rctx, http.MethodPost, workerPath(w.cfg.ID, "/certificate"), renewReq{CSR: csr}, &resp)
		cancel()
		if err != nil {
			continue
		}
		if err := w.pki.Install(resp.Cert, nil); err != nil {
			return err
		}
		w.log.Info("certificate renewed", "by", cl.addr)
		return nil
	}
	return err
}

package cluster

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"maps"
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

	"github.com/nesty92/fleetci/internal/cachedisk"
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
	idle     func(class string) int // in tests, in place of backend.Idle
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
	clients  map[string]*client   // controller address → its client
	tasks    map[string]*task     // assignment id → what this worker made of it
	lists    map[string]time.Time // controller address → when it last listed this worker's assignments
	changed  chan struct{}
	update   *updater
	started  time.Time          // when this process started
	verified time.Time          // the latest fleetci fleet verify it ran the checks for
	checking bool               // the microVM checks run
	stop     context.CancelFunc // ends Run
	exitErr  error              // what Run returns: ErrRestart for an update
}

// task is an assignment as this worker carries it out.
type task struct {
	a       Assignment
	status  AssignmentStatus
	started bool
	cancel  context.CancelFunc // the job's, once it runs
	// granted is closed when the leader's Cache comes, for a job that
	// waits for it.
	granted chan struct{}
	done    time.Time
	listed  time.Time // when a controller last listed it
	by      string    // the address of the controller that last listed it
}

// forgotten says whether a task can't get its job any more: its VM is up
// with none, and the controller that made it has gone on listing this
// worker's assignments without it, as one that restarted and lost them
// does. One whose controller is unreachable is not: it sends no lists.
func (t *task) forgotten(lastList time.Time) bool {
	return t.status.Phase == PhaseReady && t.a.Run == nil && t.by != "" && lastList.Sub(t.listed) > forgetMissing
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
	// Cache is the cache disk the worker attached to the VM, nil for
	// none; CacheNote says what it got, or why it has none, for the
	// job's log.
	Cache     *cachedisk.Scope
	CacheNote string
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
		tasks: map[string]*task{}, changed: make(chan struct{}, 1), started: time.Now()}
	f, err := GatherFacts(cfg, version)
	if err != nil {
		return nil, err
	}
	w.facts = f
	w.budget = fleet.Resources{CPUs: f.CPUs, MemoryMB: f.MemoryMB}
	if w.update, err = newUpdater(cfg.Update); err != nil {
		return nil, err
	}
	return w, nil
}

// GatherFacts reads what this machine has.
func GatherFacts(cfg *WorkerConfig, version string) (Facts, error) {
	f := Facts{Arch: runtime.GOARCH, Labels: cfg.Labels, Only: cfg.Classes, Version: version, Protocol: Protocol}
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
		f.DiskMB = int64(st.Bavail) * int64(st.Bsize) / (1 << 20)
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
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	w.mu.Lock()
	w.stop = stop
	w.mu.Unlock()
	var wg sync.WaitGroup
	wg.Go(func() { w.updateLoop(ctx) })
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
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.exitErr
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
		w.reconcile(ctx, cl.addr, list.Assignments)
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
	var cache *fleet.CacheDisk
	if c.CacheDisk {
		dir := filepath.Join(w.cfg.StateDir, "cache")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fleet.Class{}, err
		}
		pc["vm_cache_directory"] = dir
		cache = &fleet.CacheDisk{}
		if w.cfg.CacheDisk != nil {
			*cache = *w.cfg.CacheDisk
		}
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
		// Each VM takes one job, so one idle is never worth keeping: it
		// holds room the budget can't give a job.
		IdleTime:  fleet.Duration(idleSpare),
		Host:      &yes,
		CPUs:      c.CPUs,
		Memory:    c.Memory,
		CacheDisk: cache,
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

// drainedLocked: the controllers drain it, or it drains for an update.
func (w *Worker) drainedLocked() bool {
	return w.draining || (w.update != nil && w.update.status.State == UpdateDraining)
}

// room is how many more VMs of each class fit now.
func (w *Worker) roomLocked() map[string]int {
	room := map[string]int{}
	for name := range w.classes {
		room[name] = 0
		if w.passed[name] && !w.drainedLocked() {
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
	// The idle VMs of the other classes hold their room too; this class's
	// own are within its max_instances, and the next of its VMs is one.
	for other, oc := range w.classes {
		if other != name {
			k := w.idleVMs(other)
			free.CPUs -= k * oc.CPUs
			free.MemoryMB -= int64(k) * int64(oc.Memory)
		}
	}
	n := min(free.CPUs/c.CPUs, int(free.MemoryMB/int64(c.Memory)))
	n = min(n, w.fclasses[name].MaxInstances-w.count[name])
	return max(n, 0)
}

// idleVMs is how many of a class's VMs are booted with no job, which no
// task of the worker's holds.
func (w *Worker) idleVMs(class string) int {
	if w.idle != nil {
		return w.idle(class)
	}
	if w.backend == nil {
		return 0
	}
	return w.backend.Idle(class)
}

func (w *Worker) report() Report {
	w.mu.Lock()
	b := w.backend
	w.mu.Unlock()
	var caches []string
	if b != nil {
		caches = slices.Sorted(maps.Keys(b.HeldCaches())) // off the lock: it reads the disk
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	r := Report{Room: w.roomLocked(), UsedCPUs: w.used.CPUs, UsedMemory: w.used.MemoryMB, Checks: map[string]string{}, Caches: caches}
	for name := range w.classes {
		if k := w.idleVMs(name); k > 0 {
			if r.Idle == nil {
				r.Idle = map[string]int{}
			}
			r.Idle[name] = k
		}
	}
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
	r.Update = w.updateReportLocked()
	switch {
	case w.draining:
		r.State = Draining
	case w.drainedLocked():
		r.State, r.Reason = Draining, "updating to "+w.update.status.Target
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
	var turnedAway sync.Map // controller address → logged its refusal
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
					w.takeUpdateOrders(addr, nil)
					var se *StatusError
					if errors.As(err, &se) && se.Code == http.StatusConflict {
						if _, logged := turnedAway.LoadOrStore(addr, true); !logged {
							w.log.Error("the controller turns this worker away", "controller", addr, "error", se.Msg)
						}
					}
					return
				}
				if _, was := turnedAway.LoadAndDelete(addr); was {
					w.log.Info("the controller takes this worker again", "controller", addr)
				}
				w.configure(ctx, resp.Fit)
				w.setDrain(addr, &resp.Draining)
				w.takeUpdateOrders(addr, &resp)
				w.takeVerify(ctx, resp.Verify)
				w.reached()
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
func (w *Worker) reconcile(ctx context.Context, by string, list []Assignment) {
	w.mu.Lock()
	if w.lists == nil {
		w.lists = map[string]time.Time{}
	}
	w.lists[by] = time.Now()
	w.mu.Unlock()
	for _, a := range list {
		w.mu.Lock()
		t := w.tasks[a.ID]
		if t != nil {
			t.listed, t.by = time.Now(), by
		}
		if t == nil {
			if a.Cancel {
				w.mu.Unlock()
				continue
			}
			t = &task{a: a, status: AssignmentStatus{ID: a.ID, Class: a.Class, Phase: PhasePending, Since: time.Now(), Created: a.Created}, listed: time.Now(), by: by}
			w.tasks[a.ID] = t
			// The room is taken with the task, so no report has one
			// without the other: the controllers would place another VM
			// in the room this one is about to take.
			s, err := w.takeLocked(a.Class, false)
			if err != nil {
				reason := ""
				if errors.Is(err, ErrNoRoom) {
					reason = ReasonNoRoom
				}
				t.status.Phase, t.status.Reason, t.status.Err = PhaseFailed, reason, err.Error()
				t.status.Since, t.done = time.Now(), time.Now()
				t.status.Ended = t.status.Since
				w.mu.Unlock()
				w.log.Info("assignment refused", "assignment", a.ID, "class", a.Class, "err", err)
				w.poke()
				continue
			}
			w.mu.Unlock()
			w.poke()
			go w.boot(ctx, t, s)
			continue
		}
		t.a.Cancel = t.a.Cancel || a.Cancel
		if t.a.Cache == nil && a.Cache != nil {
			t.a.Cache = a.Cache
			if t.granted != nil {
				close(t.granted)
			}
		}
		if t.a.Run == nil && a.Run != nil {
			t.a.Run = a.Run
		}
		w.mu.Unlock()
		w.advance(ctx, t)
	}
}

// boot boots the task's VM in the room taken for it.
func (w *Worker) boot(ctx context.Context, t *task, s slot) {
	vm, err := w.bootSlot(ctx, s)
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
	t.status.Booted = t.status.Since
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
	t.status.Ended = t.status.Since
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
	t.granted = make(chan struct{})
	t.status.Phase, t.status.Since = PhaseRunning, time.Now()
	t.status.Started = t.status.Since
	switch {
	case run.GitHub != nil:
		t.status.Job = "github"
	case run.GitLab != nil:
		t.status.Job = "gitlab"
	}
	w.mu.Unlock()
	w.poke()
	log := w.log.With("vm", v.vm.ID, "assignment", t.a.ID)
	var err error
	switch {
	case run.GitHub != nil:
		w.setJob(v, "github")
		err = w.runGitHub(rctx, cancel, t, v, *run.GitHub, log, func(job string) {
			w.mu.Lock()
			v.took, t.status.Took = job, job
			w.mu.Unlock()
			w.poke()
		})
	case run.GitLab != nil && w.GitLab != nil:
		w.setJob(v, "gitlab")
		job := GitLabJob{Job: run.GitLab.Job, Runner: run.GitLab.Runner}
		if run.GitLab.Cache != nil {
			job.Cache, job.CacheNote = w.attachCache(rctx, v, *run.GitLab.Cache, log)
		}
		err = w.GitLab(rctx, v.vm, job)
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

// slot is the room taken for one VM of a class, until the VM is booted in
// it or the boot fails.
type slot struct {
	class string
	size  fleet.Resources
}

// takeLocked takes room for one VM of the class; check, for the microVM
// check, takes it in a class not yet passed.
func (w *Worker) takeLocked(class string, check bool) (slot, error) {
	c, ok := w.classes[class]
	if !ok {
		return slot{}, fmt.Errorf("this worker has no class %q", class)
	}
	room := w.roomLocked()[class]
	if check {
		room = w.fitsLocked(class)
	}
	if room < 1 {
		return slot{}, ErrNoRoom
	}
	s := slot{class: class, size: fleet.Resources{CPUs: c.CPUs, MemoryMB: int64(c.Memory)}}
	w.used = addRes(w.used, s.size, 1)
	w.count[class]++
	return s, nil
}

// createVM takes room for a VM of the class and boots it.
func (w *Worker) createVM(ctx context.Context, class string, check bool) (bootedVM, error) {
	w.mu.Lock()
	s, err := w.takeLocked(class, check)
	w.mu.Unlock()
	if err != nil {
		return bootedVM{}, err
	}
	w.poke()
	return w.bootSlot(ctx, s)
}

// bootSlot boots a VM in the room taken for it, and gives the room back
// when it can't.
func (w *Worker) bootSlot(ctx context.Context, s slot) (bootedVM, error) {
	w.mu.Lock()
	b := w.backend
	w.mu.Unlock()
	undo := func() {
		w.mu.Lock()
		w.used = addRes(w.used, s.size, -1)
		w.count[s.class]--
		w.mu.Unlock()
		w.poke()
	}
	r, err := b.Reserve(s.class)
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
	w.vms[vm.ID] = &workerVM{vm: vm, size: s.size, since: time.Now()}
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
func (w *Worker) runGitHub(ctx context.Context, cancel context.CancelFunc, t *task, v *workerVM, req GitHubRun, log hclog.Logger, took func(string)) error {
	start := github.SSHStart(req.RunnerVersion, req.CacheURL, log)
	var hook sync.WaitGroup
	defer hook.Wait()
	if req.Cache {
		hook.Add(1)
		go func() {
			defer hook.Done()
			w.relayCache(ctx, t, v, log)
		}()
	}
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
	return start(ctx, github.VM{VM: source.VM{ID: v.vm.ID, Class: v.vm.Class, Instance: v.vm.Instance}, Conn: v.vm.Conn, Cache: req.Cache}, req.JITConfig, func(job string) {
		once.Do(func() { close(tookc) })
		took(job)
	})
}

// relayCache is a GitHub job's cache disk: it relays what the job's hook
// says of the job to the leader, which decides the disk, attaches that,
// and answers the hook. A job that ends first leaves it.
func (w *Worker) relayCache(ctx context.Context, t *task, v *workerVM, log hclog.Logger) {
	set := func(hint, note string) {
		w.mu.Lock()
		if hint != "" {
			t.status.JobHint = hint
		}
		if note != "" {
			t.status.CacheNote = note
		}
		w.mu.Unlock()
		w.poke()
	}
	hint, err := github.WaitJobHook(ctx, v.vm.Conn)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("no cache disk", "err", err)
			set("", "No cache disk: "+err.Error())
		}
		return
	}
	set(hint, "")
	w.mu.Lock()
	grant, granted := t.a.Cache, t.granted
	w.mu.Unlock()
	if grant == nil {
		select {
		case <-granted:
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Minute):
			log.Warn("no cache disk: the leader did not say which")
			_ = github.AnswerJobHook(context.WithoutCancel(ctx), v.vm.Conn, "No cache disk: fleetci did not say which")
			set("", "No cache disk: the leader did not say which")
			return
		}
		w.mu.Lock()
		grant = t.a.Cache
		w.mu.Unlock()
	}
	note := cmp.Or(grant.Note, "No cache disk")
	if grant.Scope != nil {
		if _, n := w.attachCache(ctx, v, *grant.Scope, log); n != "" {
			note = n
		}
	}
	if err := github.AnswerJobHook(context.WithoutCancel(ctx), v.vm.Conn, note); err != nil {
		log.Warn("the job's hook got no answer; it goes on after ten minutes", "err", err)
	}
	set("", note)
}

// attachCache gives the VM its job's cache disk, when its class has one.
// It returns the scope attached, or nil, and what to tell the job.
func (w *Worker) attachCache(ctx context.Context, v *workerVM, scope cachedisk.Scope, log hclog.Logger) (*cachedisk.Scope, string) {
	w.mu.Lock()
	b := w.backend
	w.mu.Unlock()
	if b == nil || !b.HasCache(v.vm.ID) {
		return nil, ""
	}
	st, err := b.AttachCache(ctx, v.vm, scope)
	switch {
	case err != nil:
		log.Warn("no cache disk", "err", err)
		return nil, "No cache disk: " + err.Error()
	case st == nil:
		return nil, ""
	}
	w.poke() // the report's caches change
	return &scope, cachedisk.Describe(scope, *st)
}

// idleVM is how long a VM may wait for the job it was made for; one a
// leader made and then lost is removed after it.
const idleVM = 15 * time.Minute

// idleSpare is how long a worker's taskscaler keeps a VM no job took: one
// it booted for a wait that ended before the VM was ready.
const idleSpare = time.Minute

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
		var idle, forgotten []string
		var orphans, unlisted []*task
		w.mu.Lock()
		for id, v := range w.vms {
			if v.job == "" && time.Since(v.since) > idleVM {
				idle = append(idle, id)
				if t := w.tasks[v.assignment]; t != nil {
					orphans = append(orphans, t)
				}
			}
		}
		for _, t := range w.tasks {
			if t.forgotten(w.lists[t.by]) && !slices.Contains(idle, t.status.VM) {
				forgotten = append(forgotten, t.status.VM)
				unlisted = append(unlisted, t)
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
		for i, t := range unlisted {
			w.log.Warn("removing a vm its controller no longer lists, before any job", "vm", forgotten[i], "assignment", t.a.ID, "controller", t.by)
			_ = w.delete(ctx, forgotten[i])
			w.finish(t, "", errors.New("its controller no longer lists it, and gave it no job: did the controller restart?"))
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

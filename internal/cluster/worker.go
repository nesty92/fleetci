package cluster

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
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
// controller and keeps a stream to each; it holds no forge credentials.
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
	checks   map[string]string
	passed   map[string]bool
	used     fleet.Resources
	count    map[string]int
	vms      map[string]*workerVM
	draining bool
	reach    []string
	github   bool
	streams  map[string]*stream // controller address → its stream
	changed  chan struct{}
}

// GitLabJob is a GitLab job a controller placed on this worker.
type GitLabJob struct {
	Job    json.RawMessage // as GitLab gave it
	Runner json.RawMessage // the [[runners]] table, without its token
}

type workerVM struct {
	vm     fleet.VM
	size   fleet.Resources
	since  time.Time
	job    string
	took   string
	cancel context.CancelFunc
}

// NewWorker reads the worker's config and certificate.
func NewWorker(cfg *WorkerConfig, version string, log hclog.Logger) (*Worker, error) {
	p := PKI{Dir: cfg.PKIDir}
	if !p.HasCert() {
		return nil, fmt.Errorf("%s: no certificate; run fleetci join first", p.Dir)
	}
	w := &Worker{cfg: cfg, pki: p, log: log, version: version,
		checks: map[string]string{}, passed: map[string]bool{}, count: map[string]int{},
		vms: map[string]*workerVM{}, streams: map[string]*stream{}, changed: make(chan struct{}, 1)}
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

// Run connects to every controller and serves them until ctx ends, then
// removes the worker's VMs.
func (w *Worker) Run(ctx context.Context) error {
	pool, err := w.pki.Pool()
	if err != nil {
		return err
	}
	tlsCfg := clientTLS(w.pki, pool)
	var wg sync.WaitGroup
	for _, addr := range w.cfg.Controllers {
		wg.Go(func() { w.keepConnected(ctx, addr, tlsCfg) })
	}
	wg.Go(func() { w.reportLoop(ctx) })
	wg.Go(func() { w.renewLoop(ctx) })
	wg.Go(func() { w.sweepLoop(ctx) })
	<-ctx.Done()
	wg.Wait()
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

// keepConnected keeps a stream to one controller, dialling again when it
// breaks.
func (w *Worker) keepConnected(ctx context.Context, addr string, cfg *tls.Config) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := w.connect(ctx, addr, cfg)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		w.log.Warn("controller stream ended; dialling again", "controller", addr, "err", err, "in", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (w *Worker) connect(ctx context.Context, addr string, tlsCfg *tls.Config) error {
	cc, err := dial(addr, tlsCfg)
	if err != nil {
		return err
	}
	defer cc.Close()
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cs, err := cc.NewStream(sctx, &serviceDesc.Streams[0], "/"+serviceName+"/Connect")
	if err != nil {
		return err
	}
	s := newStream(cs)
	if err := s.tell("hello", Hello{Facts: w.facts, Report: w.report()}); err != nil {
		return err
	}
	w.mu.Lock()
	w.streams[addr] = s
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		if w.streams[addr] == s {
			delete(w.streams, addr)
		}
		w.mu.Unlock()
	}()
	w.log.Info("connected to controller", "controller", addr)
	return s.serve(func(m *Msg) { w.handle(ctx, s, m) })
}

// handle answers a controller.
func (w *Worker) handle(ctx context.Context, s *stream, m *Msg) {
	switch m.Kind {
	case "fit":
		var fit Fit
		if err := m.Decode(&fit); err == nil {
			w.configure(ctx, fit)
		}
	case "create":
		var req createReq
		_ = m.Decode(&req)
		resp, err := w.create(ctx, req.Class)
		_ = s.answer(m, resp, err)
	case "delete":
		var req vmReq
		_ = m.Decode(&req)
		_ = s.answer(m, nil, w.delete(ctx, req.VM))
	case "handover":
		_ = s.answer(m, nil, nil)
	case "run-github":
		var req runGitHubReq
		_ = m.Decode(&req)
		_ = s.answer(m, nil, w.runGitHub(ctx, req))
	case "run-gitlab":
		var req runGitLabReq
		_ = m.Decode(&req)
		_ = s.answer(m, nil, w.runGitLab(ctx, req))
	case "drain":
		var d bool
		_ = m.Decode(&d)
		w.mu.Lock()
		w.draining = d
		w.mu.Unlock()
		w.poke()
		if m.ID != 0 {
			_ = s.answer(m, nil, nil)
		}
	default:
		if m.ID != 0 {
			_ = s.answer(m, nil, fmt.Errorf("unknown request %q", m.Kind))
		}
	}
}

// configure builds the worker's classes from the controllers' fit, the
// first time, and runs the microVM check on each.
func (w *Worker) configure(ctx context.Context, fit Fit) {
	w.mu.Lock()
	if w.backend != nil || w.building {
		// Another controller's fit, or the same again.
		w.mu.Unlock()
		return
	}
	w.building = true
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
		r.VMs = append(r.VMs, VMInfo{ID: id, Class: v.vm.Class, Since: v.since, Job: v.job, Took: v.took})
	}
	slices.SortFunc(r.VMs, func(a, b VMInfo) int { return strings.Compare(a.ID, b.ID) })
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

// reportLoop sends the worker's report to every controller when it
// changes and every 5 s: that is also its heartbeat.
func (w *Worker) reportLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.changed:
		}
		w.broadcast("report", w.report())
	}
}

func (w *Worker) broadcast(kind string, body any) {
	w.mu.Lock()
	streams := make([]*stream, 0, len(w.streams))
	for _, s := range w.streams {
		streams = append(streams, s)
	}
	w.mu.Unlock()
	for _, s := range streams {
		_ = s.tell(kind, body)
	}
}

// create takes room for a VM of class and boots it, or answers ErrNoRoom.
func (w *Worker) create(ctx context.Context, class string) (createResp, error) {
	return w.createVM(ctx, class, false)
}

// createVM boots a VM of the class; check, for the microVM check, takes
// room in a class not yet passed.
func (w *Worker) createVM(ctx context.Context, class string, check bool) (createResp, error) {
	w.mu.Lock()
	c, ok := w.classes[class]
	if !ok {
		w.mu.Unlock()
		return createResp{}, fmt.Errorf("this worker has no class %q", class)
	}
	room := w.roomLocked()[class]
	if check {
		room = w.fitsLocked(class)
	}
	if room < 1 {
		w.mu.Unlock()
		return createResp{}, ErrNoRoom
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
			return createResp{}, ErrNoRoom
		}
		return createResp{}, err
	}
	actx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	vm, err := r.Acquire(actx)
	if err != nil {
		undo()
		return createResp{}, err
	}
	w.mu.Lock()
	w.vms[vm.ID] = &workerVM{vm: vm, size: size, since: time.Now()}
	w.mu.Unlock()
	w.poke()
	return createResp{VM: vm.ID, Class: vm.Class, Instance: vm.Instance}, nil
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

// runGitHub starts a just-in-time runner on a VM, detached: the worker
// follows it and removes the VM when it exits, whatever became of the
// controller that placed it.
func (w *Worker) runGitHub(ctx context.Context, req runGitHubReq) error {
	w.mu.Lock()
	v, ok := w.vms[req.VM]
	if !ok {
		w.mu.Unlock()
		return fmt.Errorf("no vm %q", req.VM)
	}
	if v.job != "" {
		w.mu.Unlock()
		return fmt.Errorf("vm %q runs %s already", req.VM, v.job)
	}
	rctx, cancel := context.WithCancel(ctx)
	v.job, v.cancel = "github", cancel
	w.mu.Unlock()
	w.poke()
	log := w.log.With("vm", req.VM)
	start := github.SSHStart(req.RunnerVersion, req.CacheURL, log)
	tookc := make(chan struct{})
	go func() {
		// A runner that takes no job is stopped, as the leader's idle watch
		// would; the leader may be gone.
		idle := req.IdleTimeout + time.Minute
		if req.IdleTimeout == 0 {
			idle = 11 * time.Minute
		}
		select {
		case <-tookc:
		case <-rctx.Done():
		case <-time.After(idle):
			log.Warn("runner got no job; stopping it")
			cancel()
		}
	}()
	go func() {
		var once sync.Once
		err := start(rctx, github.VM{VM: source.VM{ID: v.vm.ID, Class: v.vm.Class, Instance: v.vm.Instance}, Conn: v.vm.Conn}, req.JITConfig, func(job string) {
			once.Do(func() { close(tookc) })
			w.mu.Lock()
			v.took = job
			w.mu.Unlock()
			w.broadcast("event", Event{VM: req.VM, Kind: "took", Job: job})
			w.poke()
		})
		ev := Event{VM: req.VM, Kind: "exit"}
		if err != nil {
			ev.Err = err.Error()
		}
		if ctx.Err() != nil {
			return // the worker is stopping; Run removes the VMs
		}
		log.Info("runner exited; removing its vm", "err", err)
		_ = w.delete(context.WithoutCancel(ctx), req.VM)
		w.broadcast("event", ev)
	}()
	return nil
}

func (w *Worker) runGitLab(ctx context.Context, req runGitLabReq) error {
	if w.GitLab == nil {
		return errors.New("this worker can't run GitLab jobs")
	}
	w.mu.Lock()
	v, ok := w.vms[req.VM]
	if !ok {
		w.mu.Unlock()
		return fmt.Errorf("no vm %q", req.VM)
	}
	if v.job != "" {
		w.mu.Unlock()
		return fmt.Errorf("vm %q runs %s already", req.VM, v.job)
	}
	rctx, cancel := context.WithCancel(ctx)
	v.job, v.cancel = "gitlab", cancel
	w.mu.Unlock()
	w.poke()
	go func() {
		err := w.GitLab(rctx, v.vm, GitLabJob{Job: req.Job, Runner: req.Runner})
		ev := Event{VM: req.VM, Kind: "exit"}
		if err != nil {
			ev.Err = err.Error()
		}
		if ctx.Err() != nil {
			return
		}
		w.log.Info("gitlab job done; removing its vm", "vm", req.VM, "err", err)
		_ = w.delete(context.WithoutCancel(ctx), req.VM)
		w.broadcast("event", ev)
	}()
	return nil
}

// idleVM is how long a VM may wait for the job it was made for; one a
// leader made and then lost is removed after it.
const idleVM = 15 * time.Minute

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
		w.mu.Lock()
		for id, v := range w.vms {
			if v.job == "" && time.Since(v.since) > idleVM {
				idle = append(idle, id)
			}
		}
		w.mu.Unlock()
		for _, id := range idle {
			w.log.Warn("removing a vm no job came for", "vm", id, "after", idleVM.String())
			_ = w.delete(ctx, id)
		}
	}
}

// renewLoop renews the worker's certificate after two thirds of its life,
// over a controller's stream.
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
	w.mu.Lock()
	var s *stream
	for _, x := range w.streams {
		s = x
		break
	}
	w.mu.Unlock()
	if s == nil {
		return errors.New("no controller to renew it with")
	}
	csr, err := w.pki.NewKey()
	if err != nil {
		return err
	}
	var resp renewResp
	rctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := s.ask(rctx, "renew", renewReq{CSR: csr}, &resp); err != nil {
		return err
	}
	if err := w.pki.Install(resp.Cert, nil); err != nil {
		return err
	}
	w.log.Info("certificate renewed")
	return nil
}

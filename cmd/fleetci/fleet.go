package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/cluster"
	"github.com/nesty92/fleetci/internal/fleet"
	"github.com/nesty92/fleetci/internal/github"
	"github.com/nesty92/fleetci/internal/gitlab"
	"github.com/nesty92/fleetci/internal/source"
)

const (
	defaultControllerConfig = "/etc/fleetci/controller.toml"
	defaultWorkerConfig     = "/etc/fleetci/worker.toml"
)

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func newLog(name string) hclog.Logger {
	return hclog.New(&hclog.LoggerOptions{Name: name, Output: os.Stderr, Level: hclog.LevelFromString(cmpEnv("FLEETCI_LOG", "info"))})
}

func cmpEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func controllerConfigFlag(fs *flag.FlagSet) *string {
	return fs.String("config", cmpEnv("FLEETCI_CONTROLLER_CONFIG", defaultControllerConfig), "controller.toml")
}

// initCmd sets up the fleet's first controller: its CA and certificate.
func initCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	path := controllerConfigFlag(fs)
	_ = fs.Parse(args)
	cfg, err := cluster.LoadController(*path)
	if err != nil {
		return err
	}
	if len(cfg.Controller.Advertise) == 0 {
		return fmt.Errorf("%s: [controller] advertise: the addresses workers reach this controller at", cfg.File)
	}
	if err := cluster.Init(cfg, out); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nStart it with: fleetci controller -config %s\nThen add machines: fleetci token create, and fleetci join on each.\n", cfg.File)
	return nil
}

// controllerCmd runs a controller until SIGINT or SIGTERM; the leader
// among them finds jobs and places them on the workers.
func controllerCmd(args []string) error {
	fs := flag.NewFlagSet("controller", flag.ExitOnError)
	path := controllerConfigFlag(fs)
	_ = fs.Parse(args)
	cfg, err := cluster.LoadController(*path)
	if err != nil {
		return err
	}
	log := newLog("fleetci")
	c, err := cluster.NewController(cfg, version, log)
	if err != nil {
		return err
	}
	ctx, stop := signalContext()
	defer stop()
	var held *gitlab.Held
	if cfg.GitLab != nil {
		// The jobs this controller took before it was killed or restarted,
		// and never placed: placed once it leads, failed if it doesn't
		// lead soon.
		held = gitlab.LoadHeld(heldDir(cfg), log.Named("gitlab"))
		go held.FailAfter(ctx, gitlab.ResumeWithin)
	}
	exiting := func() bool { return ctx.Err() != nil }
	c.Sources = func(lctx context.Context, c *cluster.Controller) error { return fleetSources(lctx, c, held, exiting) }
	return c.Run(ctx)
}

// heldDir keeps the GitLab jobs the leader took and has not placed yet.
func heldDir(cfg *cluster.ControllerConfig) string {
	return filepath.Join(cfg.Controller.StateDir, "gitlab-held")
}

// fleetSources are the leader's job sources, over the fleet's capacity.
func fleetSources(ctx context.Context, c *cluster.Controller, held *gitlab.Held, exiting func() bool) error {
	cfg, log := c.Config(), c.Log()
	capacity := c.Capacity()
	var runs []func() error
	if cfg.GitHub != nil {
		gcfg := *cfg.GitHub
		gcfg.NameByJob = true    // one runner per job across leaders: GitHub refuses the second
		gcfg.HandOverIdle = true // a leader stepping down leaves its runners to their workers
		gcfg.Transport, gcfg.QueueWait = c.GitHubMetrics()
		d, err := github.New(ctx, gcfg, capacity, c.GitHubStart(gcfg), log.Named("github"))
		if err != nil {
			return err
		}
		runs = append(runs, func() error { d.Run(ctx); return nil })
	}
	if cfg.GitLab != nil {
		gcfg := *cfg.GitLab
		gcfg.HeldDir, gcfg.Held, gcfg.Exiting = heldDir(cfg), held, exiting
		l, err := gitlab.NewDispatch(ctx, gcfg, capacity, c.GitLabRemote(), log.Named("gitlab"))
		if err != nil {
			return err
		}
		runs = append(runs, func() error { return l.Run(ctx) })
	}
	errs := make([]error, len(runs))
	var wg sync.WaitGroup
	for i, run := range runs {
		wg.Go(func() { errs[i] = run() })
	}
	wg.Wait()
	return errors.Join(errs...)
}

// adminFlags are the flags of the commands run on a controller.
func adminFlags(fs *flag.FlagSet) (pki, addr *string) {
	pki = fs.String("pki", cmpEnv("FLEETCI_PKI", "/etc/fleetci/pki"), "the controller's pki_dir")
	addr = fs.String("controller", cmpEnv("FLEETCI_CONTROLLER", "localhost"), "the controller to ask")
	return pki, addr
}

func tokenCmd(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "create" {
		return errors.New("usage: fleetci token create [-controller-role] [-ttl 1h]")
	}
	fs := flag.NewFlagSet("token create", flag.ExitOnError)
	pki, addr := adminFlags(fs)
	asController := fs.Bool("for-controller", false, "a token for another controller, not a worker")
	ttl := fs.Duration("ttl", time.Hour, "how long the token is good for; it is good for one join")
	_ = fs.Parse(args[1:])
	role := cluster.RoleWorker
	if *asController {
		role = cluster.RoleController
	}
	var resp struct {
		Token       string   `json:"token"`
		Controllers []string `json:"controllers"`
		Pin         string   `json:"pin"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := cluster.Admin(ctx, *pki, *addr, cluster.AdminRequest("token", "", role, *ttl), &resp); err != nil {
		return err
	}
	at := "<controller>"
	if len(resp.Controllers) > 0 {
		at = resp.Controllers[0]
	}
	flagC := ""
	if *asController {
		flagC = " -as-controller -advertise <this machine's address>"
	}
	if *asController || resp.Pin == "" || len(resp.Controllers) == 0 {
		fmt.Fprintf(out, "%s\n\nOn the new machine, as root, good for %s and one join:\n  fleetci join%s -token %s %s\n", resp.Token, *ttl, flagC, resp.Token, at)
		return nil
	}
	fmt.Fprintf(out, "%s\n\nOn the new machine, as root, good for %s and one join: it installs fleetci, joins and\nstarts the worker as a service (fleetci join flags go at the end)\n  %s\n\nOr, with fleetci on it already:\n  fleetci join -service -token %s %s\n",
		resp.Token, *ttl, cluster.InstallLine(at, resp.Pin, resp.Token), resp.Token, at)
	return nil
}

// joinCmd joins this machine to the fleet, then, with -foreground, runs
// it as a worker.
func joinCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	var o cluster.JoinOptions
	fs.StringVar(&o.Token, "token", os.Getenv("FLEETCI_JOIN_TOKEN"), "the join token, from fleetci token create")
	fs.StringVar(&o.ID, "id", "", "this machine's name in the fleet; default its hostname")
	fs.StringVar(&o.Config, "config", defaultWorkerConfig, "worker.toml to write")
	fs.StringVar(&o.PKIDir, "pki", "/etc/fleetci/pki", "where this machine's key and certificate go")
	fs.StringVar(&o.StateDir, "state", "/var/lib/fleetci", "where the VMs' disks and images go")
	fs.StringVar(&o.BinDir, "bin", "/usr/local/bin", "where the binaries from the controller go")
	labels := fs.String("labels", "", "key=value,… matched by the classes' on")
	classes := fs.String("classes", "", "class,…: the only classes this machine runs")
	cpus := fs.Int("cpus", 0, "CPUs for VMs; default all but the host's reserve")
	mem := fs.String("memory", "", "memory for VMs, e.g. 12GB; default all but the host's reserve")
	fs.BoolVar(&o.AsController, "as-controller", false, "join as another controller, with a -for-controller token")
	advertise := fs.String("advertise", "", "addr,…: a joining controller's addresses")
	foreground := fs.Bool("foreground", false, "run the worker after joining, until SIGINT or SIGTERM")
	service := fs.Bool("service", false, "then install and start the worker as the systemd service "+workerService)
	_ = fs.Parse(args)
	if *service && (*foreground || o.AsController) {
		return errors.New("-service is for a worker that runs on its own: not with -foreground or -as-controller")
	}
	if fs.NArg() != 1 || o.Token == "" {
		return errors.New("usage: fleetci join -token <token> [flags] <controller>")
	}
	o.Controller = fs.Arg(0)
	o.Version = version
	if *labels != "" {
		o.Labels = map[string]string{}
		for kv := range strings.SplitSeq(*labels, ",") {
			k, v, _ := strings.Cut(kv, "=")
			o.Labels[k] = v
		}
	}
	if *classes != "" {
		o.Classes = strings.Split(*classes, ",")
	}
	if *advertise != "" {
		o.Advertise = strings.Split(*advertise, ",")
	}
	if *cpus > 0 || *mem != "" {
		o.Host = &fleet.HostConfig{CPUs: *cpus}
		if *mem != "" {
			var s fleet.Size
			if err := s.UnmarshalText([]byte(*mem)); err != nil {
				return fmt.Errorf("-memory: %w", err)
			}
			o.Host.Memory = s
		}
	}
	ctx, stop := signalContext()
	defer stop()
	wc, _, err := cluster.Join(ctx, o, out)
	if err != nil {
		return err
	}
	if o.AsController {
		fmt.Fprintf(out, "\nThis machine is a controller now. Give it the fleet's controller.toml (its own id, advertise\nand peers), then: fleetci controller\n")
		return nil
	}
	if *service {
		if err := installService(systemdDir, filepath.Join(o.BinDir, "fleetci"), o.Config, systemctl, out); err != nil {
			return err
		}
		fmt.Fprintf(out, "\nJoined, and the worker runs as %s: it checks a microVM per class, then\nthe controllers mark it Ready. Its log: journalctl -u %s -f\n", workerService, workerService)
		return nil
	}
	if !*foreground {
		fmt.Fprintf(out, "\nJoined. Run the worker with: fleetci worker -config %s\n(it checks a microVM per class, then the controllers mark it Ready)\n", o.Config)
		return nil
	}
	return runWorker(ctx, wc)
}

func workerCmd(args []string) error {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	path := fs.String("config", cmpEnv("FLEETCI_WORKER_CONFIG", defaultWorkerConfig), "worker.toml, written by fleetci join")
	_ = fs.Parse(args)
	wc, err := cluster.LoadWorker(*path)
	if err != nil {
		return err
	}
	ctx, stop := signalContext()
	defer stop()
	return runWorker(ctx, wc)
}

func runWorker(ctx context.Context, wc *cluster.WorkerConfig) error {
	log := newLog("fleetci")
	w, err := cluster.NewWorker(wc, version, log)
	if err != nil {
		return err
	}
	w.GitLab = func(ctx context.Context, vm fleet.VM, job cluster.GitLabJob) error {
		return gitlab.RunJob(ctx, source.VM{ID: vm.ID, Class: vm.Class, Instance: vm.Instance}, vm.Conn, job.Job, job.Runner, job.Cache, job.CacheNote, log.Named("gitlab"))
	}
	self, err := os.Executable() // before an update replaces it
	if err != nil {
		return err
	}
	err = w.StartTrial()
	if err == nil {
		err = w.Run(ctx)
	}
	if errors.Is(err, cluster.ErrRestart) && ctx.Err() == nil {
		// An update, or a rollback: start the binary now installed, as a
		// worker.
		log.Info("starting fleetci again", "path", self)
		return syscall.Exec(self, []string{self, "worker", "-config", wc.File}, os.Environ())
	}
	return err
}

// fleetCmd is fleetci fleet status|jobs|update-check|verify|drain|undrain|revoke.
func fleetCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: fleetci fleet status|jobs|update-check|verify|drain|undrain|revoke [worker]")
	}
	op := args[0]
	fs := flag.NewFlagSet("fleet "+op, flag.ExitOnError)
	pki, addr := adminFlags(fs)
	asJSON := fs.Bool("json", false, "as JSON")
	var limit *int
	var worker *string
	var failed, checks *bool
	if op == "status" {
		checks = fs.Bool("checks", false, "and each worker's microVM checks")
	}
	if op == "jobs" {
		limit = fs.Int("n", 30, "how many jobs")
		worker = fs.String("worker", "", "only this worker's")
		failed = fs.Bool("failed", false, "only those that failed")
	}
	_ = fs.Parse(args[1:])
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch op {
	case "status":
		var st cluster.Status
		if err := cluster.Admin(ctx, *pki, *addr, cluster.AdminRequest("status", "", "", 0), &st); err != nil {
			return err
		}
		if *asJSON {
			e := json.NewEncoder(out)
			e.SetIndent("", "  ")
			return e.Encode(st)
		}
		printStatus(out, st)
		if *checks {
			printChecks(out, st)
		}
		return nil
	case "jobs":
		result := ""
		if *failed {
			result = cluster.PhaseFailed
		}
		jobs, err := cluster.Jobs(ctx, *pki, *addr, *limit, *worker, result)
		if err != nil {
			return err
		}
		if *asJSON {
			e := json.NewEncoder(out)
			e.SetIndent("", "  ")
			return e.Encode(jobs)
		}
		printJobs(out, jobs)
		return nil
	case "update-check":
		if err := cluster.Admin(ctx, *pki, *addr, cluster.AdminRequest(op, "", "", 0), nil); err != nil {
			return err
		}
		fmt.Fprintln(out, "ok    the workers check for an update at their next report")
		return nil
	case "verify":
		if fs.NArg() > 1 {
			return errors.New("usage: fleetci fleet verify [worker]")
		}
		if err := cluster.Admin(ctx, *pki, *addr, cluster.AdminRequest(op, fs.Arg(0), "", 0), nil); err != nil {
			return err
		}
		who := "the workers run"
		if fs.NArg() == 1 {
			who = fs.Arg(0) + " runs"
		}
		fmt.Fprintf(out, "ok    %s the microVM checks again at their next report; fleetci fleet status -checks shows them\n", who)
		return nil
	case "drain", "undrain", "revoke":
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: fleetci fleet %s <worker>", op)
		}
		if err := cluster.Admin(ctx, *pki, *addr, cluster.AdminRequest(op, fs.Arg(0), "", 0), nil); err != nil {
			return err
		}
		fmt.Fprintf(out, "ok    %s %s\n", op, fs.Arg(0))
		return nil
	}
	return fmt.Errorf("unknown fleet command %q", op)
}

func printStatus(out io.Writer, st cluster.Status) {
	var ctl []string
	for _, id := range slices.Sorted(maps.Keys(st.Controllers)) {
		s := id
		if v := st.Versions[id]; v != "" {
			s += " " + v
		}
		if id == st.Leader {
			s += " (leader)"
		}
		if time.Since(st.Controllers[id]) > 10*time.Second {
			s += " (gone " + time.Since(st.Controllers[id]).Round(time.Second).String() + ")"
		}
		ctl = append(ctl, s)
	}
	fmt.Fprintf(out, "controllers  %s\n\n", strings.Join(ctl, ", "))
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WORKER\tVERSION\tSTATE\tBUDGET\tUSED\tCLASSES (room)\tVMS\tNOTE")
	for _, w := range st.Workers {
		var cls []string
		for _, c := range w.Fit.Classes {
			cls = append(cls, fmt.Sprintf("%s(%d)", c.Name, w.Report.Room[c.Name]))
		}
		var jobs []string
		for _, v := range w.Report.VMs {
			j := v.Job
			if v.Took != "" {
				j += " " + v.Took
			}
			jobs = append(jobs, strings.TrimSpace(v.Class+" "+j))
		}
		var notes []string
		if v := st.Versions[st.Controller]; v != "" && w.Facts.Version != "" && w.Facts.Version != v {
			notes = append(notes, "not on the controller's "+v)
		}
		if w.Report.Reason != "" {
			notes = append(notes, w.Report.Reason)
		}
		if u := w.Report.Update; u != nil {
			s := "update " + strings.TrimSpace(u.State+" "+u.Target)
			if u.Err != "" {
				s += ": " + u.Err
			}
			notes = append(notes, s)
		}
		for _, c := range slices.Sorted(maps.Keys(w.Report.Idle)) {
			notes = append(notes, fmt.Sprintf("%d %s idle", w.Report.Idle[c], c))
		}
		for _, n := range slices.Sorted(maps.Keys(w.Fit.Refused)) {
			notes = append(notes, "no "+n+": "+w.Fit.Refused[n])
		}
		note := strings.Join(notes, "; ")
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d cpu %s\t%d cpu %s\t%s\t%s\t%s\n", w.ID, w.Facts.Version, w.State,
			w.Facts.CPUs, fleet.Size(w.Facts.MemoryMB), w.Report.UsedCPUs, fleet.Size(w.Report.UsedMemory),
			strings.Join(cls, " "), strings.Join(jobs, ", "), note)
	}
	tw.Flush()
}

// printChecks prints each worker's microVM checks, as it last reported them.
func printChecks(out io.Writer, st cluster.Status) {
	fmt.Fprintln(out)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WORKER\tCLASS\tCHECK")
	for _, w := range st.Workers {
		for _, c := range slices.Sorted(maps.Keys(w.Report.Checks)) {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", w.ID, c, w.Report.Checks[c])
		}
	}
	tw.Flush()
}

// printJobs prints jobs as fleetci fleet jobs does, newest first.
func printJobs(out io.Writer, jobs []cluster.JobRecord) {
	if len(jobs) == 0 {
		fmt.Fprintln(out, "no jobs yet")
		return
	}
	dur := func(d time.Duration) string {
		if d == 0 {
			return "-"
		}
		return d.Round(time.Second).String()
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ENDED\tWORKER\tCLASS\tFORGE\tJOB\tRESULT\tBOOT\tRAN\tERROR")
	for _, j := range jobs {
		took := j.Took
		if took == "" {
			took = "-"
		}
		e := j.Err
		if len(e) > 60 {
			e = e[:57] + "..."
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", j.Ended.Local().Format("01-02 15:04:05"),
			j.Worker, j.Class, j.Forge, took, j.Result, dur(j.Boot()), dur(j.Duration()), e)
	}
	tw.Flush()
}

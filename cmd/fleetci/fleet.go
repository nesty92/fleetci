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
	c.Sources = fleetSources
	ctx, stop := signalContext()
	defer stop()
	return c.Run(ctx)
}

// fleetSources are the leader's job sources, over the fleet's capacity.
func fleetSources(ctx context.Context, c *cluster.Controller) error {
	cfg, log := c.Config(), c.Log()
	capacity := c.Capacity()
	var runs []func() error
	if cfg.GitHub != nil {
		gcfg := *cfg.GitHub
		gcfg.NameByJob = true    // one runner per job across leaders: GitHub refuses the second
		gcfg.HandOverIdle = true // a leader stepping down leaves its runners to their workers
		d, err := github.New(ctx, gcfg, capacity, c.GitHubStart(gcfg), log.Named("github"))
		if err != nil {
			return err
		}
		runs = append(runs, func() error { d.Run(ctx); return nil })
	}
	if cfg.GitLab != nil {
		l, err := gitlab.NewDispatch(ctx, *cfg.GitLab, capacity, c.GitLabRemote(), log.Named("gitlab"))
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
	fmt.Fprintf(out, "%s\n\nOn the new machine, as root, good for %s and one join:\n  fleetci join%s -token %s %s\n", resp.Token, *ttl, flagC, resp.Token, at)
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
	_ = fs.Parse(args)
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
		return gitlab.RunJob(ctx, source.VM{ID: vm.ID, Class: vm.Class, Instance: vm.Instance}, vm.Conn, job.Job, job.Runner, log.Named("gitlab"))
	}
	return w.Run(ctx)
}

// fleetCmd is fleetci fleet status|drain|undrain|revoke.
func fleetCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: fleetci fleet status|drain|undrain|revoke [worker]")
	}
	op := args[0]
	fs := flag.NewFlagSet("fleet "+op, flag.ExitOnError)
	pki, addr := adminFlags(fs)
	asJSON := fs.Bool("json", false, "status as JSON")
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
		note := w.Report.Reason
		if v := st.Versions[st.Controller]; v != "" && w.Facts.Version != "" && w.Facts.Version != v {
			note = strings.TrimSpace("not on the controller's " + v + "; " + note)
			note = strings.TrimSuffix(note, ";")
		}
		if len(w.Fit.Refused) > 0 {
			var r []string
			for _, n := range slices.Sorted(maps.Keys(w.Fit.Refused)) {
				r = append(r, "no "+n+": "+w.Fit.Refused[n])
			}
			note = strings.TrimSpace(note + " " + strings.Join(r, "; "))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d cpu %s\t%d cpu %s\t%s\t%s\t%s\n", w.ID, w.Facts.Version, w.State,
			w.Facts.CPUs, fleet.Size(w.Facts.MemoryMB), w.Report.UsedCPUs, fleet.Size(w.Report.UsedMemory),
			strings.Join(cls, " "), strings.Join(jobs, ", "), note)
	}
	tw.Flush()
}

// Command fleetci sets up and checks a fleetci install:
//
//	fleetci runner-config   writes gitlab-runner's [[runners]] entry for fleetci.toml
//	fleetci check           proves every class, the GitHub access and config.toml work
//	fleetci watchdog        restarts gitlab-runner when its fleetci plugin is gone
//	fleetci serve           runs GitHub Actions jobs on the classes, without gitlab-runner
package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/fleet"
	"github.com/nesty92/fleetci/internal/fleetci"
	"github.com/nesty92/fleetci/internal/github"
)

const defaultConfig = "/etc/fleetci/fleetci.toml"

// Set at release build time.
var (
	version  = "dev"
	revision = ""
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "runner-config":
		err = runnerConfig(os.Args[2:], os.Stdout)
	case "ci-include":
		err = ciInclude(os.Args[2:], os.Stdout)
	case "check":
		var ok bool
		ok, err = check(os.Args[2:], os.Stdout)
		if err == nil && !ok {
			os.Exit(1)
		}
	case "serve":
		err = serve(os.Args[2:])
	case "watchdog":
		err = watchdogCmd(os.Args[2:])
	case "version", "--version":
		fmt.Println("fleetci", version, revision)
		return
	case "-h", "--help", "help":
		usage(os.Stdout)
		return
	default:
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleetci:", err)
		os.Exit(1)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  fleetci runner-config [flags]   print gitlab-runner's [[runners]] entry for fleetci.toml
  fleetci check [flags]           check fleetci.toml, every class, GitHub access and config.toml
  fleetci ci-include [flags]      print a GitLab CI include with one hidden job per class
  fleetci watchdog [flags]        restart gitlab-runner when its fleetci plugin is gone
  fleetci serve [flags]           run GitHub Actions jobs on the classes, without gitlab-runner
  fleetci version

Run "fleetci <command> -h" for its flags. Both read fleetci.toml from -config,
$FLEETCI_CONFIG or `+defaultConfig+`.
`)
}

func configFlag(fs *flag.FlagSet) *string {
	return fs.String("config", cmp.Or(os.Getenv("FLEETCI_CONFIG"), defaultConfig), "fleetci.toml")
}

func runnerConfig(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("runner-config", flag.ExitOnError)
	path := configFlag(fs)
	var o fleetci.RunnerOptions
	fs.StringVar(&o.URL, "url", "https://gitlab.com", "GitLab URL")
	fs.StringVar(&o.Name, "name", "fleetci", "runner name")
	fs.StringVar(&o.Token, "token", "", "runner authentication token, or a placeholder a setup script replaces")
	tokenFile := fs.String("token-file", "", "file holding the runner authentication token")
	fs.StringVar(&o.Plugin, "plugin", "fleeting-plugin-fleetci", "fleetci's fleeting plugin, a path or a name on the PATH")
	fs.StringVar(&o.Image, "image", "ubuntu:24.04", "job image when a job names none")
	fs.BoolVar(&o.Global, "global", false, "also write concurrent and check_interval, for a config.toml holding only this runner")
	_ = fs.Parse(args)

	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			return err
		}
		o.Token = strings.TrimSpace(string(b))
	}
	if o.Token == "" {
		return fmt.Errorf("-token or -token-file is required")
	}
	c, err := fleetci.Load(*path)
	if err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	o.ConfigFile = c.ConfigFile
	s, err := c.RunnerConfig(o)
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, s)
	return err
}

func ciInclude(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("ci-include", flag.ExitOnError)
	path := configFlag(fs)
	tag := fs.String("tag", "fleetci", "the tag fleetci's runner is registered with")
	extra := fs.Bool("extra", false, "only the classes fleetci's public template lacks, to include after it")
	_ = fs.Parse(args)
	c, err := fleetci.Load(*path)
	if err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if *extra {
		if *tag != fleetci.StandardTag {
			return fmt.Errorf("-extra goes with the public template, whose tag is %s", fleetci.StandardTag)
		}
		_, err = io.WriteString(out, c.CIExtra())
		return err
	}
	_, err = io.WriteString(out, c.CIInclude(*tag))
	return err
}

// check reports each part on its own line and returns false if any failed.
func check(args []string, out io.Writer) (bool, error) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	path := configFlag(fs)
	runnerPath := fs.String("runner-config", "/etc/gitlab-runner/config.toml", `gitlab-runner's config.toml; "" skips it`)
	timeout := fs.Duration("timeout", time.Minute, "time allowed for each class's plugin")
	_ = fs.Parse(args)

	r := &report{w: out, ok: true}
	c, err := fleetci.Load(*path)
	if err != nil {
		r.fail("fleetci.toml", err)
		return false, nil
	}
	if err := c.Validate(); err != nil {
		r.fail("fleetci.toml", err)
		return false, nil
	}
	r.pass("fleetci.toml", fmt.Sprintf("%s: %d classes, default %s", c.ConfigFile, len(c.Classes), c.DefaultClass))
	if budget, reserved, on, _ := fleet.CheckHost(c.Host, c.Classes); on {
		var want fleet.Resources // what the classes' max_instances could ask for at once
		var onHost []string
		for _, name := range slices.Sorted(maps.Keys(c.Classes)) {
			class := c.Classes[name]
			if !class.OnHost() {
				continue
			}
			onHost = append(onHost, name)
			size, _ := class.Size()
			want.CPUs += size.CPUs * class.MaxInstances
			want.MemoryMB += size.MemoryMB * int64(class.MaxInstances)
		}
		r.pass("host", fmt.Sprintf("budget %s for %s; fixed shares %s", budget, strings.Join(onHost, ", "), reserved))
		if want.CPUs > budget.CPUs || want.MemoryMB > budget.MemoryMB {
			r.note(fmt.Sprintf("their max_instances could ask for %s at once: past the budget, jobs wait or overflow", want))
		}
	}

	// Classes in parallel: each starts its plugin and lists its group.
	log := hclog.New(&hclog.LoggerOptions{Level: hclog.Error, Output: os.Stderr})
	names := slices.Sorted(maps.Keys(c.Classes))
	results := make([]func(), len(names))
	groupOf := make(map[string]string, len(names))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), *timeout)
			defer cancel()
			res, err := fleet.CheckClass(ctx, name, c.Classes[name], log)
			label := "class " + name
			if err != nil {
				results[i] = func() { r.fail(label, err) }
				return
			}
			mu.Lock()
			groupOf[name] = res.Group
			mu.Unlock()
			results[i] = func() { r.pass(label, describe(c.Classes[name].Plugin, res)) }
		}()
	}
	wg.Wait()
	for _, show := range results {
		show()
	}
	// Two classes on one group would each remove the other's VMs.
	byGroup := map[string][]string{}
	for _, name := range names {
		if g := groupOf[name]; g != "" {
			byGroup[g] = append(byGroup[g], name)
		}
	}
	for _, g := range slices.Sorted(maps.Keys(byGroup)) {
		if classes := byGroup[g]; len(classes) > 1 {
			r.fail("group "+g, fmt.Errorf("shared by classes %s; each class needs a group of its own", strings.Join(classes, ", ")))
		}
	}

	// Jobs on GitLab may use the public template: say which of its classes
	// this install serves.
	var standard, own []string
	for _, name := range names {
		if slices.Contains(fleetci.StandardClasses, name) {
			standard = append(standard, name)
		} else {
			own = append(own, name)
		}
	}
	msg := fmt.Sprintf("public template: %d of its classes are here (%s)", len(standard), strings.Join(standard, ", "))
	if len(standard) == 0 {
		msg = "public template: none of its classes are here"
	}
	r.pass("gitlab ci", msg)
	if len(own) > 0 {
		r.note(`others (` + strings.Join(own, ", ") + `) need "fleetci ci-include -extra" after it, or "fleetci ci-include" alone`)
	}

	if c.GitHub != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for repo, err := range github.Check(ctx, *c.GitHub) {
			label := "github " + cmp.Or(repo, "config")
			if err != nil {
				r.fail(label, err)
			} else {
				r.pass(label, "token can list jobs and runners")
			}
		}
	}

	if *runnerPath != "" {
		findings, err := c.CheckRunnerConfig(*runnerPath)
		switch {
		case err != nil:
			r.fail("config.toml", err)
		case len(findings) > 0:
			for _, f := range findings {
				r.fail("config.toml", fmt.Errorf("%s", f))
			}
			r.note(`"fleetci runner-config" writes an entry with all of these`)
		default:
			r.pass("config.toml", *runnerPath+": fleetci's [[runners]] entry is complete")
		}
	}
	return r.ok, nil
}

func describe(plugin string, res fleet.CheckResult) string {
	total := 0
	var states []string
	for _, st := range slices.Sorted(maps.Keys(res.Instances)) {
		total += res.Instances[st]
		states = append(states, fmt.Sprintf("%d %s", res.Instances[st], st))
	}
	s := fmt.Sprintf("plugin %s works; group %s has %d instances", plugin, res.Group, total)
	if total > 0 {
		s += " (" + strings.Join(states, ", ") + ")"
	}
	return s
}

type report struct {
	w  io.Writer
	ok bool
}

func (r *report) pass(what, msg string) { fmt.Fprintf(r.w, "ok    %-32s %s\n", what, msg) }
func (r *report) note(msg string)       { fmt.Fprintf(r.w, "      %-32s %s\n", "", msg) }
func (r *report) fail(what string, err error) {
	r.ok = false
	fmt.Fprintf(r.w, "FAIL  %-32s %v\n", what, err)
}

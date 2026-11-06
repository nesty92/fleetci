// Package gitlab runs GitLab CI jobs on fleetci's classes in fleetci's own
// process, with gitlab-runner's job loop and Docker executor as a library.
//
// Each class has a runner of its own, registered with the class's name as
// its tag. fleetci asks GitLab for a class's job only when it has room for
// one more of the class's VMs, so jobs it can't run yet stay pending in
// GitLab, where another runner may take them.
//
// The runners of a gitlab-runner config.toml run here too, with all their
// settings: each job's FLEETCI_CLASS picks its class, as with fleetci's
// plugin, and the runner's autoscaler table is ignored.
package gitlab

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/hashicorp/go-hclog"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/spec"
	"gitlab.com/gitlab-org/gitlab-runner/executors"
	"gitlab.com/gitlab-org/gitlab-runner/executors/docker"
	"gitlab.com/gitlab-org/gitlab-runner/network"
	"gitlab.com/gitlab-org/gitlab-runner/router"

	"github.com/nesty92/fleetci/internal/fleet"
)

// RunnerVersion is the gitlab-runner release fleetci's copy of the job loop
// and its go.mod pin follow; it is the version fleetci reports to GitLab.
const RunnerVersion = "19.5.2"

// runnerRevision is RunnerVersion's commit.
const runnerRevision = "66ad5002"

// Config is fleetci.toml's [gitlab] table.
type Config struct {
	// URL is the GitLab instance. Default https://gitlab.com.
	URL string `json:"url" toml:"url"`
	// Concurrent caps the jobs run at once. Default: what the runners'
	// classes, and their overflows, hold at once.
	Concurrent int `json:"concurrent" toml:"concurrent"`
	// CheckInterval is how often each runner asks GitLab for a job when
	// it has room. Default 3s.
	CheckInterval fleet.Duration `json:"check_interval" toml:"check_interval"`
	// Runners are the classes' runners, one per class.
	Runners []Runner `json:"runners" toml:"runners"`
	// ConfigFile is a gitlab-runner config.toml whose runners run here, each
	// job on the class its FLEETCI_CLASS names. Its global settings
	// (concurrent, check_interval, shutdown_timeout...) apply unless set
	// above.
	ConfigFile string `json:"config_file" toml:"config_file"`
}

// Runner is one class's GitLab runner.
type Runner struct {
	// Class is the class whose jobs the runner takes.
	Class string `json:"class" toml:"class"`
	// TokenFile holds the runner's authentication token (glrt-...).
	TokenFile string `json:"token_file" toml:"token_file"`
	// Image is the job image when a job names none. Default ubuntu:24.04.
	Image string `json:"image" toml:"image"`
}

// Validate checks c against the classes.
func (c Config) Validate(classes map[string]fleet.Class) error {
	if len(c.Runners) == 0 && c.ConfigFile == "" {
		return errors.New("gitlab: no runners and no config_file")
	}
	seen := map[string]bool{}
	for i, r := range c.Runners {
		where := fmt.Sprintf("gitlab: runner %d", i+1)
		if _, ok := classes[r.Class]; !ok {
			return fmt.Errorf("%s: no class %q", where, r.Class)
		}
		if seen[r.Class] {
			return fmt.Errorf("%s: class %q has a runner already", where, r.Class)
		}
		seen[r.Class] = true
		if r.TokenFile == "" {
			return fmt.Errorf("%s: token_file is required", where)
		}
	}
	return nil
}

// Loop is the job loop of the configured runners.
type Loop struct {
	mr  *RunCommand
	log hclog.Logger
}

// New builds the loop: the runners' configs, the GitLab client and the
// executor provider over vms.
func New(c Config, classes map[string]fleet.Class, defaultClass string, vms VMs, log hclog.Logger) (*Loop, error) {
	if err := c.Validate(classes); err != nil {
		return nil, err
	}
	// gitlab-runner fills its version from the binary's build info, which
	// here is fleetci's; GitLab and the job log should see the runner's.
	common.AppVersion.Version = RunnerVersion
	common.AppVersion.Revision = runnerRevision

	systemID, err := systemID()
	if err != nil {
		return nil, err
	}

	classOf := map[*common.RunnerConfig]string{}
	config := &common.Config{}
	if c.ConfigFile != "" {
		// As gitlab-runner reads it: concurrent is 1 when the file lacks it.
		config = common.NewConfig()
		if err := config.LoadConfig(c.ConfigFile); err != nil {
			return nil, fmt.Errorf("gitlab: %s: %w", c.ConfigFile, err)
		}
		if !config.Loaded {
			return nil, fmt.Errorf("gitlab: %s: no such file", c.ConfigFile)
		}
		for _, rc := range config.Runners {
			rc.Executor = executorName
			if rc.Docker == nil {
				rc.Docker = &common.DockerConfig{Image: "ubuntu:24.04"}
			}
			rc.SystemID = cmp.Or(rc.SystemID, systemID)
		}
	}
	if c.Concurrent != 0 {
		config.Concurrent = c.Concurrent
	}
	if c.CheckInterval != 0 {
		config.CheckInterval = int(max(1, int64(c.CheckInterval)/1e9))
	}
	if config.CheckInterval == 0 {
		config.CheckInterval = 3
	}
	fromFile := len(config.Runners) > 0
	concurrent := 0
	for _, r := range c.Runners {
		token, err := os.ReadFile(r.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("gitlab: runner of %s: %w", r.Class, err)
		}
		rc := &common.RunnerConfig{
			Name: "fleetci " + r.Class,
			RunnerCredentials: common.RunnerCredentials{
				URL:   cmp.Or(c.URL, "https://gitlab.com"),
				Token: strings.TrimSpace(string(token)),
			},
			RunnerSettings: common.RunnerSettings{
				Executor: executorName,
				Docker:   &common.DockerConfig{Image: cmp.Or(r.Image, "ubuntu:24.04")},
			},
			SystemID: systemID,
		}
		config.Runners = append(config.Runners, rc)
		classOf[rc] = r.Class

		if !fromFile {
			class := classes[r.Class]
			concurrent += class.Slots()
			if o, ok := classes[class.Overflow]; ok {
				concurrent += o.Slots()
			}
		}
	}
	if fromFile {
		// A config.toml runner's jobs may take any class.
		concurrent = 0
		for _, class := range classes {
			concurrent += class.Slots()
		}
	}
	if config.Concurrent == 0 {
		config.Concurrent = concurrent
	}

	names := slices.Sorted(maps.Keys(classes))
	p := newProvider(docker.NewProvider(), vms, func(rc *common.RunnerConfig) string { return classOf[rc] }, names, defaultClass)
	providers := executors.NewProviderRegistry(map[string]common.ExecutorProvider{executorName: p})

	// As gitlab-runner's main.go newClient, without a certificate
	// directory.
	mainClient := network.NewGitLabClient(network.WithExecutorProviderFunc(providers.GetByName))
	rc := router.NewClient(mainClient, "", common.AppVersion.UserAgent())

	mr := &RunCommand{
		network:                rc,
		executorProviders:      providers,
		healthHelper:           newHealthHelper(),
		buildsHelper:           newBuildsHelper(),
		config:                 config,
		inputsMetricsCollector: spec.NewJobInputsMetricsCollector(),
		abortBuilds:            make(chan os.Signal),
		runInterruptSignal:     make(chan os.Signal),
		runFinished:            make(chan bool, 1),
	}
	log.Info("gitlab runners", "runners", len(config.Runners), "concurrent", config.Concurrent, "system_id", systemID, "gitlab_runner", RunnerVersion)
	return &Loop{mr: mr, log: log}, nil
}

// Run takes and runs jobs until ctx ends, then aborts the running ones and
// waits for them up to the shutdown timeout.
func (l *Loop) Run(ctx context.Context) error {
	go l.mr.run()
	<-ctx.Done()
	l.log.Info("stopping: aborting running GitLab jobs")
	return l.mr.Stop()
}

// systemID is this machine's id among a runner's managers, the same on
// every start: gitlab-runner's form, s_ and 12 hex digits.
func systemID() (string, error) {
	host, err := os.Hostname()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte("fleetci " + host))
	return "s_" + hex.EncodeToString(sum[:])[:12], nil
}

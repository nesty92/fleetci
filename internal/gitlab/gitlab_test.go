package gitlab

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/go-hclog"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/nesty92/fleetci/internal/proxy"
	"github.com/nesty92/fleetci/internal/source"
)

// noRoom is a host of classes() that never has room.
type noRoom struct {
	defaultClass string
	reserves     atomic.Int32
}

func (n *noRoom) Classes(context.Context) ([]source.Class, string, error) {
	return classes(), n.defaultClass, nil
}
func (n *noRoom) Reserve(context.Context, string) (source.Reservation, error) {
	n.reserves.Add(1)
	return source.Reservation{}, source.ErrNoRoom
}
func (*noRoom) Acquire(context.Context, source.Reservation) (source.VM, error) {
	return source.VM{}, errors.New("no reservations")
}
func (*noRoom) Release(context.Context, source.Reservation) error {
	return errors.New("no reservations")
}
func (*noRoom) Create(context.Context, string) (source.VM, error) {
	return source.VM{}, source.ErrNoRoom
}
func (*noRoom) HandOver(context.Context, string) bool { return false }
func (*noRoom) Conn(string) (*proxy.Conn, error) {
	return nil, errors.New("no vms")
}
func (*noRoom) Delete(context.Context, string) error { return nil }

// countingNetwork counts job requests and hands out none.
type countingNetwork struct {
	common.Network
	requests atomic.Int32
}

func (n *countingNetwork) RequestJob(context.Context, common.RunnerConfig, *common.SessionInfo) (*spec.Job, bool) {
	n.requests.Add(1)
	return nil, true
}

func classes() []source.Class {
	return []source.Class{
		{Name: "1vcpu-2gb", Slots: 3},
		{Name: "4vcpu-8gb", Slots: 1, Overflow: "4vcpu-8gb-cloud"},
		{Name: "4vcpu-8gb-cloud", Slots: 2},
	}
}

func tokenFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "runner.token")
	if err := os.WriteFile(p, []byte("glrt-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestValidate(t *testing.T) {
	ok := Runner{Class: "1vcpu-2gb", TokenFile: "t"}
	for _, tc := range []struct {
		name string
		c    Config
		want string
	}{
		{"no runners", Config{}, "no runners and no config_file"},
		{"unknown class", Config{Runners: []Runner{{Class: "nope", TokenFile: "t"}}}, `no class "nope"`},
		{"two runners on a class", Config{Runners: []Runner{ok, ok}}, "has a runner already"},
		{"no token", Config{Runners: []Runner{{Class: "1vcpu-2gb"}}}, "token_file is required"},
	} {
		err := tc.c.Validate(source.Names(classes()))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
	if err := (Config{Runners: []Runner{ok}}).Validate(source.Names(classes())); err != nil {
		t.Errorf("valid: %v", err)
	}
}

func TestNewBuildsOneRunnerPerClass(t *testing.T) {
	tok := tokenFile(t)
	l, err := New(t.Context(), Config{Runners: []Runner{
		{Class: "1vcpu-2gb", TokenFile: tok},
		{Class: "4vcpu-8gb", TokenFile: tok, Image: "alpine"},
	}}, &noRoom{defaultClass: "1vcpu-2gb"}, hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	c := l.mr.config
	// 3 of 1vcpu-2gb, 1 of 4vcpu-8gb and 2 of its overflow.
	if c.Concurrent != 6 || c.CheckInterval != 3 {
		t.Errorf("concurrent %d, check_interval %d; want 6, 3", c.Concurrent, c.CheckInterval)
	}
	if len(c.Runners) != 2 {
		t.Fatalf("%d runners", len(c.Runners))
	}
	r := c.Runners[1]
	if r.Token != "glrt-test" || r.URL != "https://gitlab.com" || r.Executor != executorName ||
		r.Docker.Image != "alpine" || !strings.HasPrefix(r.SystemID, "s_") || len(r.SystemID) != 14 {
		t.Errorf("runner %+v", r)
	}
	if c.Runners[0].Docker.Image != "ubuntu:24.04" {
		t.Errorf("default image %q", c.Runners[0].Docker.Image)
	}
	if common.AppVersion.Version != RunnerVersion {
		t.Errorf("reports gitlab-runner %q, want %s", common.AppVersion.Version, RunnerVersion)
	}
}

// A runner whose class has no room asks GitLab for nothing.
func TestNoRoomRequestsNoJob(t *testing.T) {
	vms := &noRoom{defaultClass: "1vcpu-2gb"}
	l, err := New(t.Context(), Config{Runners: []Runner{{Class: "1vcpu-2gb", TokenFile: tokenFile(t)}}},
		vms, hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	net := &countingNetwork{}
	l.mr.network = net

	runner := l.mr.config.Runners[0]
	err = l.mr.processRunner(1, runner, make(chan *common.RunnerConfig, 1))
	var noFree *common.NoFreeExecutorError
	if !errors.As(err, &noFree) {
		t.Fatalf("processRunner: %v, want NoFreeExecutorError", err)
	}
	if vms.reserves.Load() != 1 || net.requests.Load() != 0 {
		t.Fatalf("%d reservations, %d job requests; want 1, 0", vms.reserves.Load(), net.requests.Load())
	}
}

// gitlab-runner's own config.toml, as "fleetci runner-config" writes it.
const runnerConfigTOML = `concurrent = 4
check_interval = 5

[[runners]]
  name = "fleetci"
  url = "https://gitlab.example.com"
  token = "glrt-file"
  executor = "docker-autoscaler"
  environment = ["A=b"]
  [runners.docker]
    image = "alpine"
    privileged = true
  [runners.cache]
    Type = "s3"
    Shared = true
    [runners.cache.s3]
      ServerAddress = "cache.example.com"
      BucketName = "runner"
  [runners.autoscaler]
    plugin = "fleeting-plugin-fleetci"
    max_instances = 1
`

func TestConfigFileRunnersKeepTheirSettings(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(file, []byte(runnerConfigTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := New(t.Context(), Config{ConfigFile: file, Runners: []Runner{{Class: "4vcpu-8gb", TokenFile: tokenFile(t)}}}, &noRoom{defaultClass: "1vcpu-2gb"}, hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	c := l.mr.config
	if c.Concurrent != 4 || c.CheckInterval != 5 || len(c.Runners) != 2 {
		t.Fatalf("concurrent %d, check_interval %d, %d runners; want the file's 4, 5 and 2 runners",
			c.Concurrent, c.CheckInterval, len(c.Runners))
	}
	r := c.Runners[0]
	if r.Executor != executorName || r.Token != "glrt-file" || r.URL != "https://gitlab.example.com" ||
		r.Docker.Image != "alpine" || !r.Docker.Privileged || r.Cache == nil || r.Cache.S3 == nil ||
		r.Cache.S3.BucketName != "runner" || len(r.Environment) != 1+len(cacheCompression) || r.Environment[0] != "A=b" || r.SystemID == "" {
		t.Errorf("config.toml runner lost settings: %+v", r)
	}
	p := l.mr.executorProviders.GetByName(executorName).(*provider)
	if p.classOf(r) != "" || p.classOf(c.Runners[1]) != "4vcpu-8gb" {
		t.Errorf("classes %q %q; want the job's, then 4vcpu-8gb", p.classOf(r), p.classOf(c.Runners[1]))
	}
}

func TestJobClass(t *testing.T) {
	p := newProvider(nil, capacityVMs{&noRoom{}}, nil, []string{"1vcpu-2gb", "4vcpu-8gb"}, "1vcpu-2gb")
	build := func(class string) *common.Build {
		b := &common.Build{Runner: &common.RunnerConfig{}}
		if class != "" {
			b.Variables = spec.Variables{{Key: classVariable, Value: class}}
		}
		return b
	}
	if got, err := p.jobClass(build("")); got != "1vcpu-2gb" || err != nil {
		t.Errorf("unset: %q %v, want the default", got, err)
	}
	if got, err := p.jobClass(build("4vcpu-8gb")); got != "4vcpu-8gb" || err != nil {
		t.Errorf("4vcpu-8gb: %q %v", got, err)
	}
	_, err := p.jobClass(build("nope"))
	var be *common.BuildError
	if !errors.As(err, &be) || be.FailureReason != common.ConfigurationError ||
		!strings.Contains(err.Error(), "1vcpu-2gb (default), 4vcpu-8gb") {
		t.Errorf("unknown class: %v", err)
	}
}

// A config.toml runner asks GitLab for nothing while no class has room.
func TestAFullHostRequestsNoJob(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(file, []byte(runnerConfigTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	vms := &noRoom{defaultClass: "1vcpu-2gb"}
	l, err := New(t.Context(), Config{ConfigFile: file}, vms, hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	net := &countingNetwork{}
	l.mr.network = net
	err = l.mr.processRunner(1, l.mr.config.Runners[0], make(chan *common.RunnerConfig, 1))
	var noFree *common.NoFreeExecutorError
	if !errors.As(err, &noFree) {
		t.Fatalf("processRunner: %v, want NoFreeExecutorError", err)
	}
	if n := vms.reserves.Load(); n != int32(len(classes())) || net.requests.Load() != 0 {
		t.Fatalf("%d reservations tried, %d job requests; want every class, 0", n, net.requests.Load())
	}
}

func TestCacheCompressionDefaults(t *testing.T) {
	got := withCacheCompression(nil)
	if strings.Join(got, " ") != strings.Join(cacheCompression, " ") {
		t.Fatalf("empty environment: %v", got)
	}
	// The runner's own value stays, the others are added.
	got = withCacheCompression([]string{"CACHE_COMPRESSION_LEVEL=slowest", "FOO=1"})
	want := "CACHE_COMPRESSION_LEVEL=slowest FOO=1 FF_USE_FASTZIP=true CACHE_COMPRESSION_FORMAT=zipzstd"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %v, want %s", got, want)
	}
}

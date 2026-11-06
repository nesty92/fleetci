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

	"github.com/nesty92/fleetci/internal/fleet"
)

// noRoom is a host that never has room.
type noRoom struct{ reserves atomic.Int32 }

func (n *noRoom) Reserve(string) (*fleet.Reservation, error) {
	n.reserves.Add(1)
	return nil, fleet.ErrNoRoom
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

func classes() map[string]fleet.Class {
	return map[string]fleet.Class{
		"1vcpu-2gb":       {MaxInstances: 3},
		"4vcpu-8gb":       {MaxInstances: 1, Overflow: "4vcpu-8gb-cloud"},
		"4vcpu-8gb-cloud": {MaxInstances: 2},
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
		{"no runners", Config{}, "no runners"},
		{"unknown class", Config{Runners: []Runner{{Class: "nope", TokenFile: "t"}}}, `no class "nope"`},
		{"two runners on a class", Config{Runners: []Runner{ok, ok}}, "has a runner already"},
		{"no token", Config{Runners: []Runner{{Class: "1vcpu-2gb"}}}, "token_file is required"},
	} {
		err := tc.c.Validate(classes())
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
	if err := (Config{Runners: []Runner{ok}}).Validate(classes()); err != nil {
		t.Errorf("valid: %v", err)
	}
}

func TestNewBuildsOneRunnerPerClass(t *testing.T) {
	tok := tokenFile(t)
	l, err := New(Config{Runners: []Runner{
		{Class: "1vcpu-2gb", TokenFile: tok},
		{Class: "4vcpu-8gb", TokenFile: tok, Image: "alpine"},
	}}, classes(), &noRoom{}, hclog.NewNullLogger())
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
	vms := &noRoom{}
	l, err := New(Config{Runners: []Runner{{Class: "1vcpu-2gb", TokenFile: tokenFile(t)}}},
		classes(), vms, hclog.NewNullLogger())
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

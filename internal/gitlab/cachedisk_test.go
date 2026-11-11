package gitlab

import (
	"testing"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/nesty92/fleetci/internal/cachedisk"
)

func TestCacheScope(t *testing.T) {
	protected := true
	build := &common.Build{
		Job: spec.Job{
			JobInfo:   spec.JobInfo{ProjectID: 42},
			GitInfo:   spec.GitInfo{Ref: "main", Protected: &protected},
			Variables: spec.Variables{{Key: "CI_DEFAULT_BRANCH", Value: "main", Public: true}},
		},
		Runner: &common.RunnerConfig{RunnerCredentials: common.RunnerCredentials{URL: "https://gitlab.example.com/"}},
	}
	got, err := cacheScope(build)
	if err != nil {
		t.Fatal(err)
	}
	want := cachedisk.Scope{Forge: "gitlab", Instance: "gitlab.example.com", Project: "42", Ref: "main", Protected: true, Default: "main"}
	if got != want {
		t.Fatalf("scope %+v, want %+v", got, want)
	}
	build.Runner.URL = "not a url"
	if _, err := cacheScope(build); err == nil {
		t.Fatal("a runner without a host made a scope")
	}
}

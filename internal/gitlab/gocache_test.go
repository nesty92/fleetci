package gitlab

import (
	"crypto/ed25519"
	"path/filepath"
	"testing"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/nesty92/fleetci/internal/jobtoken"
)

func TestGoCacheToken(t *testing.T) {
	dir := t.TempDir()
	priv, pub := filepath.Join(dir, "key"), filepath.Join(dir, "key.pub")
	if err := jobtoken.GenerateKey(priv, pub); err != nil {
		t.Fatal(err)
	}
	g, err := newGoCache(&GoCache{URL: "http://10.0.1.5:8091", KeyFile: priv})
	if err != nil {
		t.Fatal(err)
	}
	protected := false
	build := &common.Build{
		Job: spec.Job{
			ID:        7,
			JobInfo:   spec.JobInfo{ProjectID: 42},
			GitInfo:   spec.GitInfo{Ref: "feature", Protected: &protected},
			Variables: spec.Variables{{Key: "FLEETCI_CLASS", Value: "small", Public: true}},
		},
		Runner: &common.RunnerConfig{RunnerCredentials: common.RunnerCredentials{URL: "https://gitlab.example.com/"}},
	}
	// The class is read first, which caches the variables.
	if build.GetAllVariables().Value("FLEETCI_CLASS") != "small" {
		t.Fatal("no class")
	}
	now := time.Unix(1_800_000_000, 0)
	if err := g.addTo(build, now); err != nil {
		t.Fatal(err)
	}
	if err := g.addTo(build, now); err != nil { // Prepare again: no second token
		t.Fatal(err)
	}
	vars := build.GetAllVariables()
	if vars.Value(goCacheURLVariable) != "http://10.0.1.5:8091" {
		t.Errorf("url = %q", vars.Value(goCacheURLVariable))
	}
	n := 0
	for _, v := range build.Variables {
		if v.Key == goCacheTokenVariable {
			n++
			if !v.Masked {
				t.Error("token not masked")
			}
		}
	}
	if n != 1 {
		t.Errorf("%d tokens", n)
	}
	k, _ := jobtoken.LoadPublicKey(pub)
	c, err := jobtoken.Verify([]ed25519.PublicKey{k}, vars.Value(goCacheTokenVariable), now)
	if err != nil {
		t.Fatal(err)
	}
	want := jobtoken.Claims{Forge: "gitlab", Instance: "gitlab.example.com", Project: "42", Ref: "feature", Job: "7",
		Expires: now.Add(build.GetBuildTimeout() + tokenSlack).Unix()}
	if c != want {
		t.Errorf("claims = %+v, want %+v", c, want)
	}

	if g, err := newGoCache(nil); g != nil || err != nil {
		t.Errorf("no table: %v, %v", g, err)
	}
	if err := (*goCache)(nil).addTo(build, now); err != nil {
		t.Error(err)
	}
	if _, err := newGoCache(&GoCache{URL: "10.0.1.5:8091", KeyFile: priv}); err == nil {
		t.Error("url without scheme accepted")
	}
}

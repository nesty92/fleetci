package gitlab

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/nesty92/fleetci/internal/jobtoken"
)

// GoCache is the [gitlab.go_cache] table: fleetci's shared Go build cache
// (cmd/fleetci-gocache). Each job gets FLEETCI_GOCACHE_URL and a token,
// FLEETCI_GOCACHE_TOKEN, signed with the key, which says what fleetci
// knows of the job: its project, its ref and whether the ref is protected.
// The job's container runs the client as GOCACHEPROG, which the runner's
// config.toml sets up (see the README).
type GoCache struct {
	// URL is the cache server, as jobs reach it.
	URL string `json:"url" toml:"url"`
	// KeyFile is the private key from fleetci-gocache keygen.
	KeyFile string `json:"key_file" toml:"key_file"`
}

// Variables jobs get.
const (
	goCacheURLVariable   = "FLEETCI_GOCACHE_URL"
	goCacheTokenVariable = "FLEETCI_GOCACHE_TOKEN"
)

// goCache signs jobs' tokens.
type goCache struct {
	url string
	key ed25519.PrivateKey
}

func newGoCache(c *GoCache) (*goCache, error) {
	if c == nil {
		return nil, nil
	}
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("gitlab: go_cache url %q: want an http(s) URL", c.URL)
	}
	if c.KeyFile == "" {
		return nil, errors.New("gitlab: go_cache key_file is required")
	}
	key, err := jobtoken.LoadPrivateKey(c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("gitlab: go_cache: %w", err)
	}
	return &goCache{url: c.URL, key: key}, nil
}

// tokenSlack keeps a job's token valid a little past the job's timeout,
// for its after_script and cache upload.
const tokenSlack = 10 * time.Minute

// addTo gives the build the cache's URL and its token. The claims come
// from GitLab's job payload, which the job's own code can't change.
func (g *goCache) addTo(build *common.Build, now time.Time) error {
	if g == nil || build.Variables.Get(goCacheTokenVariable) != "" {
		return nil
	}
	u, err := url.Parse(build.Runner.URL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("go cache: the runner's url %q", build.Runner.URL)
	}
	tok, err := jobtoken.Sign(g.key, jobtoken.Claims{
		Forge:     "gitlab",
		Instance:  u.Host,
		Project:   strconv.FormatInt(build.JobInfo.ProjectID, 10),
		Ref:       build.GitInfo.Ref,
		Protected: build.IsProtected(),
		Job:       strconv.FormatInt(build.ID, 10),
		Expires:   now.Add(build.GetBuildTimeout() + tokenSlack).Unix(),
	})
	if err != nil {
		return err
	}
	build.Variables = append(build.Variables,
		spec.Variable{Key: goCacheURLVariable, Value: g.url, Public: true},
		spec.Variable{Key: goCacheTokenVariable, Value: tok, Masked: true},
	)
	build.RefreshAllVariables()
	return nil
}

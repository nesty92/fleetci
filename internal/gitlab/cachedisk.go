package gitlab

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/nesty92/fleetci/internal/cachedisk"
)

// CacheDisk is the [gitlab.cache_disk] table: which jobs write the caches
// the rest of their project reads, for classes with a cache disk (see
// fleet.CacheDisk and cachedisk.Policy). Projects match by path.
type CacheDisk = cachedisk.Policy

// CacheDiskProject is one [[gitlab.cache_disk.projects]].
type CacheDiskProject = cachedisk.ProjectPolicy

// cacheVariable "off", set by a .gitlab-ci.yml, runs the job without a
// cache disk.
const cacheVariable = "FLEETCI_CACHE"

// gitlabJob is what GitLab's payload says of a job, which its
// .gitlab-ci.yml can't change.
func gitlabJob(build *common.Build) cachedisk.Job {
	ref := build.GitInfo.Ref
	return cachedisk.Job{
		Ref:       ref,
		Tag:       build.GitInfo.RefType == spec.RefTypeTag,
		Protected: build.IsProtected(),
		Default:   ref != "" && ref == gitlabDefaultBranch(build),
		Name:      build.JobInfo.Name,
		Review: slices.ContainsFunc(build.GitInfo.Refspecs, func(s string) bool {
			return strings.Contains(s, "refs/merge-requests/")
		}),
	}
}

// gitlabDefaultBranch is CI_DEFAULT_BRANCH as GitLab set it. A job's own
// variables come after GitLab's in the payload, and the last of a name
// wins in the job, so a job can set its own; fleetci takes the first.
func gitlabDefaultBranch(build *common.Build) string {
	for _, v := range build.Variables {
		if v.Key == "CI_DEFAULT_BRANCH" {
			return v.Value
		}
	}
	return ""
}

// cacheJob is the job's cache disk.
type cacheJob struct {
	scope cachedisk.Scope
}

// cacheFor is the job's cache disk, or nil for none. It is the job's own,
// by its ref and name, and starts from the same job's on the default
// branch.
func cacheFor(c *CacheDisk, build *common.Build) (*cacheJob, error) {
	vars := build.GetAllVariables()
	if v := strings.ToLower(vars.Value(cacheVariable)); v == "off" || v == "false" || v == "0" {
		return nil, nil
	}
	u, err := url.Parse(build.Runner.URL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("the runner's url %q", build.Runner.URL)
	}
	req := cachedisk.Request{
		Forge:    "gitlab",
		Instance: u.Host,
		Project:  strconv.FormatInt(build.JobInfo.ProjectID, 10),
		Path:     build.JobInfo.ProjectFullPath,
		Job:      gitlabJob(build),
	}
	if d := gitlabDefaultBranch(build); d != "" {
		req.From = []string{d}
	}
	return &cacheJob{scope: c.Scope(req)}, nil
}

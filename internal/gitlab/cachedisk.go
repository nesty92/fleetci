package gitlab

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/nesty92/fleetci/internal/cachedisk"
)

// CacheDisk is the [gitlab.cache_disk] table: which jobs write the caches
// the rest of their project reads, for classes with a cache disk (see
// fleet.CacheDisk). It is fleetci's, not the jobs': a job's .gitlab-ci.yml
// can choose its caches' names and what to keep, but not who it trusts.
type CacheDisk struct {
	// Writers are the jobs that write the project's shared caches; other
	// jobs get caches of their ref's own, which start from the shared
	// ones. A job is a writer when any rule matches it:
	//
	//	"protected"       a job on a protected branch or tag (the default)
	//	"default"         a job on the default branch, protected or not
	//	"branch:<glob>"   a job on a branch that matches, "release/*" say
	//	"tag:<glob>"      a job on a tag that matches, "v*" say
	//	"all"             every job, merge requests' too
	//
	// A merge request's pipeline matches only "protected" when GitLab
	// protects it, and "all". "all" lets any branch change what the
	// default branch's jobs run with: use it only where every pushed
	// branch is trusted. Writers = [] makes no job a writer.
	Writers *[]string `json:"writers" toml:"writers"`
	// Projects set Writers for some projects: the first whose Match fits
	// the project's path applies.
	Projects []CacheDiskProject `json:"projects" toml:"projects"`
}

// CacheDiskProject is one [[gitlab.cache_disk.projects]].
type CacheDiskProject struct {
	// Match is a project's path, "group/app", a glob, "group/*", or a
	// group and everything under it, "group/**".
	Match   string   `json:"match" toml:"match"`
	Writers []string `json:"writers" toml:"writers"`
}

// Job variables a .gitlab-ci.yml sets to choose its cache disk.
const (
	// cacheVariable "off" runs the job without one.
	cacheVariable = "FLEETCI_CACHE"
	// cacheKeyVariable names the job's cache; default, its ref. Jobs that
	// share a key share a cache: "deps", "$CI_JOB_NAME", "release".
	cacheKeyVariable = "FLEETCI_CACHE_KEY"
	// cacheFromVariable are the shared caches a cache with nothing yet
	// starts from, the first that exists; default, the default branch's.
	cacheFromVariable = "FLEETCI_CACHE_FROM"
	// cachePathsVariable are paths in the job's container kept on the
	// disk, such as node_modules or .next/cache. Relative ones are in the
	// project's directory.
	cachePathsVariable = "FLEETCI_CACHE_PATHS"
)

var defaultWriters = []string{"protected"}

func (c *CacheDisk) validate() error {
	if c == nil {
		return nil
	}
	check := func(where string, rules []string) error {
		for _, r := range rules {
			if err := checkRule(r); err != nil {
				return fmt.Errorf("gitlab: cache_disk %s: %w", where, err)
			}
		}
		return nil
	}
	if c.Writers != nil {
		if err := check("writers", *c.Writers); err != nil {
			return err
		}
	}
	for i, p := range c.Projects {
		where := fmt.Sprintf("projects %d", i+1)
		if p.Match == "" {
			return fmt.Errorf("gitlab: cache_disk %s: match is required", where)
		}
		if _, err := path.Match(strings.TrimSuffix(p.Match, "/**"), ""); err != nil {
			return fmt.Errorf("gitlab: cache_disk %s: match %q: %w", where, p.Match, err)
		}
		if err := check(where, p.Writers); err != nil {
			return err
		}
	}
	return nil
}

func checkRule(r string) error {
	switch r {
	case "protected", "default", "all":
		return nil
	}
	kind, glob, ok := strings.Cut(r, ":")
	if !ok || (kind != "branch" && kind != "tag") || glob == "" {
		return fmt.Errorf("writer %q: want protected, default, all, branch:<glob> or tag:<glob>", r)
	}
	if _, err := path.Match(glob, ""); err != nil {
		return fmt.Errorf("writer %q: %w", r, err)
	}
	return nil
}

// writers are the rules for a project.
func (c *CacheDisk) writers(project string) []string {
	if c != nil {
		for _, p := range c.Projects {
			if matchProject(p.Match, project) {
				return p.Writers
			}
		}
		if c.Writers != nil {
			return *c.Writers
		}
	}
	return defaultWriters
}

func matchProject(pattern, project string) bool {
	if group, ok := strings.CutSuffix(pattern, "/**"); ok {
		return strings.HasPrefix(project, group+"/")
	}
	ok, _ := path.Match(pattern, project)
	return ok
}

// writer says whether a job writes its project's shared caches. It reads
// only what GitLab's payload says of the job, which its .gitlab-ci.yml
// can't change.
func writer(rules []string, build *common.Build) bool {
	mr := slices.ContainsFunc(build.GitInfo.Refspecs, func(s string) bool {
		return strings.Contains(s, "refs/merge-requests/")
	})
	ref, tag := build.GitInfo.Ref, build.GitInfo.RefType == spec.RefTypeTag
	for _, r := range rules {
		kind, glob, _ := strings.Cut(r, ":")
		switch {
		case r == "all":
			return true
		case r == "protected" && build.IsProtected():
			return true
		case r == "default" && !mr && !tag && ref != "" && ref == gitlabDefaultBranch(build):
			return true
		case kind == "branch" && !mr && !tag && globMatch(glob, ref):
			return true
		case kind == "tag" && tag && globMatch(glob, ref):
			return true
		}
	}
	return false
}

func globMatch(glob, s string) bool {
	ok, _ := path.Match(glob, s)
	return ok
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

// cacheJob is what a job asks of its cache disk.
type cacheJob struct {
	scope cachedisk.Scope
	// paths are the container's paths kept on the disk.
	paths []string
	// inCheckout are those of paths in the project's checkout, from its
	// top, which git clean must leave.
	inCheckout []string
}

// cacheFor is the job's cache disk, or nil for none.
func (c *CacheDisk) cacheFor(build *common.Build, rc *common.RunnerConfig) (*cacheJob, error) {
	vars := build.GetAllVariables()
	if v := strings.ToLower(vars.Value(cacheVariable)); v == "off" || v == "false" || v == "0" {
		return nil, nil
	}
	u, err := url.Parse(build.Runner.URL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("the runner's url %q", build.Runner.URL)
	}
	project := build.JobInfo.ProjectFullPath
	scope := cachedisk.Scope{
		Forge:    "gitlab",
		Instance: u.Host,
		Project:  strconv.FormatInt(build.JobInfo.ProjectID, 10),
		Ref:      build.GitInfo.Ref,
		Key:      strings.TrimSpace(vars.ExpandValue(vars.Value(cacheKeyVariable))),
		Writer:   writer(c.writers(project), build),
		From:     fields(vars.ExpandValue(vars.Value(cacheFromVariable))),
	}
	if len(scope.Key) > 200 {
		return nil, fmt.Errorf("%s is longer than 200 characters", cacheKeyVariable)
	}
	if len(scope.From) == 0 {
		if d := vars.Value("CI_DEFAULT_BRANCH"); d != "" {
			scope.From = []string{d}
		}
	}
	job := &cacheJob{scope: scope}
	dir := projectDir(build, rc)
	for _, p := range fields(vars.ExpandValue(vars.Value(cachePathsVariable))) {
		if !path.IsAbs(p) {
			p = path.Join(dir, p)
		}
		p = path.Clean(p)
		if p == "/" || p == dir || strings.HasPrefix(dir, p+"/") {
			return nil, fmt.Errorf("%s: %q holds the project's checkout", cachePathsVariable, p)
		}
		job.paths = append(job.paths, p)
		if rel, ok := strings.CutPrefix(p, dir+"/"); ok {
			job.inCheckout = append(job.inCheckout, rel)
		}
	}
	return job, nil
}

// keepFromClean has the job's git clean leave its kept paths in the
// checkout, which its default -ffdx would empty after each fetch. It adds
// them to GIT_CLEAN_FLAGS, unless the job set it to none.
func (j *cacheJob) keepFromClean(build *common.Build) {
	if len(j.inCheckout) == 0 {
		return
	}
	flags := strings.TrimSpace(build.GetAllVariables().Value("GIT_CLEAN_FLAGS"))
	switch flags {
	case "none":
		return
	case "":
		flags = "-ffdx"
	}
	for _, rel := range j.inCheckout {
		flags += " -e /" + rel
	}
	build.Variables = append(build.Variables, spec.Variable{Key: "GIT_CLEAN_FLAGS", Value: flags})
	build.RefreshAllVariables()
}

// projectDir is where the Docker executor will check the project out, as
// its Prepare works it out. The cache disk is attached before Prepare, so
// fleetci works it out the same way: GIT_CLONE_PATH, or the builds_dir
// (/builds by default) and the project's path. A builds_dir mounted from
// the host adds the job's concurrency to it; jobs there give absolute
// paths.
func projectDir(build *common.Build, rc *common.RunnerConfig) string {
	if d := build.Settings().GitClonePath; d != "" {
		return path.Clean(d)
	}
	root := "/builds"
	if rc != nil && rc.BuildsDir != "" {
		root = rc.BuildsDir
	}
	return path.Join(root, build.ProjectUniqueDir(false))
}

// fields splits a list on commas and spaces.
func fields(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })
}

// volumes are the job's paths as Docker volumes from the VM's cache disk.
func (j *cacheJob) volumes() []string {
	var out []string
	for _, p := range j.paths {
		h := sha256.Sum256([]byte(p))
		out = append(out, filepath.Join("/cache/paths", hex.EncodeToString(h[:6])+"-"+safeName(path.Base(p)))+":"+p)
	}
	return out
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r == '.' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9') {
			return r
		}
		return '_'
	}, s)
}

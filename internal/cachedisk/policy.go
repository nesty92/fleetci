package cachedisk

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// Policy says which jobs write the caches the rest of their project reads.
// It is fleetci's config, not the jobs': a job may choose what it keeps,
// but not who it trusts. GitLab's and GitHub's sources read it the same
// way.
type Policy struct {
	// Writers are the jobs that write the project's shared caches; other
	// jobs get caches of their ref's own, which start from the shared
	// ones. A job is a writer when any rule matches it:
	//
	//	"protected"       a job on a protected branch or tag (the default)
	//	"default"         a job on the default branch, protected or not
	//	"branch:<glob>"   a job on a branch that matches, "release/*" say
	//	"tag:<glob>"      a job on a tag that matches, "v*" say
	//	"all"             every job, merge and pull requests' too
	//
	// A merge or pull request's job matches only "all", and "protected"
	// where GitLab protects a merge request's pipeline. "all" lets any
	// branch change what the default branch's jobs run with: use it only
	// where every pushed branch is trusted. Writers = [] makes no job a
	// writer.
	Writers *[]string `json:"writers" toml:"writers"`
	// Projects set Writers for some projects: the first whose Match fits
	// the project's path applies.
	Projects []ProjectPolicy `json:"projects" toml:"projects"`
}

// ProjectPolicy is the Writers of the projects Match fits.
type ProjectPolicy struct {
	// Match is a project's path, "group/app", a glob, "group/*", or a
	// group and everything under it, "group/**".
	Match   string   `json:"match" toml:"match"`
	Writers []string `json:"writers" toml:"writers"`
}

// Job is what the forge says of a job, which its own config can't change.
type Job struct {
	// Ref is the branch or tag the job runs on.
	Ref string
	// Tag says Ref is a tag.
	Tag bool
	// Protected says the forge protects Ref.
	Protected bool
	// Default says Ref is the project's default branch.
	Default bool
	// Review says the job is a merge or pull request's.
	Review bool
}

// DefaultWriters are the rules when the config sets none.
var DefaultWriters = []string{"protected"}

// Validate checks the rules and matches.
func (p *Policy) Validate() error {
	if p == nil {
		return nil
	}
	check := func(where string, rules []string) error {
		for _, r := range rules {
			if err := checkRule(r); err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
		}
		return nil
	}
	if p.Writers != nil {
		if err := check("writers", *p.Writers); err != nil {
			return err
		}
	}
	for i, pp := range p.Projects {
		where := fmt.Sprintf("projects %d", i+1)
		if pp.Match == "" {
			return errors.New(where + ": match is required")
		}
		if _, err := path.Match(strings.TrimSuffix(pp.Match, "/**"), ""); err != nil {
			return fmt.Errorf("%s: match %q: %w", where, pp.Match, err)
		}
		if err := check(where, pp.Writers); err != nil {
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

// Rules are the writers' rules for a project.
func (p *Policy) Rules(project string) []string {
	if p != nil {
		for _, pp := range p.Projects {
			if matchProject(pp.Match, project) {
				return pp.Writers
			}
		}
		if p.Writers != nil {
			return *p.Writers
		}
	}
	return DefaultWriters
}

// Writer says whether the project's job writes its shared caches.
func (p *Policy) Writer(project string, j Job) bool {
	return IsWriter(p.Rules(project), j)
}

// IsWriter says whether any of rules matches the job.
func IsWriter(rules []string, j Job) bool {
	for _, r := range rules {
		kind, glob, _ := strings.Cut(r, ":")
		switch {
		case r == "all":
			return true
		case r == "protected" && j.Protected:
			return true
		case r == "default" && !j.Review && !j.Tag && j.Ref != "" && j.Default:
			return true
		case kind == "branch" && !j.Review && !j.Tag && globMatch(glob, j.Ref):
			return true
		case kind == "tag" && j.Tag && globMatch(glob, j.Ref):
			return true
		}
	}
	return false
}

func matchProject(pattern, project string) bool {
	if group, ok := strings.CutSuffix(pattern, "/**"); ok {
		return strings.HasPrefix(project, group+"/")
	}
	return globMatch(pattern, project)
}

func globMatch(glob, s string) bool {
	ok, _ := path.Match(glob, s)
	return ok
}

// Describe says, for the job's log, which cache disk the job got and what
// it starts from.
func Describe(s Scope, st Start) string {
	whose := "this ref's own"
	if s.Writer {
		whose = "shared with the project"
	}
	msg := "Cache disk " + strconv.Quote(s.name()) + " (" + whose + ")"
	switch {
	case st.Empty:
		msg += ": empty, its first job"
	case !st.Own:
		msg += ": starting from " + strconv.Quote(st.From) + "'s"
	}
	return msg
}

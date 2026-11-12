package gitlab

import (
	"slices"
	"strings"
	"testing"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/nesty92/fleetci/internal/cachedisk"
)

// job is a GitLab job on ref, as its payload has it: GitLab's variables
// first, then the job's own.
func job(ref string, opts ...func(*common.Build)) *common.Build {
	b := &common.Build{
		Job: spec.Job{
			JobInfo: spec.JobInfo{ProjectID: 42, ProjectFullPath: "group/app"},
			GitInfo: spec.GitInfo{
				RepoURL:  "https://gitlab.example.com/group/app.git",
				Ref:      ref,
				RefType:  spec.RefTypeBranch,
				Refspecs: []string{"+refs/heads/" + ref + ":refs/remotes/origin/" + ref},
			},
			Variables: spec.Variables{{Key: "CI_DEFAULT_BRANCH", Value: "main", Public: true}},
		},
		Runner: &common.RunnerConfig{RunnerCredentials: common.RunnerCredentials{URL: "https://gitlab.example.com/"}},
	}
	f := false
	b.GitInfo.Protected = &f
	for _, o := range opts {
		o(b)
	}
	return b
}

func protected(b *common.Build) {
	t := true
	b.GitInfo.Protected = &t
}

func tag(b *common.Build) {
	b.GitInfo.RefType = spec.RefTypeTag
	b.GitInfo.Refspecs = []string{"+refs/tags/" + b.GitInfo.Ref + ":refs/tags/" + b.GitInfo.Ref}
}

// mergeRequest makes the job a merge request pipeline's, from its branch.
func mergeRequest(b *common.Build) {
	b.GitInfo.Refspecs = []string{"+refs/merge-requests/7/head:refs/remotes/origin/merge-requests/7/head"}
}

func vars(kv ...string) func(*common.Build) {
	return func(b *common.Build) {
		for i := 0; i < len(kv); i += 2 {
			b.Variables = append(b.Variables, spec.Variable{Key: kv[i], Value: kv[i+1], Public: true})
		}
	}
}

func TestWriter(t *testing.T) {
	jobs := map[string]*common.Build{
		"protected main":   job("main", protected),
		"unprotected main": job("main"),
		"feature":          job("feature"),
		"release branch":   job("release/1.2"),
		"mr on main":       job("main", mergeRequest),
		"tag":              job("v1.2.0", tag),
		"protected tag":    job("v1.2.0", tag, protected),
		"staging tag":      job("staging-42", tag),
		// A job that names itself the default branch is not it.
		"feature as default": job("feature", vars("CI_DEFAULT_BRANCH", "feature")),
	}
	for _, tc := range []struct {
		rules   []string
		writers []string
	}{
		{nil, nil},
		{[]string{"protected"}, []string{"protected main", "protected tag"}},
		{[]string{"default"}, []string{"protected main", "unprotected main"}},
		{[]string{"default", "tag:v*"}, []string{"protected main", "unprotected main", "tag", "protected tag"}},
		{[]string{"branch:release/*"}, []string{"release branch"}},
		{[]string{"tag:staging-*"}, []string{"staging tag"}},
		{[]string{"all"}, []string{"protected main", "unprotected main", "feature", "release branch", "mr on main", "tag", "protected tag", "staging tag", "feature as default"}},
	} {
		var got []string
		for name, b := range jobs {
			if cachedisk.IsWriter(tc.rules, gitlabJob(b)) {
				got = append(got, name)
			}
		}
		slices.Sort(got)
		want := slices.Sorted(slices.Values(tc.writers))
		if !slices.Equal(got, want) {
			t.Errorf("writers %q: %q, want %q", tc.rules, got, want)
		}
	}
}

func TestWritersPerProject(t *testing.T) {
	none := []string{}
	c := &CacheDisk{
		Writers: &none,
		Projects: []CacheDiskProject{
			{Match: "group/app", Writers: []string{"default"}},
			{Match: "group/**", Writers: []string{"tag:v*"}},
			{Match: "other/*", Writers: []string{"all"}},
		},
	}
	for project, want := range map[string][]string{
		"group/app":     {"default"},
		"group/sub/lib": {"tag:v*"},
		"other/x":       {"all"},
		"other/x/y":     {},
		"elsewhere/x":   {},
	} {
		if got := c.Rules(project); !slices.Equal(got, want) {
			t.Errorf("%s: %q, want %q", project, got, want)
		}
	}
	var unset *CacheDisk
	if got := unset.Rules("group/app"); !slices.Equal(got, []string{"protected"}) {
		t.Errorf("default writers %q", got)
	}
}

func TestCacheDiskValidate(t *testing.T) {
	ok := []string{"protected", "default", "all", "branch:release/*", "tag:v*"}
	if err := (&CacheDisk{Writers: &ok, Projects: []CacheDiskProject{{Match: "g/**", Writers: ok}}}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*CacheDisk{
		{Writers: &[]string{"everyone"}},
		{Writers: &[]string{"branch:"}},
		{Writers: &[]string{"tag:[v"}},
		{Projects: []CacheDiskProject{{Writers: ok}}},
		{Projects: []CacheDiskProject{{Match: "g/[", Writers: ok}}},
		{Projects: []CacheDiskProject{{Match: "g/*", Writers: []string{"main"}}}},
	} {
		if c.Validate() == nil {
			t.Errorf("%+v passed", c)
		}
	}
}

func TestCacheFor(t *testing.T) {
	var c *CacheDisk
	j, err := cacheFor(c, job("feature"), nil)
	if err != nil {
		t.Fatal(err)
	}
	s := j.scope
	if s.Instance != "gitlab.example.com" || s.Project != "42" || s.Ref != "feature" || s.Key != "" || s.Writer || !slices.Equal(s.From, []string{"main"}) || len(j.paths) != 0 {
		t.Fatalf("default job: %+v", j)
	}

	b := job("v1.2.0", tag, protected, vars(
		"FLEETCI_CACHE_KEY", "release-$CI_COMMIT_REF_SLUG",
		"FLEETCI_CACHE_FROM", "staging, main",
		"FLEETCI_CACHE_PATHS", "node_modules .next/cache\n/root/.npm",
	))
	b.Variables = append(b.Variables, spec.Variable{Key: "CI_COMMIT_REF_SLUG", Value: "v1-2-0", Public: true})
	j, err = cacheFor(c, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	if j.scope.Key != "release-v1-2-0" || !j.scope.Writer || !slices.Equal(j.scope.From, []string{"staging", "main"}) {
		t.Fatalf("scope %+v", j.scope)
	}
	want := []string{"/builds/group/app/node_modules", "/builds/group/app/.next/cache", "/root/.npm"}
	if !slices.Equal(j.paths, want) {
		t.Fatalf("paths %q, want %q", j.paths, want)
	}
	vols := j.volumes()
	if len(vols) != 3 || !strings.HasPrefix(vols[0], "/cache/paths/") || !strings.HasSuffix(vols[0], "-node_modules:/builds/group/app/node_modules") || vols[0] == vols[1] {
		t.Fatalf("volumes %q", vols)
	}

	// builds_dir and GIT_CLONE_PATH move relative paths with the checkout.
	j, _ = cacheFor(c, job("main", vars("FLEETCI_CACHE_PATHS", "vendor")), &common.RunnerConfig{RunnerSettings: common.RunnerSettings{BuildsDir: "/b"}})
	if !slices.Equal(j.paths, []string{"/b/group/app/vendor"}) {
		t.Fatalf("builds_dir paths %q", j.paths)
	}
	j, _ = cacheFor(c, job("main", vars("GIT_CLONE_PATH", "/builds/x", "FLEETCI_CACHE_PATHS", "vendor")), nil)
	if !slices.Equal(j.paths, []string{"/builds/x/vendor"}) {
		t.Fatalf("GIT_CLONE_PATH paths %q", j.paths)
	}

	if j, err := cacheFor(c, job("main", vars("FLEETCI_CACHE", "off")), nil); j != nil || err != nil {
		t.Fatalf("FLEETCI_CACHE=off: %+v, %v", j, err)
	}
	for _, p := range []string{".", "/", "/builds", "..", "/builds/group"} {
		if _, err := cacheFor(c, job("main", vars("FLEETCI_CACHE_PATHS", p)), nil); err == nil {
			t.Errorf("FLEETCI_CACHE_PATHS=%s kept the checkout", p)
		}
	}
	if _, err := cacheFor(c, job("main", vars("FLEETCI_CACHE_KEY", strings.Repeat("k", 201))), nil); err == nil {
		t.Error("a 201-character key passed")
	}
	b = job("main")
	b.Runner.URL = "not a url"
	if _, err := cacheFor(c, b, nil); err == nil {
		t.Error("a runner without a host made a scope")
	}
}

func TestWithVolumes(t *testing.T) {
	rc := &common.RunnerConfig{RunnerSettings: common.RunnerSettings{Docker: &common.DockerConfig{Volumes: []string{"/a:/a"}}}}
	o := withVolumes(common.ExecutorPrepareOptions{Config: rc}, []string{"/cache/paths/x:/x"})
	if !slices.Equal(o.Config.Docker.Volumes, []string{"/a:/a", "/cache/paths/x:/x"}) {
		t.Fatalf("volumes %q", o.Config.Docker.Volumes)
	}
	if !slices.Equal(rc.Docker.Volumes, []string{"/a:/a"}) {
		t.Fatalf("the runner's own config changed: %q", rc.Docker.Volumes)
	}
}

func TestCacheDirVolume(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    *common.DockerConfig
		dir  string
		want []string
	}{
		{"no volume", &common.DockerConfig{}, "", []string{"/cache"}},
		{"has it", &common.DockerConfig{Volumes: []string{"/cache"}}, "", nil},
		{"has it from the host", &common.DockerConfig{Volumes: []string{"/srv/c:/cache:rw"}}, "", nil},
		{"other volumes", &common.DockerConfig{Volumes: []string{"/a:/a", "/cache2"}}, "", []string{"/cache"}},
		{"cache_dir", &common.DockerConfig{}, "/var/cache/ci/", []string{"/var/cache/ci"}},
		{"caches off", &common.DockerConfig{DisableCache: true}, "", nil},
		{"no docker", nil, "", nil},
	} {
		rc := &common.RunnerConfig{RunnerSettings: common.RunnerSettings{Docker: tc.d, CacheDir: tc.dir}}
		if got := cacheDirVolume(rc); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := cacheDirVolume(nil); got != nil {
		t.Errorf("no config: %q", got)
	}
}

func TestKeepFromClean(t *testing.T) {
	var c *CacheDisk
	for _, tc := range []struct {
		flags, want string
	}{
		{"", "-ffdx -e /node_modules -e /.next/cache"},
		{"-ffd", "-ffd -e /node_modules -e /.next/cache"},
		{"none", "none"},
	} {
		v := []string{"FLEETCI_CACHE_PATHS", "node_modules .next/cache /root/.npm"}
		if tc.flags != "" {
			v = append(v, "GIT_CLEAN_FLAGS", tc.flags)
		}
		b := job("main", vars(v...))
		j, err := cacheFor(c, b, nil)
		if err != nil {
			t.Fatal(err)
		}
		j.keepFromClean(b)
		got := strings.Join(b.GetGitCleanFlags(), " ")
		if tc.flags == "none" {
			got = b.GetAllVariables().Value("GIT_CLEAN_FLAGS")
		}
		if got != tc.want {
			t.Errorf("GIT_CLEAN_FLAGS %q: git clean %q, want %q", tc.flags, got, tc.want)
		}
	}
	// Paths outside the checkout leave git clean alone.
	b := job("main", vars("FLEETCI_CACHE_PATHS", "/root/.npm"))
	j, _ := cacheFor(c, b, nil)
	j.keepFromClean(b)
	if got := strings.Join(b.GetGitCleanFlags(), " "); got != "-ffdx" {
		t.Errorf("git clean %q", got)
	}
}

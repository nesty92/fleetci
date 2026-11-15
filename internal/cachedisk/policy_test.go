package cachedisk

import (
	"slices"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestPolicyScope(t *testing.T) {
	all := []string{"all"}
	p := &Policy{Writers: &all}
	r := Request{Forge: "github", Instance: "api.github.com", Project: "9", Path: "o/r", Job: Job{Ref: "main"}, From: []string{"main"}}
	s := p.Scope(r)
	if s.Ref != "main" || !s.Writer || s.Project != "9" || !slices.Equal(s.From, []string{"main"}) {
		t.Fatalf("%+v", s)
	}
	// A fork's request never writes, and is kept under the fork's name.
	r.Fork = "someone/r"
	if s := p.Scope(r); s.Writer || s.Ref != "someone/r:main" {
		t.Fatalf("fork: %+v", s)
	}
	// The default policy: protected only.
	var none *Policy
	if none.Scope(Request{Job: Job{Ref: "main"}}).Writer || !none.Scope(Request{Job: Job{Ref: "main", Protected: true}}).Writer {
		t.Fatal("default writers")
	}
}

// A project's writers = [] is no writers, which Validate takes; left out,
// it is an error, not the same.
func TestProjectWritersFromTOML(t *testing.T) {
	var p Policy
	if _, err := toml.Decode("[[projects]]\nmatch = \"g/*\"\nwriters = []\n", &p); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil || p.Rules("g/a") == nil || len(p.Rules("g/a")) != 0 {
		t.Fatalf("writers = []: %v, %#v", err, p.Rules("g/a"))
	}
	p = Policy{}
	if _, err := toml.Decode("[[projects]]\nmatch = \"g/*\"\n", &p); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err == nil {
		t.Fatal("a project without writers passed")
	}
}

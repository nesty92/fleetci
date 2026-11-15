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

// A project's writers = [] is no writers; left out, the top-level ones.
func TestProjectWritersFromTOML(t *testing.T) {
	var p Policy
	if _, err := toml.Decode("[[projects]]\nmatch = \"g/*\"\nwriters = []\n", &p); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil || p.Rules("g/a") == nil || len(p.Rules("g/a")) != 0 {
		t.Fatalf("writers = []: %v, %#v", err, p.Rules("g/a"))
	}
	p = Policy{}
	if _, err := toml.Decode("writers = [\"default\"]\n[[projects]]\nmatch = \"g/*\"\ncheckout = true\n", &p); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil || !slices.Equal(p.Rules("g/a"), []string{"default"}) {
		t.Fatalf("writers left out: %v, %q", err, p.Rules("g/a"))
	}
	if !p.Checkout("g/a") || !p.Checkout("G/A") || p.Checkout("other/a") {
		t.Fatal("checkout is the matching project's")
	}
	if s := p.Scope(Request{Forge: "gitlab", Path: "g/a", Project: "1", Job: Job{Ref: "main"}}); !s.Checkout {
		t.Fatal("the scope doesn't carry checkout")
	}
}

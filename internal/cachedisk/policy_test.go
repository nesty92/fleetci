package cachedisk

import (
	"slices"
	"testing"
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

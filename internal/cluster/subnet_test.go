package cluster

import (
	"os/exec"
	"runtime"
	"slices"
	"testing"
)

// Two workers on one network, here one process, never get the same VM
// subnet, and a subnet is free again once its worker lets it go.
func TestFreeSubnetsClaimed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("abstract unix sockets are Linux's")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("no ip command")
	}
	a, ca, err := freeSubnets(2)
	if err != nil {
		t.Fatal(err)
	}
	b, cb, err := freeSubnets(2)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range b {
		if slices.Contains(a, s) {
			t.Fatalf("both workers got %s: %v and %v", s, a, b)
		}
	}
	for _, l := range ca {
		l.Close()
	}
	c, cc, err := freeSubnets(2)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c, a) {
		t.Errorf("after the first worker ended: %v, want its %v", c, a)
	}
	for _, l := range append(cb, cc...) {
		l.Close()
	}
}

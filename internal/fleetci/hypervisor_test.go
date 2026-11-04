package fleetci

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/fleet"
)

type fakeBackend map[string]bool

func (f fakeBackend) Has(class string) bool { return f[class] }
func (f fakeBackend) Classes() []string {
	var names []string
	for n := range f {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}
func (fakeBackend) Create(_ context.Context, class string) (fleet.VM, error) {
	return fleet.VM{ID: "fleetci-" + class + "-1a2b", Class: class, Instance: "i-0530.x"}, nil
}
func (fakeBackend) Delete(context.Context, string) error { return nil }
func (fakeBackend) Shutdown(context.Context) error       { return nil }

func TestClassFor(t *testing.T) {
	h := NewHypervisor("1vcpu-2gb", fakeBackend{"1vcpu-2gb": true, "4vcpu-6gb": true}, hclog.NewNullLogger())

	for name, want := range map[string]string{
		"class-4vcpu-6gb": "4vcpu-6gb",
		"class-1vcpu-2gb": "1vcpu-2gb",
		"class-":          "1vcpu-2gb", // FLEETCI_CLASS unset
	} {
		got, err := h.ClassFor(name)
		if err != nil || got != want {
			t.Errorf("ClassFor(%q) = %q, %v; want %q", name, got, err, want)
		}
	}

	for _, name := range []string{"class-huge", "ubuntu:24.04", ""} {
		if _, err := h.ClassFor(name); err == nil {
			t.Errorf("ClassFor(%q) succeeded; want error", name)
		}
	}
}

func TestUnknownClassListsClasses(t *testing.T) {
	h := NewHypervisor("1vcpu-2gb", fakeBackend{"1vcpu-2gb": true, "4vcpu-6gb": true}, hclog.NewNullLogger())
	_, err := h.ClassFor("class-huge")
	if err == nil || !strings.Contains(err.Error(), `unknown class "huge"; this runner has 1vcpu-2gb (default), 4vcpu-6gb`) {
		t.Errorf("error %v", err)
	}
}

// The job log shows the address, so it names the machine; the gateway still
// finds the VM by it.
func TestVMAddrNamesInstance(t *testing.T) {
	h := NewHypervisor("1vcpu-2gb", fakeBackend{"1vcpu-2gb": true}, hclog.NewNullLogger())
	vm, err := h.Create(context.Background(), "class-")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := vm.GetAddr(), "fleetci-1vcpu-2gb-1a2b.i-0530-x:22"; got != want {
		t.Errorf("addr %q; want %q", got, want)
	}
	if _, ok := h.Get("fleetci-1vcpu-2gb-1a2b.i-0530-x"); !ok {
		t.Error("gateway can't find the vm by its host")
	}
	if _, ok := h.Get("fleetci-1vcpu-2gb-1a2b"); !ok {
		t.Error("gateway can't find the vm by its id")
	}
}

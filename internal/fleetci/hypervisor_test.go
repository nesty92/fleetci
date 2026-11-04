package fleetci

import (
	"context"
	"testing"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/fleet"
)

type fakeBackend map[string]bool

func (f fakeBackend) Has(class string) bool { return f[class] }
func (fakeBackend) Create(context.Context, string) (fleet.VM, error) {
	return fleet.VM{}, nil
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

package dockervm

import (
	"log/slog"
	"testing"
)

func TestClassFor(t *testing.T) {
	h, err := New(Config{
		Image: "vm", Network: "net", Password: "pw",
		DefaultClass: "small",
		Classes: map[string]Class{
			"small": {CPUs: "1", Memory: "1g"},
			"large": {CPUs: "4", Memory: "4g"},
		},
	}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]string{
		"class-large": "large",
		"class-small": "small",
		"class-":      "small", // FLEETCI_CLASS unset
	} {
		got, _, err := h.ClassFor(name)
		if err != nil || got != want {
			t.Errorf("ClassFor(%q) = %q, %v; want %q", name, got, err, want)
		}
	}

	for _, name := range []string{"class-huge", "ubuntu:24.04", ""} {
		if _, _, err := h.ClassFor(name); err == nil {
			t.Errorf("ClassFor(%q) succeeded; want error", name)
		}
	}
}

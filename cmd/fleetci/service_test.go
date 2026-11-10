package main

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestInstallService(t *testing.T) {
	dir := t.TempDir()
	var calls []string
	ctl := func(args ...string) error { calls = append(calls, strings.Join(args, " ")); return nil }
	if err := installService(dir, "/usr/local/bin/fleetci", "/etc/fleetci/worker.toml", ctl, io.Discard); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, workerService))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ExecStart=/usr/local/bin/fleetci worker -config /etc/fleetci/worker.toml\n", "KillMode=mixed\n", "Restart=always\n", "WantedBy=multi-user.target\n"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the unit has no %q", want)
		}
	}
	if want := []string{"daemon-reload", "enable " + workerService, "restart " + workerService}; !slices.Equal(calls, want) {
		t.Errorf("systemctl %q, want %q", calls, want)
	}
	if err := installService(dir, "/opt/my fleetci", "/etc/fleetci/worker.toml", ctl, io.Discard); err == nil {
		t.Error("a path with a space went into ExecStart")
	}
}

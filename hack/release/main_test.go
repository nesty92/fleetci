package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDistFile(t *testing.T) {
	dir := t.TempDir()
	if got, want := distFile(dir, "fleetci-linux-amd64"), filepath.Join(dir, "fleetci-linux-amd64"); got != want {
		t.Fatalf("no artifacts.json: got %s, want %s", got, want)
	}
	arts := `[{"name":"fleetci","path":"dist/fleetci_linux_amd64_v1/fleetci","type":"Binary"},
{"name":"fleetci-linux-amd64","path":"dist/fleetci_linux_amd64_v1/fleetci","type":"Binary"}]`
	if err := os.WriteFile(filepath.Join(dir, "artifacts.json"), []byte(arts), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := distFile(dir, "fleetci-linux-amd64"), "dist/fleetci_linux_amd64_v1/fleetci"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

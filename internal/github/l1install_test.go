package github

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/vmjob/vmjobtest"
)

// A VM with a cache disk gets the L1 before its runner starts, and it runs
// there.
func TestInstallL1(t *testing.T) {
	if _, err := l1For("riscv64"); !errors.Is(err, errNoL1) {
		t.Fatalf("riscv64: %v", err)
	}
	machine, _ := exec.Command("uname", "-m").Output()
	if _, err := l1For(string(machine)); err != nil {
		t.Skip(err) // built without go generate
	}
	vm := vmjobtest.New(t)
	runner := `read -r jit; read -r hook; "$HOME/` + l1Path + `" > "$HOME/usage" 2>&1; echo "Running job: build"`
	v := vmFor(vmConn(t, vm.Addr))
	v.Cache = true
	if err := startWith(runner, nil, hclog.NewNullLogger())(context.Background(), v, "jit", nil); err != nil {
		t.Fatal(err)
	}
	if out, _ := os.ReadFile(vm.Path("usage")); !strings.Contains(string(out), "usage: fleetci-l1") {
		t.Fatalf("the L1 didn't run on the VM: %q", out)
	}
}

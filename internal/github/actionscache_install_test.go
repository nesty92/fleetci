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

// A VM with a cache disk gets the actions cache before its runner starts, and it runs
// there.
func TestInstallActionsCache(t *testing.T) {
	if _, err := actionsCacheFor("riscv64"); !errors.Is(err, errNoActionsCache) {
		t.Fatalf("riscv64: %v", err)
	}
	machine, _ := exec.Command("uname", "-m").Output()
	if _, err := actionsCacheFor(string(machine)); err != nil {
		t.Skip(err) // built without go generate
	}
	vm := vmjobtest.New(t)
	runner := `read -r jit; read -r hook; "$HOME/` + actionsCachePath + `" > "$HOME/usage" 2>&1; echo "Running job: build"`
	v := vmFor(vmConn(t, vm.Addr))
	v.Cache = true
	if err := startWith(runner, nil, hclog.NewNullLogger())(context.Background(), v, "jit", nil); err != nil {
		t.Fatal(err)
	}
	if out, _ := os.ReadFile(vm.Path("usage")); !strings.Contains(string(out), "usage: fleetci-actions-cache") {
		t.Fatalf("the actions cache didn't run on the VM: %q", out)
	}
}

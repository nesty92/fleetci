//go:build docker

// Run with: go test -tags docker ./internal/fleet/
// Needs Docker and the kit's VM image: docker build -t fleetci-local-vm hack/local/vm
package fleet

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/proxy"
)

func TestClassWithDockerPlugin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	bin := filepath.Join(t.TempDir(), "fleeting-plugin-dockervm")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/fleeting-plugin-dockervm").CombinedOutput(); err != nil {
		t.Fatalf("building plugin: %v\n%s", err, out)
	}

	const network = "fleetci-test"
	_ = exec.Command("docker", "network", "create", network).Run()
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", network).Run() })

	group := "test-" + randomHex(3)
	pluginCfg, _ := json.Marshal(map[string]string{
		"name": group, "image": "fleetci-local-vm", "network": network, "cpus": "1", "memory": "2g",
	})

	log := hclog.New(&hclog.LoggerOptions{Level: hclog.Info, Output: os.Stderr})
	b, err := New(ctx, map[string]Class{
		"1vcpu-2gb": {
			Plugin:       bin,
			PluginConfig: RawConfig(pluginCfg),
			Connector:    Connector{Username: "fleetci", Timeout: Duration(time.Minute)},
			MaxInstances: 2,
			IdleCount:    1,
		},
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Shutdown(context.Background())

	vm, err := b.Create(ctx, "1vcpu-2gb")
	if err != nil {
		t.Fatal(err)
	}

	// Two sessions on the machine's one connection.
	for i := range 2 {
		c, _, err := vm.Conn.Client(ctx)
		if err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		s, _ := c.NewSession()
		out, err := s.Output("docker info --format '{{.NCPU}}' && cat /sys/fs/cgroup/cpu.max")
		if err != nil {
			t.Fatalf("connection %d: %v: %s", i, err, out)
		}
		t.Logf("connection %d: %s", i, strings.Join(strings.Fields(string(out)), " "))
	}

	// Hold the VM like a running job, until the replacement idle VM is up.
	for wait := time.Now().Add(time.Minute); ; {
		out, _ := exec.Command("docker", "ps", "--filter", "label=fleetci.group="+group, "--filter", "status=running", "-q").Output()
		if len(strings.Fields(string(out))) >= 2 {
			break
		}
		if time.Now().After(wait) {
			t.Fatal("no replacement idle VM while the job held its VM")
		}
		time.Sleep(time.Second)
	}
	time.Sleep(5 * time.Second)

	// Released after one use, the instance goes away.
	if err := b.Delete(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Minute)
	for exec.Command("docker", "inspect", vm.Instance).Run() == nil {
		if time.Now().After(deadline) {
			t.Fatalf("instance %s was not removed after its one use", vm.Instance)
		}
		time.Sleep(time.Second)
	}

	// Shut down while a VM is still booting: taking the idle VM makes
	// taskscaler boot a replacement, and shutdown must remove it too.
	vm2, err := b.Create(ctx, "1vcpu-2gb")
	if err != nil {
		t.Fatal(err)
	}
	for wait := time.Now().Add(time.Minute); ; {
		out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label=fleetci.group="+group).Output()
		if len(strings.Fields(string(out))) >= 2 {
			break
		}
		if time.Now().After(wait) {
			t.Fatal("no replacement VM was started")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := b.Delete(ctx, vm2.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label=fleetci.group="+group).Output()
	if n := len(strings.Fields(string(out))); n != 0 {
		t.Errorf("%d VMs left after shutdown", n)
	}
}

// Two classes on one group would each remove the other's VMs, so the second
// is refused before its taskscaler starts.
func TestClassesNeedTheirOwnGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	bin := filepath.Join(t.TempDir(), "fleeting-plugin-dockervm")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/fleeting-plugin-dockervm").CombinedOutput(); err != nil {
		t.Fatalf("building plugin: %v\n%s", err, out)
	}
	cfg := func(group string) RawConfig {
		b, _ := json.Marshal(map[string]string{"name": group, "image": "fleetci-local-vm", "network": "bridge", "cpus": "1", "memory": "1g"})
		return b
	}
	class := func(group string) Class {
		return Class{Plugin: bin, PluginConfig: cfg(group), Connector: Connector{Username: "fleetci"}, MaxInstances: 1}
	}
	group := "test-" + randomHex(3)
	log := hclog.NewNullLogger()

	_, err := New(ctx, map[string]Class{"a": class(group), "b": class(group)}, log)
	if err == nil || !strings.Contains(err.Error(), `as class "a" does`) {
		t.Fatalf("shared group accepted: %v", err)
	}

	b, err := New(ctx, map[string]Class{"a": class(group), "b": class(group + "-b")}, log)
	if err != nil {
		t.Fatalf("separate groups refused: %v", err)
	}
	_ = b.Shutdown(ctx)
}

package fleet

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestBackendMetrics(t *testing.T) {
	own, cloud := fakePool("4vcpu-8gb", 2, "4vcpu-8gb-aws"), fakePool("4vcpu-8gb-aws", 1, "")
	b := &Backend{log: hclog.NewNullLogger(), vms: map[string]*pool{}, metrics: newMetrics(),
		pools: map[string][]*pool{own.name: {own}, cloud.name: {cloud}}}
	// The class's one instance was up before the jobs asked; the
	// overflow's boots for its job.
	own.ups.up("i-1", time.Now().Add(-time.Minute))
	ctx := context.Background()

	var first VM
	for i := range 3 {
		vm, err := b.Create(ctx, own.name)
		if err != nil {
			t.Fatalf("job %d: %v", i+1, err)
		}
		if i == 0 {
			first = vm
		}
	}
	if _, err := b.Create(ctx, own.name); err == nil {
		t.Fatal("a fourth job got a VM of full classes")
	}
	if err := b.Delete(ctx, first.ID); err != nil {
		t.Fatal(err)
	}

	// What taskscaler, fleeting and a reclaim watch report.
	b.metrics.scaler(cloud.name).TasksCountSet("idle", 3)
	b.metrics.provisioner(cloud.name).InstancesCountSet("running", 4)
	b.metrics.provisioner(cloud.name).InstanceIsRunningTimeObserve(40 * time.Second)
	b.metrics.nominate(own.name)
	w := newReclaimWatch(nil, false, hclog.NewNullLogger())
	w.noted = b.metrics.notice(cloud.name)
	w.stops["i-1"], w.stops["i-2"] = func() {}, func() {}
	w.saw("i-1", "terminate", "2026-11-17T10:00:00Z", hclog.NewNullLogger())
	w.saw("i-2", "rebalance", "2026-11-17T10:00:00Z", hclog.NewNullLogger())
	cloud.reclaim = w

	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(b.Metrics())
	want := `
# HELP fleetci_class_draining_vms The class's VMs with a reclaim notice: they take no new jobs and go once their jobs end.
# TYPE fleetci_class_draining_vms gauge
fleetci_class_draining_vms{class="4vcpu-8gb"} 0
fleetci_class_draining_vms{class="4vcpu-8gb-aws"} 1
# HELP fleetci_class_instances The class's instances as its fleeting provisioner counts them: requested, pending, creating, running, deleting and others.
# TYPE fleetci_class_instances gauge
fleetci_class_instances{class="4vcpu-8gb-aws",state="running"} 4
# HELP fleetci_class_jobs The class's jobs: waiting for a VM (for room, or for one to boot) or running on one.
# TYPE fleetci_class_jobs gauge
fleetci_class_jobs{class="4vcpu-8gb",state="running"} 1
fleetci_class_jobs{class="4vcpu-8gb",state="waiting"} 0
fleetci_class_jobs{class="4vcpu-8gb-aws",state="running"} 1
fleetci_class_jobs{class="4vcpu-8gb-aws",state="waiting"} 0
# HELP fleetci_class_slots The class's job slots as its taskscaler counts them: idle (ready for a job), pending (booting), acquired, reserved, unavailable, unhealthy, suspended.
# TYPE fleetci_class_slots gauge
fleetci_class_slots{class="4vcpu-8gb-aws",state="idle"} 3
# HELP fleetci_nominations_total Jobs that waited nominate_after and had a host hold the room that frees for them.
# TYPE fleetci_nominations_total counter
fleetci_nominations_total{class="4vcpu-8gb"} 1
# HELP fleetci_overflow_total Jobs of a full class that ran on its overflow class, by that class.
# TYPE fleetci_overflow_total counter
fleetci_overflow_total{class="4vcpu-8gb",to="4vcpu-8gb-aws"} 1
# HELP fleetci_reclaim_notices_total Notices the cloud gave about a class's VMs: reclaim (the VM is being taken back; it takes no new jobs) or rebalance (a recommendation).
# TYPE fleetci_reclaim_notices_total counter
fleetci_reclaim_notices_total{class="4vcpu-8gb-aws",kind="rebalance"} 1
fleetci_reclaim_notices_total{class="4vcpu-8gb-aws",kind="reclaim"} 1
# HELP fleetci_vm_acquire_failures_total Jobs that got no VM: timeout (acquire_timeout passed, waiting for room or a boot), cancelled (the job stopped waiting) or error.
# TYPE fleetci_vm_acquire_failures_total counter
fleetci_vm_acquire_failures_total{class="4vcpu-8gb",reason="timeout"} 1
# HELP fleetci_vms_acquired_total VMs jobs got, by whether the VM was up before the job asked (warm: idle_count or a VM left by an ended wait) or booted for it (cold).
# TYPE fleetci_vms_acquired_total counter
fleetci_vms_acquired_total{class="4vcpu-8gb",start="warm"} 2
fleetci_vms_acquired_total{class="4vcpu-8gb-aws",start="cold"} 1
`
	names := []string{"fleetci_class_draining_vms", "fleetci_class_instances", "fleetci_class_jobs", "fleetci_class_slots",
		"fleetci_nominations_total", "fleetci_overflow_total", "fleetci_reclaim_notices_total",
		"fleetci_vm_acquire_failures_total", "fleetci_vms_acquired_total"}
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), names...); err != nil {
		t.Fatal(err)
	}

	// Every job asked as 4vcpu-8gb: the wait is the class it asked for.
	counts := map[string]uint64{}
	held := map[string]uint64{}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			switch mf.GetName() {
			case "fleetci_vm_wait_seconds":
				counts[m.GetLabel()[0].GetValue()] = m.GetHistogram().GetSampleCount()
			case "fleetci_vm_held_seconds":
				held[m.GetLabel()[0].GetValue()] = m.GetHistogram().GetSampleCount()
			case "fleetci_instance_boot_seconds":
				if m.GetHistogram().GetSampleCount() != 1 {
					t.Errorf("boot samples %d, want 1", m.GetHistogram().GetSampleCount())
				}
			}
		}
	}
	if counts["4vcpu-8gb"] != 3 || len(counts) != 1 {
		t.Errorf("wait samples %v, want 3 for 4vcpu-8gb", counts)
	}
	if held["4vcpu-8gb"] != 1 || len(held) != 1 {
		t.Errorf("held samples %v, want 1 for 4vcpu-8gb", held)
	}
}

// A job held for room, its class and overflow full, counts as waiting
// until it stops waiting; a cancelled one isn't a timeout.
func TestHeldJobsCountAsWaiting(t *testing.T) {
	own, cloud := fakePool("4vcpu-8gb", 1, "4vcpu-8gb-aws"), fakePool("4vcpu-8gb-aws", 1, "")
	own.timeout = time.Minute
	b := &Backend{log: hclog.NewNullLogger(), vms: map[string]*pool{}, metrics: newMetrics(),
		pools: map[string][]*pool{own.name: {own}, cloud.name: {cloud}}}
	for range 2 {
		if _, err := b.Create(context.Background(), own.name); err != nil {
			t.Fatal(err)
		}
	}
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(b.Metrics())
	jobs := func(state string) string {
		return fmt.Sprintf(`
# HELP fleetci_class_jobs The class's jobs: waiting for a VM (for room, or for one to boot) or running on one.
# TYPE fleetci_class_jobs gauge
fleetci_class_jobs{class="4vcpu-8gb",state="running"} 1
fleetci_class_jobs{class="4vcpu-8gb",state="waiting"} %s
fleetci_class_jobs{class="4vcpu-8gb-aws",state="running"} 1
fleetci_class_jobs{class="4vcpu-8gb-aws",state="waiting"} 0
`, state)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		_, err := b.Create(ctx, own.name)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		own.mu.Lock()
		held := own.held
		own.mu.Unlock()
		if held == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the third job was never held")
		}
		time.Sleep(time.Millisecond)
	}
	if err := testutil.GatherAndCompare(reg, strings.NewReader(jobs("1")), "fleetci_class_jobs"); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a cancelled job got a VM")
	}
	if err := testutil.GatherAndCompare(reg, strings.NewReader(jobs("0")), "fleetci_class_jobs"); err != nil {
		t.Fatal(err)
	}
	want := `
# HELP fleetci_vm_acquire_failures_total Jobs that got no VM: timeout (acquire_timeout passed, waiting for room or a boot), cancelled (the job stopped waiting) or error.
# TYPE fleetci_vm_acquire_failures_total counter
fleetci_vm_acquire_failures_total{class="4vcpu-8gb",reason="cancelled"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "fleetci_vm_acquire_failures_total"); err != nil {
		t.Fatal(err)
	}
}

// An instance that's gone is forgotten, so its id coming back later
// isn't taken for warm.
func TestUpTimesForgetsGoneInstances(t *testing.T) {
	var u upTimes
	u.up("i-1", time.Now().Add(-time.Minute))
	g := vmGroup{newCloud(), newVMConns(newCloud(), false, hclog.NewNullLogger()), nil, nil, &u}
	g.forget("i-1")
	if u.before("i-1", time.Now()) {
		t.Fatal("a gone instance is still taken for up")
	}
}

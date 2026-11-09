package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
)

func TestAllowed(t *testing.T) {
	for _, c := range []struct {
		policy, cur, v string
		ok             bool
	}{
		{UpdatePatch, "v0.7.0", "v0.7.1", true},
		{UpdatePatch, "v0.7.0", "v0.8.0", false},
		{UpdateMinor, "v0.7.0", "v0.8.0", true},
		{UpdateMinor, "v0.7.0", "v1.0.0", false},
		{UpdateLatest, "v0.7.0", "v1.0.0", true},
		{UpdateLatest, "v0.7.0", "v0.7.0", false},
		{UpdateLatest, "v0.7.0", "v0.6.0", false},
		{UpdateLatest, "v0.7.0", "v0.8.0-rc.1", false},
		{UpdateLatest, "dev", "v0.8.0", false},
		{"v0.6.0", "v0.7.0", "v0.6.0", true}, // a pin goes back too
		{"v0.6.0", "dev", "v0.6.0", true},
		{"v0.6.0", "v0.7.0", "v0.8.0", false},
		{UpdateOff, "v0.7.0", "v0.7.1", false},
	} {
		if got := allowed(c.policy, c.cur, c.v); got != c.ok {
			t.Errorf("allowed(%s, %s, %s) = %v", c.policy, c.cur, c.v, got)
		}
	}
	if s := strictest(UpdateLatest, UpdatePatch); s != UpdatePatch {
		t.Errorf("strictest %s", s)
	}
	if s := strictest("", UpdateOff); s != UpdateOff {
		t.Errorf("strictest %s", s)
	}
}

// TestUpdateSlot: the leader gives one worker at a time its slot, and none
// to a version whose protocol it does not speak.
func TestUpdateSlot(t *testing.T) {
	f := newTestFleet(t)
	c := f.c
	c.cfg.Controller.WorkerUpdate, c.cfg.Controller.UpdateParallel = UpdateLatest, 1
	c.mu.Lock()
	c.leader = c.ID()
	a, b := c.workerLocked("w1"), c.workerLocked("w2")
	a.seen, b.seen = time.Now(), time.Now()
	a.report.Update = &UpdateStatus{State: UpdateReady, Target: "v1.0.0", Protocol: Protocol + 1}
	if v := c.updateSlotLocked(a); v != "" {
		t.Fatalf("a slot for a protocol the controllers don't speak: %q", v)
	}
	a.report.Update.Protocol = Protocol
	b.report.Update = &UpdateStatus{State: UpdateReady, Target: "v1.0.0", Protocol: Protocol}
	if v := c.updateSlotLocked(a); v != "v1.0.0" {
		t.Fatalf("w1: %q", v)
	}
	if v := c.updateSlotLocked(b); v != "" {
		t.Fatalf("w2 got a slot with w1's taken: %q", v)
	}
	a.report.Update = &UpdateStatus{State: UpdateTrial, Target: "v1.0.0"}
	c.updateSlotLocked(a)
	if v := c.updateSlotLocked(b); v != "" {
		t.Fatalf("w2 got a slot with w1 on trial: %q", v)
	}
	a.report.Update = nil // committed
	c.updateSlotLocked(a)
	if v := c.updateSlotLocked(b); v != "v1.0.0" {
		t.Fatalf("w2 after w1: %q", v)
	}
	// A worker that rolled back from a version stops its rollout.
	delete(c.granted, "w2")
	a.report.Update = &UpdateStatus{State: UpdateFailed, Target: "v1.0.0"}
	if v := c.updateSlotLocked(b); v != "" {
		t.Fatalf("w2 got a slot for the version w1 failed: %q", v)
	}
	a.report.Update = nil
	c.leader = "ctl-0"
	delete(c.granted, "w2")
	if v := c.updateSlotLocked(b); v != "" {
		t.Fatalf("a standby gave a slot: %q", v)
	}
	c.mu.Unlock()
}

// testWorker is a Worker with only what the update needs: binaries in a
// directory, and its state.
func testWorker(t *testing.T, version string) (*Worker, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	_ = os.MkdirAll(bin, 0o755)
	for _, n := range []string{"fleetci", "fleeting-plugin-fleetingd", "cloud-hypervisor"} {
		if err := os.WriteFile(filepath.Join(bin, n), []byte(n+" "+version), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &WorkerConfig{StateDir: filepath.Join(dir, "state"), FleetingdPlugin: filepath.Join(bin, "fleeting-plugin-fleetingd")}
	_ = os.MkdirAll(cfg.StateDir, 0o755)
	u, err := newUpdater(UpdateConfig{Trial: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	u.self = filepath.Join(bin, "fleetci")
	return &Worker{cfg: cfg, log: hclog.NewNullLogger(), version: version, update: u,
		checks: map[string]string{}, passed: map[string]bool{}, tasks: map[string]*task{}, changed: make(chan struct{}, 1)}, bin
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestTrialRollback: a swapped-in version that fails its microVM check is
// rolled back, and skipped from then on; one that stops during its trial
// too.
func TestTrialRollback(t *testing.T) {
	old, bin := testWorker(t, "v1.0.0")
	// What install does, by hand: the old binaries kept, the marker, the
	// new ones in.
	files := old.installedFiles()
	_ = os.MkdirAll(old.previousDir(), 0o755)
	for name, path := range files {
		if err := copyFile(path, filepath.Join(old.previousDir(), name)); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(path, []byte(name+" v1.1.0"), 0o755)
	}
	if err := writeJSONFile(old.trialPath(), trialMarker{From: "v1.0.0", To: "v1.1.0", Files: files, Passed: []string{"small"}}); err != nil {
		t.Fatal(err)
	}

	// The new version starts: on trial. Its check fails: rolled back.
	nw := &Worker{cfg: old.cfg, log: old.log, version: "v1.1.0", update: old.update, checks: map[string]string{}, passed: map[string]bool{}, tasks: map[string]*task{}, changed: make(chan struct{}, 1)}
	if err := nw.StartTrial(); err != nil {
		t.Fatal(err)
	}
	if nw.update.status.State != UpdateTrial {
		t.Fatalf("not on trial: %+v", nw.update.status)
	}
	nw.checks["small"] = "FAIL: boot timed out"
	if err := nw.advanceUpdate(context.Background()); !errors.Is(err, ErrRestart) {
		t.Fatalf("a failed check: %v", err)
	}
	if got := read(t, filepath.Join(bin, "cloud-hypervisor")); got != "cloud-hypervisor v1.0.0" {
		t.Fatalf("not rolled back: %q", got)
	}
	if _, err := os.Stat(old.trialPath()); err == nil {
		t.Fatal("the marker is still there")
	}
	if why := old.failedVersions()["v1.1.0"]; why == "" {
		t.Fatal("v1.1.0 not recorded as failed")
	}
	if allowed(UpdateLatest, "v1.0.0", "v1.1.0") {
		if v, _ := old.lastFailure(); v != "v1.1.0" {
			t.Fatalf("last failure %q", v)
		}
	}
	old.update.status = UpdateStatus{} // the old version, restarted
	old.mu.Lock()
	r := old.updateReportLocked()
	old.mu.Unlock()
	if r == nil || r.State != UpdateFailed || r.Target != "v1.1.0" {
		t.Fatalf("the old version's report: %+v", r)
	}

	// A version that stops during its trial: the next start rolls back.
	_ = os.WriteFile(filepath.Join(bin, "fleetci"), []byte("fleetci v1.2.0"), 0o755)
	_ = writeJSONFile(old.trialPath(), trialMarker{From: "v1.0.0", To: "v1.2.0", Files: map[string]string{"fleetci": filepath.Join(bin, "fleetci")}, Attempts: 1})
	again := &Worker{cfg: old.cfg, log: old.log, version: "v1.2.0", update: old.update}
	if err := again.StartTrial(); !errors.Is(err, ErrRestart) {
		t.Fatalf("a second start on trial: %v", err)
	}
	if got := read(t, filepath.Join(bin, "fleetci")); got != "fleetci v1.0.0" {
		t.Fatalf("not rolled back: %q", got)
	}
}

// TestTrialCommit: a version that reached a controller, passed its checks
// and stayed up is committed.
func TestTrialCommit(t *testing.T) {
	w, _ := testWorker(t, "v1.1.0")
	_ = os.MkdirAll(w.previousDir(), 0o755)
	_ = writeJSONFile(w.trialPath(), trialMarker{From: "v1.0.0", To: "v1.1.0", Files: map[string]string{}, Passed: []string{"small"}})
	if err := w.StartTrial(); err != nil {
		t.Fatal(err)
	}
	w.passed["small"] = true
	w.reached()
	if err := w.advanceUpdate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.update.trial == nil {
		t.Fatal("committed before its trial time")
	}
	w.update.reachedAt = time.Now().Add(-2 * time.Minute)
	if err := w.advanceUpdate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.update.trial != nil {
		t.Fatal("not committed")
	}
	if _, err := os.Stat(w.trialPath()); err == nil {
		t.Fatal("the marker is still there")
	}
}

// TestPickRelease: the newest release the policy allows, with a manifest,
// not failed before.
func TestPickRelease(t *testing.T) {
	w, _ := testWorker(t, "v1.0.0")
	rels := []map[string]any{
		{"tag_name": "v1.0.1", "assets": []map[string]string{{"name": "versions.json", "browser_download_url": "https://x/v1.0.1/versions.json"}}},
		{"tag_name": "v1.0.2", "assets": []map[string]string{{"name": "versions.json", "browser_download_url": "https://x/v1.0.2/versions.json"}}},
		{"tag_name": "v1.0.3", "assets": []map[string]string{{"name": "fleetci-linux-amd64"}}}, // no manifest
		{"tag_name": "v1.1.0", "assets": []map[string]string{{"name": "versions.json", "browser_download_url": "https://x/v1.1.0/versions.json"}}},
		{"tag_name": "vm-image-github", "assets": []map[string]string{{"name": "versions.json", "browser_download_url": "https://x/i/versions.json"}}},
	}
	srv := httpServeJSON(t, rels)
	w.update.cfg.Source = srv
	if v, base, err := w.pickRelease(context.Background(), UpdatePatch); err != nil || v != "v1.0.2" || base != "https://x/v1.0.2/" {
		t.Fatalf("patch: %q %q %v", v, base, err)
	}
	if v, _, _ := w.pickRelease(context.Background(), UpdateLatest); v != "v1.1.0" {
		t.Fatalf("latest: %q", v)
	}
	w.recordFailed("v1.1.0", "boom")
	if v, _, _ := w.pickRelease(context.Background(), UpdateLatest); v != "v1.0.2" {
		t.Fatalf("latest, v1.1.0 failed: %q", v)
	}
}

func httpServeJSON(t *testing.T, v any) string {
	t.Helper()
	b, _ := json.Marshal(v)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(b) })}
	lis, err := netListen()
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + lis.Addr().String() + "/releases"
}

func netListen() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }

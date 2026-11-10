package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/nesty92/fleetci/internal/release"
)

// A worker updates itself from fleetci's signed releases, never from the
// controllers, which only say how far it may go and when its turn is:
//
//  1. It checks for a release now and then, or when asked, and downloads
//     the newest the controllers allow to update/<version>, checked
//     against the release's signed versions.json.
//  2. It asks the leader for a slot: one worker at a time by default, so a
//     bad release reaches one machine, not the fleet.
//  3. With the slot it drains, and once its jobs are done it checks that
//     the new fleetci runs, swaps the binaries in, keeping the old, writes
//     update.trial and starts the new one.
//  4. The new one is on trial: it must reach a controller and pass the
//     microVM check of every class the old one passed, within trialWait,
//     and stay up for trial. Then it is committed. If it fails, or stops
//     during its trial, the old binaries go back and the version is
//     skipped until a newer one comes out.

// UpdateConfig is worker.toml's [update].
type UpdateConfig struct {
	// Auto lets the worker update itself, as far as the controllers'
	// worker_update allows. Default false.
	Auto bool `toml:"auto"`
	// Source lists the releases, as GitHub's API does. Default fleetci's.
	// "controllers" takes the controllers' version and the binaries they
	// serve to joining workers instead, over the fleet's own TLS: for a
	// fleet that does not reach GitHub, whose controllers are updated by
	// hand.
	Source string `toml:"source,omitempty"`
	// PublicKey is the key releases are signed with. Default fleetci's.
	PublicKey string `toml:"public_key,omitempty"`
	// CheckEvery is how often it looks for a release, give or take a
	// quarter. Default 24h.
	CheckEvery time.Duration `toml:"check_every,omitzero"`
	// Trial is how long a new version must stay up. Default 3m.
	Trial time.Duration `toml:"trial,omitzero"`
}

const (
	defaultUpdateSource = "https://api.github.com/repos/nesty92/fleetci/releases"
	// trialWait is how long a new version has to reach a controller and
	// pass its microVM checks.
	trialWait = 15 * time.Minute
)

// ErrRestart is the worker's way of asking fleetci to start its binary
// again: a new version to try, or the old one back.
var ErrRestart = errors.New("restart the worker")

// The update's files, in the worker's state dir.
func (w *Worker) updateDir() string   { return filepath.Join(w.cfg.StateDir, "update") }
func (w *Worker) trialPath() string   { return filepath.Join(w.cfg.StateDir, "update.trial") }
func (w *Worker) failedPath() string  { return filepath.Join(w.updateDir(), "failed.json") }
func (w *Worker) previousDir() string { return filepath.Join(w.updateDir(), "previous") }

// trialMarker is update.trial: what was swapped, and how the trial goes.
type trialMarker struct {
	From     string            `json:"from"`
	To       string            `json:"to"`
	Files    map[string]string `json:"files"`  // binary name → where it was installed
	Passed   []string          `json:"passed"` // the classes the old version passed
	Attempts int               `json:"attempts"`
	Started  time.Time         `json:"started"`
}

// updater is a worker's self-update.
type updater struct {
	cfg  UpdateConfig
	key  release.Key
	hc   *http.Client
	self string // this fleetci binary

	nudge chan struct{}

	// Under the worker's mu:
	status    UpdateStatus
	policies  map[string]string // each reachable controller's policy, by address
	offers    map[string]offer  // each reachable controller's version and binaries, by address
	checked   time.Time         // the last check
	asked     time.Time         // the latest check the controllers asked for
	staged    *release.Manifest
	goAhead   string // the version the leader gave the slot to
	trial     *trialMarker
	reachedAt time.Time // when a controller first took a report, on trial
}

func newUpdater(cfg UpdateConfig) (*updater, error) {
	cfg.Source = cmpStr(cfg.Source, defaultUpdateSource)
	if cfg.CheckEvery <= 0 {
		cfg.CheckEvery = 24 * time.Hour
	}
	if cfg.Trial <= 0 {
		cfg.Trial = 3 * time.Minute
	}
	k, err := release.ParseKey(cmpStr(cfg.PublicKey, release.PublicKey))
	if err != nil {
		return nil, fmt.Errorf("[update] public_key: %w", err)
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return &updater{cfg: cfg, key: k, hc: &http.Client{Timeout: 10 * time.Minute}, self: self, nudge: make(chan struct{}, 1)}, nil
}

func cmpStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// StartTrial is run before the worker starts: with an update on trial it
// counts the attempt, and rolls back when this is not the first, since
// the new version stopped during its trial. ErrRestart then asks for the
// old binary to be started.
func (w *Worker) StartTrial() error {
	b, err := os.ReadFile(w.trialPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var m trialMarker
	if err := json.Unmarshal(b, &m); err != nil {
		w.log.Error("update.trial is damaged; rolling back", "err", err)
		return w.rollback(&trialMarker{Files: w.installedFiles()}, "its trial marker was damaged")
	}
	if m.To != w.version {
		// Not the version on trial: the swap did not take. Put the old
		// binaries back, if they are not.
		return w.rollback(&m, fmt.Sprintf("fleetci %s started in its place", w.version))
	}
	m.Attempts++
	if m.Attempts > 1 {
		return w.rollback(&m, "it stopped during its trial")
	}
	m.Started = time.Now()
	if err := writeJSONFile(w.trialPath(), m); err != nil {
		return err
	}
	w.log.Info("on trial after an update", "from", m.From, "to", m.To)
	w.mu.Lock()
	w.update.trial = &m
	w.update.status = UpdateStatus{State: UpdateTrial, Target: m.To}
	w.mu.Unlock()
	return nil
}

// updateLoop looks for releases, stages them, and takes them in its slot.
func (w *Worker) updateLoop(ctx context.Context) {
	u := w.update
	check := time.NewTimer(time.Duration(rand.Int64N(int64(10 * time.Minute)))) // not all at once after a restart
	defer check.Stop()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-check.C:
			w.checkUpdate(ctx)
			jitter := time.Duration(rand.Int64N(int64(u.cfg.CheckEvery/2))) - u.cfg.CheckEvery/4
			check.Reset(u.cfg.CheckEvery + jitter)
		case <-u.nudge:
			w.checkUpdate(ctx)
		case <-tick.C:
		}
		if err := w.advanceUpdate(ctx); err != nil {
			if errors.Is(err, ErrRestart) {
				w.restart(err)
				return
			}
			w.log.Error("update", "err", err)
		}
	}
}

// restart ends the worker with ErrRestart, for fleetci to start its binary
// again.
func (w *Worker) restart(err error) {
	w.mu.Lock()
	w.exitErr = err
	stop := w.stop
	w.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// takeUpdateOrders reads a controller's answer: its policy, a check it
// asks for, and the slot.
func (w *Worker) takeUpdateOrders(addr string, resp *StatusResponse) {
	if w.update == nil {
		return
	}
	u := w.update
	w.mu.Lock()
	defer w.mu.Unlock()
	if resp == nil { // unreachable: what it said no longer counts
		delete(u.policies, addr)
		delete(u.offers, addr)
		return
	}
	if u.policies == nil {
		u.policies, u.offers = map[string]string{}, map[string]offer{}
	}
	u.policies[addr] = resp.Update
	u.offers[addr] = offer{Version: resp.Version, Protocol: resp.Protocol, Artifacts: resp.Artifacts}
	if resp.UpdateGo != "" && u.staged != nil && resp.UpdateGo == u.staged.Version && u.goAhead == "" {
		u.goAhead = resp.UpdateGo
		w.log.Info("update: the leader gave this worker its slot; draining", "to", u.goAhead)
	}
	if resp.UpdateCheck.After(u.asked) {
		u.asked = resp.UpdateCheck
		if resp.UpdateCheck.After(u.checked) {
			select {
			case u.nudge <- struct{}{}:
			default:
			}
		}
	}
}

// strictest of two policies; "" is none heard yet.
func strictest(a, b string) string {
	rank := map[string]int{"": 5, UpdateLatest: 4, UpdateMinor: 3, UpdatePatch: 2, UpdateOff: 0}
	ra, oka := rank[a]
	rb, okb := rank[b]
	switch {
	case !oka: // a pin
		return a
	case !okb:
		return b
	case rb < ra:
		return b
	}
	return a
}

// checkUpdate looks for the newest release the policy allows, and
// downloads it.
func (w *Worker) checkUpdate(ctx context.Context) {
	u := w.update
	w.mu.Lock()
	u.checked = time.Now()
	policy, state := "", u.status.State
	for _, p := range slices.Sorted(maps.Values(u.policies)) { // sorted: two pins, the same one each time
		policy = strictest(policy, p)
	}
	w.mu.Unlock()
	if !u.cfg.Auto || policy == "" || policy == UpdateOff || state == UpdateTrial || state == UpdateDraining {
		return
	}
	var target, base string
	var err error
	if u.cfg.Source == SourceControllers {
		target, base = w.pickOffer(policy)
	} else {
		target, base, err = w.pickRelease(ctx, policy)
	}
	if err != nil || target == "" {
		w.setUpdate(func(s *UpdateStatus) {
			if err != nil {
				s.Err = "checking for releases: " + err.Error()
			}
		})
		if err != nil {
			w.log.Warn("update: checking for releases", "err", err)
		}
		return
	}
	w.mu.Lock()
	staged := u.staged != nil && u.staged.Version == target
	w.mu.Unlock()
	if staged {
		return
	}
	w.setUpdate(func(s *UpdateStatus) { *s = UpdateStatus{State: UpdateStaging, Target: target} })
	w.log.Info("update: downloading", "to", target)
	var m release.Manifest
	if u.cfg.Source == SourceControllers {
		m, err = w.stageOffer(ctx, base, target)
	} else {
		m, err = w.stage(ctx, base, target)
	}
	if err != nil {
		w.log.Error("update: downloading", "to", target, "err", err)
		w.setUpdate(func(s *UpdateStatus) { *s = UpdateStatus{Err: fmt.Sprintf("downloading %s: %v", target, err)} })
		return
	}
	w.mu.Lock()
	u.staged = &m
	u.status = UpdateStatus{State: UpdateReady, Target: target, Protocol: m.Protocol}
	w.mu.Unlock()
	w.log.Info("update: downloaded; waiting for a slot", "to", target)
	w.poke()
}

func (w *Worker) setUpdate(f func(*UpdateStatus)) {
	w.mu.Lock()
	f(&w.update.status)
	w.mu.Unlock()
	w.poke()
}

// SourceControllers, as [update] source, updates a worker to the
// controllers' version, with the binaries they serve.
const SourceControllers = "controllers"

// offer is what a controller serves: its version, protocol and binaries.
type offer struct {
	Version   string
	Protocol  int
	Artifacts []Artifact
}

// pickOffer is the newest controller version the policy allows past this
// one and not failed here before, and the controller to fetch it from.
func (w *Worker) pickOffer(policy string) (version, addr string) {
	failed := w.failedVersions()
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, a := range slices.Sorted(maps.Keys(w.update.offers)) {
		o := w.update.offers[a]
		if failed[o.Version] != "" || !allowed(policy, w.version, o.Version) || !slices.ContainsFunc(o.Artifacts, func(x Artifact) bool { return x.Name == "fleetci" }) {
			continue
		}
		if version == "" || semver.Compare(o.Version, version) > 0 {
			version, addr = o.Version, a
		}
	}
	return version, addr
}

// stageOffer downloads a controller's binaries, each checked against the
// sum it gave, into the version's update directory.
func (w *Worker) stageOffer(ctx context.Context, addr, version string) (release.Manifest, error) {
	w.mu.Lock()
	o, cl := w.update.offers[addr], w.clients[addr]
	w.mu.Unlock()
	m := release.Manifest{Version: version, Protocol: o.Protocol, MinProtocol: o.Protocol, Platforms: map[string]map[string]release.Binary{}}
	if o.Version != version || cl == nil {
		return m, fmt.Errorf("controller %s no longer serves %s", addr, version)
	}
	dir := filepath.Join(w.updateDir(), version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return m, err
	}
	bins := map[string]release.Binary{}
	for _, a := range o.Artifacts {
		path := filepath.Join(dir, a.Name)
		if sum, _, err := release.SHA256File(path); err != nil || sum != a.SHA256 {
			if err := fetchArtifact(ctx, cl, a, path); err != nil {
				return m, fmt.Errorf("%s: %w", a.Name, err)
			}
		}
		bins[a.Name] = release.Binary{Version: version, SHA256: a.SHA256, Size: a.Size}
	}
	m.Platforms["linux/"+runtime.GOARCH] = bins
	return m, nil
}

// ghRelease is a release as GitHub's API lists it.
type ghRelease struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// pickRelease is the newest release the policy allows past this version
// and not failed here before: its version, and where its assets are.
func (w *Worker) pickRelease(ctx context.Context, policy string) (string, string, error) {
	u := w.update
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.cfg.Source, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := u.hc.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("%s: %s", u.cfg.Source, resp.Status)
	}
	var rels []ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&rels); err != nil {
		return "", "", err
	}
	failed := w.failedVersions()
	best, base := "", ""
	for _, r := range rels {
		if r.Draft || r.Prerelease || !allowed(policy, w.version, r.Tag) {
			continue
		}
		if _, bad := failed[r.Tag]; bad {
			continue
		}
		for _, a := range r.Assets {
			if a.Name == release.ManifestName && (best == "" || semver.Compare(r.Tag, best) > 0) {
				best, base = r.Tag, strings.TrimSuffix(a.URL, release.ManifestName)
			}
		}
	}
	return best, base, nil
}

// allowed says whether the policy lets a worker on current go to v.
func allowed(policy, current, v string) bool {
	if !semver.IsValid(v) || semver.Prerelease(v) != "" || v != semver.Canonical(v) {
		return false
	}
	if semver.IsValid(policy) {
		return v == policy && v != current
	}
	if !semver.IsValid(current) {
		return false // a build with no version, as dev: only a pin moves it
	}
	if semver.Compare(v, current) <= 0 {
		return false
	}
	switch policy {
	case UpdatePatch:
		return semver.MajorMinor(v) == semver.MajorMinor(current)
	case UpdateMinor:
		return semver.Major(v) == semver.Major(current)
	case UpdateLatest:
		return true
	}
	return false
}

// stage downloads a release's binaries for this machine to update/<v>.
func (w *Worker) stage(ctx context.Context, base, version string) (release.Manifest, error) {
	u := w.update
	m, err := release.FetchManifest(ctx, u.hc, u.key, base)
	if err != nil {
		return m, err
	}
	if m.Version != version {
		return m, fmt.Errorf("its versions.json is %s's", m.Version)
	}
	bins := m.Platforms["linux/"+runtime.GOARCH]
	if bins["fleetci"].URL == "" {
		return m, release.ErrNoPlatform
	}
	dir := filepath.Join(w.updateDir(), version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return m, err
	}
	for name, b := range bins {
		path := filepath.Join(dir, name)
		if sum, _, err := release.SHA256File(path); err == nil && sum == b.SHA256 {
			continue
		}
		if err := release.Download(ctx, u.hc, b, path); err != nil {
			return m, fmt.Errorf("%s: %w", name, err)
		}
	}
	return m, nil
}

// advanceUpdate moves an update on: a staged one with its slot drains,
// and once idle is installed; one on trial is committed or rolled back.
func (w *Worker) advanceUpdate(ctx context.Context) error {
	u := w.update
	w.mu.Lock()
	if t := u.trial; t != nil {
		defer w.mu.Unlock()
		return w.judgeTrialLocked(t)
	}
	if u.goAhead == "" || u.staged == nil {
		w.mu.Unlock()
		return nil
	}
	if u.status.State != UpdateDraining {
		u.status.State = UpdateDraining
		w.mu.Unlock()
		w.poke()
		return nil
	}
	busy := 0
	for _, t := range w.tasks {
		if !t.status.terminal() {
			busy++
		}
	}
	m := *u.staged
	w.mu.Unlock()
	if busy > 0 {
		return nil
	}
	return w.install(ctx, m)
}

// install swaps the staged binaries in, keeping the old ones, and asks
// for the new fleetci to be started, on trial.
func (w *Worker) install(ctx context.Context, m release.Manifest) error {
	u := w.update
	dir := filepath.Join(w.updateDir(), m.Version)
	// A binary that can't even say its version is not swapped in.
	out, err := exec.CommandContext(ctx, filepath.Join(dir, "fleetci"), "version").Output()
	if err != nil || !strings.Contains(string(out), m.Version) {
		return w.giveUp(m.Version, fmt.Sprintf("its fleetci does not run here: %q %v", strings.TrimSpace(string(out)), err))
	}
	files := w.installedFiles()
	w.mu.Lock()
	var passed []string
	for c, ok := range w.passed {
		if ok {
			passed = append(passed, c)
		}
	}
	w.mu.Unlock()
	slices.Sort(passed)
	t := &trialMarker{From: w.version, To: m.Version, Files: map[string]string{}, Passed: passed, Started: time.Now()}
	if err := os.RemoveAll(w.previousDir()); err != nil {
		return err
	}
	if err := os.MkdirAll(w.previousDir(), 0o755); err != nil {
		return err
	}
	for name, path := range files {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			continue // the release does not have it
		}
		if err := copyFile(path, filepath.Join(w.previousDir(), name)); err != nil {
			return w.giveUp(m.Version, fmt.Sprintf("keeping the old %s: %v", name, err))
		}
		t.Files[name] = path
	}
	// The marker first: from here a stop leaves something to roll back by.
	if err := writeJSONFile(w.trialPath(), t); err != nil {
		return err
	}
	for name, path := range t.Files {
		if err := installFile(filepath.Join(dir, name), path); err != nil {
			return w.rollback(t, fmt.Sprintf("installing %s: %v", name, err))
		}
	}
	w.log.Info("update: installed; starting it on trial", "from", w.version, "to", m.Version)
	u.goAhead = ""
	return ErrRestart
}

// judgeTrialLocked commits a version on trial that reached a controller,
// passed its checks and stayed up, and rolls back one that failed.
func (w *Worker) judgeTrialLocked(t *trialMarker) error {
	u := w.update
	allPassed := true
	for _, c := range t.Passed {
		if strings.HasPrefix(w.checks[c], "FAIL") {
			w.mu.Unlock()
			err := w.rollback(t, fmt.Sprintf("the microVM check of %s failed: %s", c, strings.TrimPrefix(w.checks[c], "FAIL: ")))
			w.mu.Lock()
			return err
		}
		allPassed = allPassed && w.passed[c]
	}
	switch {
	case !u.reachedAt.IsZero() && allPassed && time.Since(u.reachedAt) >= u.cfg.Trial:
		_ = os.Remove(w.trialPath())
		_ = os.RemoveAll(w.previousDir())
		_ = os.RemoveAll(filepath.Join(w.updateDir(), t.To))
		u.trial, u.status = nil, UpdateStatus{}
		w.log.Info("update: committed", "version", t.To, "from", t.From)
	case time.Since(t.Started) > trialWait+u.cfg.Trial:
		why := "it reached no controller"
		if !u.reachedAt.IsZero() {
			why = "its microVM checks did not pass"
		}
		w.mu.Unlock()
		err := w.rollback(t, why+" within "+trialWait.String())
		w.mu.Lock()
		return err
	}
	return nil
}

// reached marks that a controller took a report: a version on trial
// reaches the controllers.
func (w *Worker) reached() {
	if w.update == nil {
		return
	}
	w.mu.Lock()
	if w.update.trial != nil && w.update.reachedAt.IsZero() {
		w.update.reachedAt = time.Now()
	}
	w.mu.Unlock()
}

// rollback puts the old binaries back, skips the version from now on, and
// asks for the old fleetci to be started.
func (w *Worker) rollback(t *trialMarker, why string) error {
	w.log.Error("update: rolling back", "from", t.To, "to", t.From, "why", why)
	var errs []error
	for name, path := range t.Files {
		prev := filepath.Join(w.previousDir(), name)
		if _, err := os.Stat(prev); err != nil {
			continue
		}
		if err := installFile(prev, path); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	if len(errs) > 0 {
		// Leave the marker: the next start tries again.
		return fmt.Errorf("rolling back: %w", errors.Join(errs...))
	}
	if t.To != "" {
		w.recordFailed(t.To, why)
	}
	_ = os.Remove(w.trialPath())
	return ErrRestart
}

// giveUp skips a version that can't be installed, with nothing swapped.
func (w *Worker) giveUp(version, why string) error {
	w.log.Error("update: giving up", "to", version, "why", why)
	w.recordFailed(version, why)
	w.mu.Lock()
	w.update.staged, w.update.goAhead = nil, ""
	w.update.status = UpdateStatus{State: UpdateFailed, Target: version, Err: why}
	w.mu.Unlock()
	w.poke()
	return nil
}

func (w *Worker) failedVersions() map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(w.failedPath())
	if err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func (w *Worker) recordFailed(version, why string) {
	f := w.failedVersions()
	f[version] = why
	if err := os.MkdirAll(w.updateDir(), 0o755); err == nil {
		_ = writeJSONFile(w.failedPath(), f)
	}
}

// lastFailure is the newest failed version and why, for the reports.
func (w *Worker) lastFailure() (string, string) {
	best, why := "", ""
	for v, y := range w.failedVersions() {
		if best == "" || semver.Compare(v, best) > 0 {
			best, why = v, y
		}
	}
	return best, why
}

// installedFiles are the binaries an update replaces: this fleetci,
// fleetingd and cloud-hypervisor, where they are installed.
func (w *Worker) installedFiles() map[string]string {
	files := map[string]string{"fleetci": w.update.self}
	plugin := w.cfg.FleetingdPlugin
	if !filepath.IsAbs(plugin) {
		plugin, _ = exec.LookPath(plugin)
	}
	if plugin != "" {
		files["fleeting-plugin-fleetingd"] = plugin
		ch := filepath.Join(filepath.Dir(plugin), "cloud-hypervisor")
		if _, err := os.Stat(ch); err != nil {
			ch, _ = exec.LookPath("cloud-hypervisor")
		}
		if ch != "" {
			files["cloud-hypervisor"] = ch
		}
	}
	return files
}

// installFile copies src over dst, by a rename next to it, so a running
// binary keeps its old file.
func installFile(src, dst string) error {
	tmp := dst + ".fleetci-new"
	if err := copyFile(src, tmp); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// updateReport is the update's part of a report.
func (w *Worker) updateReportLocked() *UpdateStatus {
	if w.update == nil {
		return nil
	}
	s := w.update.status
	if s.State == UpdateIdle && s.Err == "" {
		if v, why := w.lastFailure(); v != "" && semver.Compare(v, w.version) > 0 {
			s = UpdateStatus{State: UpdateFailed, Target: v, Err: why}
		} else {
			return nil
		}
	}
	return &s
}

package cachedisk

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
)

// fakeImages writes each layer as a small file and remembers its parent.
type fakeImages struct {
	mu      sync.Mutex
	parent  map[string]string
	data    int // bytes each new layer gets, as if a job wrote them
	flatten chan struct{}
	fail    error
	// flattenFail fails Flatten, as qemu-img did on a VM's locks.
	flattenFail error
}

func (f *fakeImages) Create(path, parent string, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	if f.parent == nil {
		f.parent = map[string]string{}
	}
	f.parent[path] = parent
	return os.WriteFile(path, make([]byte, f.data), 0o600)
}

func (f *fakeImages) Flatten(src, base, dst string) error {
	if f.flatten != nil {
		<-f.flatten
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.flattenFail != nil {
		return f.flattenFail
	}
	f.parent[dst] = base
	return os.WriteFile(dst, nil, 0o600)
}

func (f *fakeImages) Rebase(path, parent string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.parent == nil {
		f.parent = map[string]string{}
	}
	f.parent[path] = parent
	return nil
}

func open(t *testing.T, cfg Config, img Images) *Store {
	t.Helper()
	if cfg.MinFree == 0 {
		cfg.MinFree = 1 // the test machine's free space mustn't count
	}
	s, err := Open(t.TempDir(), cfg, img, hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func run(t *testing.T, s *Store, sc Scope) *Lease {
	t.Helper()
	l, err := s.Begin(sc)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(l); err != nil {
		t.Fatal(err)
	}
	return l
}

var (
	main_ = Scope{Forge: "gitlab", Instance: "gitlab.com", Project: "1", Ref: "main", Writer: true, From: []string{"main"}}
	mr    = Scope{Forge: "gitlab", Instance: "gitlab.com", Project: "1", Ref: "feature", From: []string{"main"}}
)

func TestChain(t *testing.T) {
	img := &fakeImages{}
	s := open(t, Config{}, img)
	first := run(t, s, main_)
	if first.From() != "" || img.parent[first.Layer()] != "" {
		t.Fatalf("first layer from %s, parent %q", first.From(), img.parent[first.Layer()])
	}
	second := run(t, s, main_)
	if second.From() != "own" || img.parent[second.Layer()] != first.Layer() {
		t.Fatalf("second layer from %s, parent %q", second.From(), img.parent[second.Layer()])
	}
}

func TestMergeRequestReadsDefaultWritesOwn(t *testing.T) {
	img := &fakeImages{}
	s := open(t, Config{}, img)
	m := run(t, s, main_)
	l := run(t, s, mr)
	if l.From() != "main" || img.parent[l.Layer()] != m.Layer() {
		t.Fatalf("MR layer from %s, parent %q; want main's", l.From(), img.parent[l.Layer()])
	}
	// main's head is still main's own.
	next, err := s.Begin(main_)
	if err != nil {
		t.Fatal(err)
	}
	if img.parent[next.Layer()] != m.Layer() {
		t.Fatalf("main's next layer is on %q; the MR wrote main's cache", img.parent[next.Layer()])
	}
	s.Abort(next)
	// The MR's next job reads its own.
	again, _ := s.Begin(mr)
	if again.From() != "own" || img.parent[again.Layer()] != l.Layer() {
		t.Fatalf("MR's second layer from %s", again.From())
	}
}

func TestNonWriterOnMainIsNotTrusted(t *testing.T) {
	img := &fakeImages{}
	s := open(t, Config{}, img)
	run(t, s, main_)
	fake := main_
	fake.Writer = false
	l, _ := s.Begin(fake)
	// Its own cache is empty; it may read the shared one.
	if l.From() != "main" {
		t.Fatalf("non-writer on main from %q", l.From())
	}
	if err := s.Commit(l); err != nil {
		t.Fatal(err)
	}
	h, err := s.readHead(main_.key())
	if err != nil || h.layer() == l.Layer() {
		t.Fatalf("a non-writer wrote the shared cache")
	}
}

func TestAbortKeepsParent(t *testing.T) {
	img := &fakeImages{}
	s := open(t, Config{}, img)
	first := run(t, s, main_)
	l, _ := s.Begin(main_)
	s.Abort(l)
	if _, err := os.Stat(l.Layer()); !os.IsNotExist(err) {
		t.Fatalf("aborted layer is still there: %v", err)
	}
	next, _ := s.Begin(main_)
	if img.parent[next.Layer()] != first.Layer() {
		t.Fatal("after an abort, the next layer isn't on the last committed one")
	}
}

// Jobs of one pipeline run at once, each on a disk of its own: none undoes
// another's. A merge request's job starts from the same job's on main.
func TestEachJobHasItsOwn(t *testing.T) {
	img := &fakeImages{}
	s := open(t, Config{}, img)
	build, test := main_, main_
	build.Job, test.Job = "build", "test (node 24)"
	if build.key() == test.key() {
		t.Fatal("two jobs share a disk")
	}
	a, _ := s.Begin(build)
	b, _ := s.Begin(test)
	if err := s.Commit(a); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(b); err != nil {
		t.Fatal(err)
	}
	for i, sc := range []Scope{build, test} {
		l := []*Lease{a, b}[i]
		if h, err := s.readHead(sc.key()); err != nil || h.layer() != l.Layer() {
			t.Fatalf("%s lost its layer", sc.Label())
		}
	}
	feature := mr
	feature.Job = "test (node 24)"
	l, _ := s.Begin(feature)
	if l.From() != "main · test (node 24)" || img.parent[l.Layer()] != b.Layer() {
		t.Fatalf("MR's test job from %q, parent %q; want main's test job's", l.From(), img.parent[l.Layer()])
	}
	s.Abort(l)
	// A key the job chose names the disk whole, whatever the job.
	keyed := build
	keyed.Key = "deps"
	if keyed.Label() != "deps" {
		t.Fatalf("keyed disk is %q", keyed.Label())
	}
}

func TestLastFinisherWins(t *testing.T) {
	img := &fakeImages{}
	s := open(t, Config{}, img)
	base := run(t, s, main_)
	a, _ := s.Begin(main_)
	b, _ := s.Begin(main_)
	if err := s.Commit(b); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(a); err != nil {
		t.Fatal(err)
	}
	h, _ := s.readHead(main_.key())
	if h.layer() != a.Layer() {
		t.Fatal("head isn't the last committed layer")
	}
	// b is nobody's now, and gone; base is a's parent, and kept.
	if _, err := os.Stat(b.Layer()); !os.IsNotExist(err) {
		t.Fatal("the losing layer was kept")
	}
	if _, err := os.Stat(base.Layer()); err != nil {
		t.Fatal("the shared parent was removed")
	}
}

func TestFlatten(t *testing.T) {
	img := &fakeImages{}
	s := open(t, Config{MaxDepth: 3}, img)
	var layers []string
	for range 4 {
		layers = append(layers, run(t, s, main_).Layer())
	}
	s.Wait()
	h, _ := s.readHead(main_.key())
	if len(h.Chain) != 1 {
		t.Fatalf("chain of %d after flattening", len(h.Chain))
	}
	for _, f := range layers {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("%s outlived flattening", filepath.Base(f))
		}
	}
	if l := run(t, s, main_); img.parent[l.Layer()] != h.Chain[0] {
		t.Fatal("next layer isn't on the flattened one")
	}
}

// A merge request's disk, flattened, stays on the default branch's layers
// it started from: the two still share those blocks.
func TestFlattenKeepsSharedLayers(t *testing.T) {
	img := &fakeImages{}
	s := open(t, Config{MaxDepth: 6}, img)
	base := run(t, s, main_).Layer()
	for range 6 {
		run(t, s, mr)
	}
	s.Wait()
	h, _ := s.readHead(mr.key())
	if len(h.Chain) != 2 || h.Chain[0] != base || img.parent[h.Chain[1]] != base {
		t.Fatalf("chain %v after flattening; want main's layer and one of its own on it", h.Chain)
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("main's layer went: %v", err)
	}
}

// A job that commits while the chain is flattened doesn't leave it deep:
// the new head is flattened in turn.
func TestFlattenAgainWhenCommittedOver(t *testing.T) {
	img := &fakeImages{flatten: make(chan struct{})}
	s := open(t, Config{MaxDepth: 2}, img)
	run(t, s, main_)
	run(t, s, main_)
	run(t, s, main_) // starts flattening, which waits
	run(t, s, main_) // commits over it
	close(img.flatten)
	s.Wait()
	if h, _ := s.readHead(main_.key()); len(h.Chain) != 1 {
		t.Fatalf("chain of %d after flattening", len(h.Chain))
	}
}

// A chain too deep for a VM, its flattening having failed, is refused
// once and flattened for the next job.
func TestTooDeepIsFlattenedForTheNext(t *testing.T) {
	img := &fakeImages{flattenFail: errors.New("Failed to lock byte 100")}
	s := open(t, Config{}, img)
	for range chainLimit {
		run(t, s, main_)
		s.Wait()
	}
	img.mu.Lock()
	img.flattenFail = nil
	img.mu.Unlock()
	if l, err := s.Begin(main_); err == nil {
		t.Fatalf("a VM got a chain of %d layers", len(l.chain))
	}
	s.Wait()
	l, err := s.Begin(main_)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.chain) != 2 {
		t.Fatalf("chain of %d after flattening; want the flattened layer and the job's", len(l.chain))
	}
	s.Abort(l)
}

func TestFlattenKeepsLeasedChain(t *testing.T) {
	img := &fakeImages{flatten: make(chan struct{})}
	s := open(t, Config{MaxDepth: 2}, img)
	run(t, s, main_)
	run(t, s, main_)
	run(t, s, main_) // starts flattening, which waits
	busy, _ := s.Begin(main_)
	close(img.flatten)
	s.Wait()
	for _, f := range busy.chain {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("a layer a VM is using was removed: %v", err)
		}
	}
	s.Abort(busy)
}

func TestEvictLeastRecentlyUsed(t *testing.T) {
	img := &fakeImages{data: 64 << 10}
	s := open(t, Config{Budget: 150 << 10, MinFree: 1}, img)
	now := time.Now()
	s.now = func() time.Time { return now }
	other := main_
	other.Project = "2"
	run(t, s, main_)
	now = now.Add(time.Minute)
	run(t, s, other)
	now = now.Add(time.Minute)
	// A third cache goes over budget: main_, the oldest, goes.
	third := main_
	third.Project = "3"
	run(t, s, third)
	if _, err := s.readHead(main_.key()); err == nil {
		t.Fatal("the least recently used cache was kept")
	}
	if _, err := s.readHead(other.key()); err != nil {
		t.Fatal("a more recent cache was evicted")
	}
}

func TestEvictSkipsLeased(t *testing.T) {
	img := &fakeImages{data: 64 << 10}
	s := open(t, Config{Budget: 100 << 10, MinFree: 1}, img)
	now := time.Now()
	s.now = func() time.Time { return now }
	run(t, s, main_)
	busy, _ := s.Begin(main_)
	now = now.Add(time.Minute)
	other := main_
	other.Project = "2"
	run(t, s, other)
	for _, f := range busy.chain {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("evicted a cache a VM is using: %v", err)
		}
	}
	s.Abort(busy)
}

func TestMaxAge(t *testing.T) {
	img := &fakeImages{}
	s := open(t, Config{MaxAge: time.Hour}, img)
	now := time.Now()
	s.now = func() time.Time { return now }
	run(t, s, main_)
	now = now.Add(2 * time.Hour)
	l, _ := s.Begin(mr)
	if l.From() != "" {
		t.Fatalf("a cache past max_age was read (from %s)", l.From())
	}
}

// A merge request's disk goes after ReaderMaxAge; the default branch's,
// used before it, stays until MaxAge.
func TestReaderMaxAge(t *testing.T) {
	img := &fakeImages{}
	s := open(t, Config{MaxAge: 10 * time.Hour, ReaderMaxAge: time.Hour}, img)
	now := time.Now()
	s.now = func() time.Time { return now }
	run(t, s, main_)
	now = now.Add(time.Minute)
	run(t, s, mr)
	now = now.Add(2 * time.Hour)
	other := main_
	other.Project = "2"
	run(t, s, other) // evicts on Begin
	if _, err := s.readHead(mr.key()); err == nil {
		t.Fatal("the merge request's disk outlived reader_max_age")
	}
	if _, err := s.readHead(main_.key()); err != nil {
		t.Fatalf("the default branch's disk went before max_age: %v", err)
	}
}

func TestMaxPerCache(t *testing.T) {
	img := &fakeImages{data: 64 << 10}
	s := open(t, Config{MaxPerCache: 100 << 10}, img)
	run(t, s, main_)
	run(t, s, main_)
	l, _ := s.Begin(main_)
	if l.From() != "" {
		t.Fatalf("a cache over max_per_cache was read (from %s)", l.From())
	}
}

func TestBeginFails(t *testing.T) {
	img := &fakeImages{fail: errors.New("no space")}
	s := open(t, Config{}, img)
	if _, err := s.Begin(main_); err == nil {
		t.Fatal("Begin hid the error")
	}
	if len(s.leases) != 0 {
		t.Fatal("a failed Begin left a lease")
	}
}

func TestOpenSweepsLeftovers(t *testing.T) {
	img := &fakeImages{}
	dir := t.TempDir()
	s, _ := Open(dir, Config{MinFree: 1}, img, hclog.NewNullLogger())
	kept := run(t, s, main_)
	// A layer a stopped fleetci never committed.
	left, _ := s.Begin(main_)
	if _, err := Open(dir, Config{}, img, hclog.NewNullLogger()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(left.Layer()); !os.IsNotExist(err) {
		t.Fatal("an uncommitted layer survived a restart")
	}
	if _, err := os.Stat(kept.Layer()); err != nil {
		t.Fatal("a committed layer was swept")
	}
}

func TestOpenNeedsAbsolute(t *testing.T) {
	if _, err := Open("cache", Config{}, &fakeImages{}, hclog.NewNullLogger()); err == nil {
		t.Fatal("a relative directory was taken")
	}
}

func TestKeys(t *testing.T) {
	img := &fakeImages{}
	s := open(t, Config{}, img)
	deps := main_
	deps.Key = "deps"
	d := run(t, s, deps)
	if d.From() != "" {
		t.Fatalf("a new key started from %q", d.From())
	}
	// main's own cache is another one.
	if m := run(t, s, main_); m.From() != "" {
		t.Fatalf("main's cache started from %q; keys mixed", m.From())
	}
	// A merge request's job on key deps starts from the shared deps.
	mrDeps := mr
	mrDeps.Key = "deps"
	l, _ := s.Begin(mrDeps)
	if l.From() != "deps" || img.parent[l.Layer()] != d.Layer() {
		t.Fatalf("MR deps from %q", l.From())
	}
	s.Abort(l)
}

func TestFromOrder(t *testing.T) {
	img := &fakeImages{}
	s := open(t, Config{}, img)
	dev := main_
	dev.Ref = "develop"
	run(t, s, dev)
	run(t, s, main_)
	// A tag job that keeps one cache for all tags starts from develop's,
	// the first of its From that exists.
	tag := Scope{Forge: "gitlab", Instance: "gitlab.com", Project: "1", Ref: "v1.2.0", Key: "release", Writer: true, From: []string{"staging", "develop", "main"}}
	l := run(t, s, tag)
	if l.From() != "develop" {
		t.Fatalf("tag from %q, want develop", l.From())
	}
	tag.Ref = "v1.3.0"
	if l := run(t, s, tag); l.From() != "own" {
		t.Fatalf("the next tag from %q, want the shared release cache", l.From())
	}
}

func TestOtherProjectsDontMix(t *testing.T) {
	img := &fakeImages{}
	s := open(t, Config{}, img)
	run(t, s, main_)
	other := mr
	other.Project = "2"
	if l, _ := s.Begin(other); l.From() != "" {
		t.Fatalf("another project read this one's main (from %q)", l.From())
	}
}

// TestHeldNamesDirs: what a store holds goes by the names Dirs gives a
// job's caches, so a fleet can place the job where they are.
func TestHeldNamesDirs(t *testing.T) {
	s := open(t, Config{}, &fakeImages{})
	if len(s.Held()) != 0 {
		t.Fatalf("new store holds %v", s.Held())
	}
	run(t, s, main_)
	held := s.Held()
	dirs := mr.Dirs() // the MR's own, a shared one of its ref, then main's
	if len(dirs) != 3 || dirs[0] != mr.key() {
		t.Fatalf("dirs %v", dirs)
	}
	if _, ok := held[dirs[0]]; ok {
		t.Fatal("holds the MR's cache before its job")
	}
	if _, ok := held[dirs[2]]; !ok || len(held) != 1 {
		t.Fatalf("held %v; want main's, %s", held, dirs[2])
	}
}

// A chain left deep by a stopped fleetci is flattened when the store opens,
// before a job asks for it.
func TestDeepChainFlattenedAtOpen(t *testing.T) {
	s := open(t, Config{MaxDepth: 10}, &fakeImages{})
	for range 5 {
		run(t, s, main_)
	}
	again, err := Open(s.dir, Config{MaxDepth: 3, MinFree: 1}, &fakeImages{parent: map[string]string{}}, hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	again.Wait()
	if h, _ := again.readHead(main_.key()); len(h.Chain) != 1 {
		t.Fatalf("chain of %d after opening", len(h.Chain))
	}
}

// A flattening that failed is tried again on its own, not at the next
// job's commit.
func TestFailedFlattenIsRetried(t *testing.T) {
	defer func(d time.Duration) { flattenRetry = d }(flattenRetry)
	flattenRetry = 10 * time.Millisecond
	img := &fakeImages{flattenFail: errors.New("Failed to lock byte 100")}
	s := open(t, Config{MaxDepth: 2}, img)
	for range 3 {
		run(t, s, main_)
	}
	s.Wait()
	img.mu.Lock()
	img.flattenFail = nil
	img.mu.Unlock()
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		s.mu.Lock()
		h, _ := s.readHead(main_.key())
		s.mu.Unlock()
		if len(h.Chain) == 1 {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("chain of %d: the failed flattening wasn't tried again", len(h.Chain))
		}
	}
}

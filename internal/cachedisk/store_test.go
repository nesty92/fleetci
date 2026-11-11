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

func (f *fakeImages) Flatten(src, dst string) error {
	if f.flatten != nil {
		<-f.flatten
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parent[dst] = ""
	return os.WriteFile(dst, nil, 0o600)
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

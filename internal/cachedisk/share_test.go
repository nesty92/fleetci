package cachedisk

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fetchFrom fetches layers from another node's store.
func fetchFrom(src *Store, got *[]string) Fetch {
	return func(_ context.Context, path string, w io.Writer) error {
		*got = append(*got, path)
		f, err := src.OpenLayer(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	}
}

func commitJob(t *testing.T, s *Store, scope Scope) {
	t.Helper()
	l, err := s.Begin(scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(l); err != nil {
		t.Fatal(err)
	}
}

func offerOf(t *testing.T, s *Store, key string) Offer {
	t.Helper()
	for _, o := range s.Shared() {
		if o.Key == key {
			return o
		}
	}
	t.Fatalf("no offer of %s", key)
	return Offer{}
}

// TestPull: a node copies a neighbour's shared cache, then only the new
// layers of its newer copies, and points them at its own paths.
func TestPull(t *testing.T) {
	imgA, imgB := &fakeImages{data: 3 * sparseBlock}, &fakeImages{}
	a, b := open(t, Config{}, imgA), open(t, Config{}, imgB)
	// Something that isn't zeros, so the copy is checked byte for byte.
	main := Scope{Forge: "gitlab", Instance: "gitlab.com", Project: "1", Ref: "main", Writer: true}
	commitJob(t, a, main)
	o := offerOf(t, a, main.key())
	f := filepath.Join(a.dir, "scopes", o.Layers[0].Path)
	if err := os.WriteFile(f, append(make([]byte, sparseBlock), []byte("docker layers")...), 0o600); err != nil {
		t.Fatal(err)
	}
	o = offerOf(t, a, main.key())

	var got []string
	n, err := b.Pull(context.Background(), o, fetchFrom(a, &got))
	if err != nil || n != o.Size() || len(got) != 1 {
		t.Fatalf("first copy: %d bytes, %v, fetched %v", n, err, got)
	}
	want, _ := os.ReadFile(f)
	copied, _ := os.ReadFile(filepath.Join(b.dir, "scopes", o.Layers[0].Path))
	if string(copied) != string(want) {
		t.Fatal("the copy differs")
	}
	if c, ok := b.Has(main.key()); !ok || !c.Equal(o.Committed) {
		t.Fatalf("b's copy: %v %v", c, ok)
	}
	// As new as a's: nothing to copy.
	if n, err := b.Pull(context.Background(), o, fetchFrom(a, &got)); n != 0 || err != nil || len(got) != 1 {
		t.Fatalf("again: %d, %v", n, err)
	}

	// a's next job: b copies its new layer alone, onto its own copy of
	// the first.
	time.Sleep(time.Millisecond)
	commitJob(t, a, main)
	o2 := offerOf(t, a, main.key())
	got = nil
	if n, err := b.Pull(context.Background(), o2, fetchFrom(a, &got)); err != nil || n != o2.Layers[1].Size || len(got) != 1 || got[0] != o2.Layers[1].Path {
		t.Fatalf("second copy: %d, %v, fetched %v", n, err, got)
	}
	newest := filepath.Join(b.dir, "scopes", o2.Layers[1].Path)
	if p := imgB.parent[newest+".part"]; p != filepath.Join(b.dir, "scopes", o2.Layers[0].Path) {
		t.Fatalf("the new layer's parent here: %q", p)
	}
	// The next job on b starts from it.
	l, err := b.Begin(main)
	if err != nil || l.Start() != (Start{Own: true}) || len(l.chain) != 3 || l.chain[1] != newest {
		t.Fatalf("b's next job: %+v %v", l, err)
	}
	b.Abort(l)

	// A job on b kept a newer copy: a's older one isn't copied.
	commitJob(t, b, main)
	if n, err := b.Pull(context.Background(), o2, fetchFrom(a, &got)); n != 0 || err != nil {
		t.Fatalf("older copy: %d, %v", n, err)
	}
}

// TestShared: only writers' caches are offered or taken, by their own
// keys, and no other file is opened.
func TestShared(t *testing.T) {
	a := open(t, Config{}, &fakeImages{data: 10})
	mr := Scope{Forge: "gitlab", Instance: "gitlab.com", Project: "1", Ref: "mr-7"}
	commitJob(t, a, mr)
	if o := a.Shared(); len(o) != 0 {
		t.Fatalf("a ref's own cache is offered: %+v", o)
	}
	main := Scope{Forge: "gitlab", Instance: "gitlab.com", Project: "1", Ref: "main", Writer: true}
	commitJob(t, a, main)
	o := offerOf(t, a, main.key())

	for _, p := range []string{"../head.json", mr.key() + "/head.json", o.Key + "/head.json"} {
		if _, err := a.OpenLayer(p); err == nil {
			t.Fatalf("opened %s", p)
		}
	}
	b := open(t, Config{}, &fakeImages{})
	var got []string
	for name, bad := range map[string]func(*Offer){
		"not a writer's": func(o *Offer) { o.Scope.Writer = false },
		"another key":    func(o *Offer) { o.Key = mr.key() },
		"another dir":    func(o *Offer) { o.Layers[0].Path = "../../x.qcow2" },
	} {
		c := o
		c.Layers = append([]OfferLayer(nil), o.Layers...)
		bad(&c)
		if _, err := b.Pull(context.Background(), c, fetchFrom(a, &got)); err == nil {
			t.Fatalf("%s: taken", name)
		}
	}
	if len(got) != 0 {
		t.Fatalf("fetched %v", got)
	}
}

// TestPullNoRoom: a cache a node lacks is copied only into room it has
// spare; a failed copy leaves nothing behind.
func TestPullNoRoom(t *testing.T) {
	a := open(t, Config{}, &fakeImages{data: 1 << 20})
	main := Scope{Forge: "gitlab", Instance: "gitlab.com", Project: "1", Ref: "main", Writer: true}
	commitJob(t, a, main)
	f := filepath.Join(a.dir, "scopes", offerOf(t, a, main.key()).Layers[0].Path)
	if err := os.WriteFile(f, []byte(strings.Repeat("x", 1<<20)), 0o600); err != nil {
		t.Fatal(err)
	}
	o := offerOf(t, a, main.key())

	b := open(t, Config{Budget: 512 << 10}, &fakeImages{})
	var got []string
	if _, err := b.Pull(context.Background(), o, fetchFrom(a, &got)); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("over budget: %v", err)
	}

	c := open(t, Config{}, &fakeImages{})
	fail := func(_ context.Context, _ string, w io.Writer) error {
		_, _ = w.Write([]byte("half"))
		return errors.New("the neighbour went")
	}
	if _, err := c.Pull(context.Background(), o, fail); err == nil {
		t.Fatal("a failed copy succeeded")
	}
	short := func(_ context.Context, _ string, w io.Writer) error {
		_, err := w.Write([]byte("short"))
		return err
	}
	if _, err := c.Pull(context.Background(), o, short); err == nil || !strings.Contains(err.Error(), "bytes, not") {
		t.Fatalf("a short copy: %v", err)
	}
	if _, ok := c.Has(main.key()); ok || c.used() != 0 {
		t.Fatalf("a failed copy left %d bytes", c.used())
	}
}

// A layer a job trimmed is mostly holes: it is offered with what it
// stores, and a node takes it within a max_per_cache below its length.
func TestPullSparse(t *testing.T) {
	a := open(t, Config{}, &fakeImages{})
	main := Scope{Forge: "gitlab", Instance: "gitlab.com", Project: "1", Ref: "main", Writer: true}
	commitJob(t, a, main)
	f := filepath.Join(a.dir, "scopes", offerOf(t, a, main.key()).Layers[0].Path)
	w, err := os.OpenFile(f, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteAt([]byte(strings.Repeat("x", 64<<10)), 64<<20); err != nil {
		t.Fatal(err)
	}
	w.Close()
	o := offerOf(t, a, main.key())
	if l := o.Layers[0]; l.Size != 64<<20+64<<10 || l.Stored >= 1<<20 {
		t.Skipf("no holes on this filesystem: size %d, stored %d", l.Size, l.Stored)
	}

	b := open(t, Config{MaxPerCache: 8 << 20}, &fakeImages{})
	var got []string
	if _, err := b.Pull(context.Background(), o, fetchFrom(a, &got)); err != nil {
		t.Fatalf("a sparse layer within max_per_cache: %v", err)
	}
	if _, ok := b.Has(main.key()); !ok {
		t.Fatal("not kept")
	}
	if b.used() >= 8<<20 {
		t.Fatalf("the copy takes %d bytes: holes filled", b.used())
	}
}

// A neighbour's deep chain is flattened here as soon as it is copied, not
// when a job finds it too deep.
func TestPulledDeepChainIsFlattened(t *testing.T) {
	a, b := open(t, Config{MaxDepth: 10}, &fakeImages{}), open(t, Config{MaxDepth: 3}, &fakeImages{})
	main := Scope{Forge: "gitlab", Instance: "gitlab.com", Project: "1", Ref: "main", Writer: true}
	for range 5 {
		commitJob(t, a, main)
	}
	var got []string
	if _, err := b.Pull(context.Background(), offerOf(t, a, main.key()), fetchFrom(a, &got)); err != nil {
		t.Fatal(err)
	}
	b.Wait()
	if h, _ := b.readHead(main.key()); len(h.Chain) != 1 {
		t.Fatalf("copied chain of %d left deep", len(h.Chain))
	}
}

// TestPullNotAfterEvicting: a cache a node evicted, or would not take, is
// copied again only when the neighbour's is newer, so evicting and copying
// it back don't take turns.
func TestPullNotAfterEvicting(t *testing.T) {
	a, b := open(t, Config{}, &fakeImages{data: 10}), open(t, Config{}, &fakeImages{})
	main := Scope{Forge: "gitlab", Instance: "gitlab.com", Project: "1", Ref: "main", Job: "go", Writer: true}
	commitJob(t, a, main)
	o := offerOf(t, a, main.key())
	var got []string
	if n, err := b.Pull(context.Background(), o, fetchFrom(a, &got)); n == 0 || err != nil {
		t.Fatalf("first copy: %d, %v", n, err)
	}
	now := b.now
	b.now = func() time.Time { return now().Add(365 * 24 * time.Hour) }
	b.mu.Lock()
	b.evict()
	b.mu.Unlock()
	b.now = now
	if _, ok := b.Has(main.key()); ok {
		t.Fatal("not evicted")
	}
	got = nil
	if n, err := b.Pull(context.Background(), o, fetchFrom(a, &got)); n != 0 || err != nil || len(got) != 0 {
		t.Fatalf("the evicted copy came back: %d, %v, fetched %v", n, err, got)
	}
	time.Sleep(time.Millisecond)
	commitJob(t, a, main)
	if n, err := b.Pull(context.Background(), offerOf(t, a, main.key()), fetchFrom(a, &got)); n == 0 || err != nil {
		t.Fatalf("a newer copy: %d, %v", n, err)
	}

	small := open(t, Config{MaxPerCache: 1}, &fakeImages{})
	if _, err := small.Pull(context.Background(), o, fetchFrom(a, &got)); err == nil {
		t.Fatal("over max_per_cache: taken")
	}
	if n, err := small.Pull(context.Background(), o, fetchFrom(a, &got)); n != 0 || err != nil {
		t.Fatalf("over max_per_cache again: %d, %v", n, err)
	}
}

// A pull request's job on a node with none of its caches wants its own
// shared cache, then main's; fetched for the job, main's comes in even
// where the budget only has room once another cache goes, and the job
// starts from it.
func TestWantsAndPullForJob(t *testing.T) {
	a := open(t, Config{}, &fakeImages{data: 1 << 20})
	main := Scope{Forge: "gitlab", Instance: "gitlab.com", Project: "1", Ref: "main", Writer: true}
	commitJob(t, a, main)
	f := filepath.Join(a.dir, "scopes", offerOf(t, a, main.key()).Layers[0].Path)
	if err := os.WriteFile(f, []byte(strings.Repeat("x", 1<<20)), 0o600); err != nil {
		t.Fatal(err)
	}
	o := offerOf(t, a, main.key())

	b := open(t, Config{Budget: 3 << 19}, &fakeImages{data: 1 << 20})
	other := Scope{Forge: "gitlab", Instance: "gitlab.com", Project: "2", Ref: "main", Writer: true}
	commitJob(t, b, other)
	pr := Scope{Forge: "gitlab", Instance: "gitlab.com", Project: "1", Ref: "pr-1", From: []string{"main"}}
	want := b.Wants(pr)
	if len(want) != 2 || want[0] != pr.shared("pr-1") || want[1] != main.key() {
		t.Fatalf("wants %v", want)
	}
	if w := b.Wants(other); w != nil {
		t.Fatalf("has other's, wants %v", w)
	}

	var got []string
	if _, err := b.Pull(context.Background(), o, fetchFrom(a, &got)); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("a background pull made room: %v", err)
	}
	n, err := b.PullForJob(context.Background(), o, fetchFrom(a, &got))
	if err != nil || n == 0 {
		t.Fatalf("for a job: %d, %v", n, err)
	}
	if _, ok := b.Has(other.key()); ok {
		t.Fatal("other's cache was kept over the budget")
	}
	if w := b.Wants(pr); w != nil {
		t.Fatalf("has main's, still wants %v", w)
	}
	l, err := b.Begin(pr)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Abort(l)
	if l.From() != "main" {
		t.Fatalf("starts from %q", l.From())
	}
}

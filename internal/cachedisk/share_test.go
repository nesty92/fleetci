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

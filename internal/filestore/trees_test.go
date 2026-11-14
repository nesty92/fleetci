package filestore

import (
	"context"
	"strings"
	"testing"
)

// A snapshot's listings come in one round of reads, however deep its
// folders go, and a listing changed in the bucket is not used.
func TestSnapReadsListingsAtOnce(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	deep := strings.Repeat("d/", 30) + "f"
	writeTree(t, root, map[string]string{deep: "deep", "a/b": "b", "c": "c"})
	r := &memRemote{objs: map[string][]byte{}}
	if _, _, err := New(r, "p").Save(ctx, root, "s1", nil); err != nil {
		t.Fatal(err)
	}

	var st Stats
	snap, err := New(r, "p").Snap(ctx, "s1", &st)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Entries) != 34 {
		t.Fatalf("%d entries, want 34", len(snap.Entries))
	}
	// The snapshot, the listings' one span, the index's one object.
	if st.Get != 3 {
		t.Fatalf("%d reads, want 3", st.Get)
	}

	// Spoil the packs: Snap must fail rather than list it.
	for k, v := range r.objs {
		if strings.HasPrefix(k, "p/packs/") {
			for i := range v {
				v[i] ^= 0xff
			}
		}
	}
	if _, err := New(r, "p").Snap(ctx, "s1", &Stats{}); err == nil {
		t.Fatal("Snap used spoilt listings")
	}
}

package cluster

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/cachedisk"
)

// testImages makes each new layer 256 KiB of random data, as if a job
// wrote it.
type testImages struct{}

func (testImages) Create(path, _ string, _ int64) error {
	b := make([]byte, 256<<10)
	_, _ = rand.Read(b)
	return os.WriteFile(path, b, 0o600)
}
func (testImages) Flatten(_, _, dst string) error { return os.WriteFile(dst, nil, 0o600) }
func (testImages) Rebase(string, string) error { return nil }

func testStore(t *testing.T) *cachedisk.Store {
	t.Helper()
	s, err := cachedisk.Open(t.TempDir(), cachedisk.Config{MinFree: 1}, testImages{}, hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// servePeer answers peers as w1 would, with h.
func servePeer(t *testing.T, pk PKI, n *neighbourState, h http.Handler) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := pk.Pool()
	srv := &http.Server{Handler: h, TLSConfig: peerServerTLS(pk, p, n)}
	go func() { _ = srv.ServeTLS(lis, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	return lis.Addr().String()
}

// TestShareOverPeers: w2 copies w1's shared cache over the peers' mTLS,
// and refuses a layer whose SHA-256 isn't the one sent after it.
func TestShareOverPeers(t *testing.T) {
	ca := newCA(t)
	p1, id1 := peerPKI(t, ca, "w1")
	p2, id2 := peerPKI(t, ca, "w2")
	pool, _ := p2.Pool()
	n1 := newNeighbourState()
	n1.set("ctl", &[]PeerHint{{ID: "w2", Serial: id2.Serial}})

	a, b := testStore(t), testStore(t)
	main := cachedisk.Scope{Forge: "gitlab", Instance: "gitlab.com", Project: "1", Ref: "main", Writer: true}
	l, err := a.Begin(main)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Commit(l); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	shareHandlers(mux, func() []*cachedisk.Store { return []*cachedisk.Store{a} })
	addr := servePeer(t, p1, n1, mux)

	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: peerClientTLS(p2, pool, PeerHint{ID: "w1", Serial: id1.Serial})}}
	ctx := context.Background()
	offs, err := neighbourOffers(ctx, hc, addr)
	if err != nil || len(offs) != 1 || offs[0].Key != main.Dirs()[0] {
		t.Fatalf("offers: %+v, %v", offs, err)
	}
	var wire int64
	n, err := b.Pull(ctx, offs[0], fetchLayer(hc, addr, true, &wire))
	if err != nil || n != 256<<10 || wire == 0 {
		t.Fatalf("copy: %d, %v, %d on the wire", n, err, wire)
	}
	// Uncompressed too.
	d := testStore(t)
	if n, err := d.Pull(ctx, offs[0], fetchLayer(hc, addr, false, &wire)); err != nil || n != 256<<10 {
		t.Fatalf("uncompressed copy: %d, %v", n, err)
	}
	if _, ok := b.Held()[offs[0].Key]; !ok {
		t.Fatal("w2 doesn't hold the copy")
	}
	// Path tricks get nothing.
	for _, p := range []string{"../../etc/passwd", offs[0].Key + "/head.json"} {
		err := fetchLayer(hc, addr, true, &wire)(ctx, p, io.Discard)
		if err == nil {
			t.Fatalf("fetched %s", p)
		}
	}

	// A neighbour whose layer isn't what its SHA-256 says.
	bad := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Trailer", layerSum)
		_, _ = w.Write([]byte("not the layer"))
		w.Header().Set(layerSum, strings.Repeat("0", 64))
	})
	badAddr := servePeer(t, p1, n1, bad)
	c := testStore(t)
	if _, err := c.Pull(ctx, offs[0], fetchLayer(hc, badAddr, false, &wire)); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("a bad layer: %v", err)
	}
	if len(c.Held()) != 0 {
		t.Fatal("a bad layer was kept")
	}

	// A neighbour that stops sending halfway, and one that sends more
	// than it offered: both copies end, and leave nothing.
	defer func(d time.Duration) { layerIdle = d }(layerIdle)
	layerIdle = 200 * time.Millisecond
	stall := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, 1000))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	long := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte{1}, int(offs[0].Size())+1))
	})
	for name, h := range map[string]http.Handler{"stalled": stall, "too long": long} {
		start := time.Now()
		_, err := c.Pull(ctx, offs[0], fetchLayer(hc, servePeer(t, p1, n1, h), false, &wire))
		if err == nil || time.Since(start) > 5*time.Second {
			t.Fatalf("%s: %v after %s", name, err, time.Since(start))
		}
		if len(c.Held()) != 0 {
			t.Fatalf("%s: kept", name)
		}
		// The store still serves jobs.
		l, err := c.Begin(main)
		if err != nil {
			t.Fatalf("%s: then Begin: %v", name, err)
		}
		c.Abort(l)
	}
}

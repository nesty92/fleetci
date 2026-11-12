package cluster

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nesty92/fleetci/internal/cachedisk"
)

// TestPeerHints: the controller names to each worker the others that
// answer peers, with their certificates' serials, and stops naming one
// once it is revoked.
func TestPeerHints(t *testing.T) {
	f := newTestFleet(t)
	status := func(cl *client, id string, addrs []string) StatusResponse {
		t.Helper()
		up := StatusUpdate{Facts: Facts{Arch: "amd64", CPUs: 4, MemoryMB: 8192, DiskMB: 100 << 10, Protocol: Protocol, PeerAddrs: addrs},
			Report: Report{State: Ready}}
		var resp StatusResponse
		if err := cl.do(context.Background(), http.MethodPut, workerPath(id, "/status"), up, &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if r := status(f.worker, "w1", []string{"10.0.0.1:9483"}); len(r.Peers) != 0 {
		t.Fatalf("w1 is named peers before any other reports: %+v", r.Peers)
	}
	r := status(f.other, "w2", []string{"10.0.0.2:9483"})
	if len(r.Peers) != 1 || r.Peers[0].ID != "w1" || r.Peers[0].Serial == "" || r.Peers[0].Addrs[0] != "10.0.0.1:9483" {
		t.Fatalf("w2's peers %+v", r.Peers)
	}
	if r := status(f.worker, "w1", []string{"10.0.0.1:9483"}); len(r.Peers) != 1 || r.Peers[0].ID != "w2" {
		t.Fatalf("w1's peers %+v", r.Peers)
	}
	if err := f.c.workerOp("revoke", "w1"); err != nil {
		t.Fatal(err)
	}
	if r := status(f.other, "w2", []string{"10.0.0.2:9483"}); len(r.Peers) != 0 {
		t.Fatalf("a revoked worker is named: %+v", r.Peers)
	}
}

// peerPKI is a worker's PKI, signed by ca.
func peerPKI(t *testing.T, ca PKI, id string) (PKI, Identity) {
	t.Helper()
	p := PKI{Dir: t.TempDir()}
	csr, err := p.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	der, err := ca.Sign(csr, RoleWorker, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := ca.CA()
	if err := p.Install(der, caCert.Raw); err != nil {
		t.Fatal(err)
	}
	c, _ := p.Cert()
	who, err := IdentityOf(c)
	if err != nil {
		t.Fatal(err)
	}
	return p, who
}

func newCA(t *testing.T) PKI {
	t.Helper()
	p := PKI{Dir: t.TempDir()}
	if _, err := p.InitCA(); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestProbePeer: a worker reaches a peer the controllers named to it, and
// that named it back; not one whose certificate is another's, or from
// another fleet, nor one that wasn't told of it.
func TestProbePeer(t *testing.T) {
	ca := newCA(t)
	p1, id1 := peerPKI(t, ca, "w1")
	p2, id2 := peerPKI(t, ca, "w2")
	_, id3 := peerPKI(t, ca, "w3")
	pool, err := p1.Pool()
	if err != nil {
		t.Fatal(err)
	}

	// w1 answers, and the controllers named w2 to it.
	n1 := newNeighbourState()
	n1.set("ctl", &[]PeerHint{{ID: "w2", Serial: id2.Serial}})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: peerHandler("w1", func() []*cachedisk.Store { return nil }), TLSConfig: peerServerTLS(p1, pool, n1)}
	go func() { _ = srv.ServeTLS(lis, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	addr := lis.Addr().String()
	ctx := context.Background()

	got, err := probePeer(ctx, p2, pool, PeerHint{ID: "w1", Serial: id1.Serial, Addrs: []string{"127.0.0.1:1", addr}}, "")
	if err != nil || got.ID != "w1" || got.Addr != addr || got.RTT <= 0 || got.RTT > time.Second {
		t.Fatalf("w2 reaching w1: %+v, %v", got, err)
	}
	// The controllers named another certificate of w1's: a stolen key's
	// or an old one.
	if _, err := probePeer(ctx, p2, pool, PeerHint{ID: "w1", Serial: id3.Serial, Addrs: []string{addr}}, ""); err == nil || !strings.Contains(err.Error(), "not the w1") {
		t.Fatalf("w1 with another serial: %v", err)
	}
	// A worker of another fleet, with a certificate of the same name.
	other := newCA(t)
	pOut, _ := peerPKI(t, other, "w2")
	outPool, _ := pOut.Pool()
	if _, err := probePeer(ctx, pOut, outPool, PeerHint{ID: "w1", Serial: id1.Serial, Addrs: []string{addr}}, ""); err == nil {
		t.Fatal("another fleet's worker reached w1")
	}
	// A worker of this fleet the controllers didn't name to w1.
	p3, _ := peerPKI(t, ca, "w3")
	if _, err := probePeer(ctx, p3, pool, PeerHint{ID: "w1", Serial: id1.Serial, Addrs: []string{addr}}, ""); err == nil {
		t.Fatal("w3 reached w1, which wasn't told of it")
	}
	// Once the controllers stop naming w2, w1 turns it away.
	n1.set("ctl", &[]PeerHint{})
	if _, err := probePeer(ctx, p2, pool, PeerHint{ID: "w1", Serial: id1.Serial, Addrs: []string{addr}}, ""); err == nil {
		t.Fatal("w2 reached w1 after the controllers stopped naming it")
	}
}

// TestPeerAddrs: a worker tells its peers the address it listens at, or
// its own in the peer network, with its port.
func TestPeerAddrs(t *testing.T) {
	pn := &peerNet{addrs: []net.IP{net.ParseIP("192.168.1.5"), net.ParseIP("2001:db8::5")}}
	got := peerAddrs(&net.TCPAddr{IP: net.IPv4zero, Port: 9483}, pn)
	if len(got) != 2 || got[0] != "192.168.1.5:9483" || got[1] != "[2001:db8::5]:9483" {
		t.Fatalf("own addresses: %v", got)
	}
	if got := peerAddrs(&net.TCPAddr{IP: net.ParseIP("192.168.1.5"), Port: 9484}, pn); len(got) != 1 || got[0] != "192.168.1.5:9484" {
		t.Fatalf("a listener on one address: %v", got)
	}
}

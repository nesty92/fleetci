package cluster

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nesty92/fleetci/internal/cachedisk"
)

// Workers find which of them can reach each other: the controllers name
// every worker's peers, and each worker checks which it reaches and how
// fast. A worker answers only workers the controllers named to it, and
// believes only the ones that show the fleet's certificate the controllers
// named: a revoked worker is named to no one. What the workers find is in
// their reports; nothing is copied between them yet.

// DefaultPeerPort is where workers answer each other.
const DefaultPeerPort = "9483"

// peerProbeEvery is how often a worker checks its peers again, besides
// when the controllers name others.
const peerProbeEvery = 30 * time.Second

// peerRetry is how soon a worker checks again after the controllers name
// others, which may not have been told of it yet.
const peerRetry = 6 * time.Second

// PeerHint is a worker the controllers name to another: its certificate's
// serial, and the addresses it says it answers peers at.
type PeerHint struct {
	ID     string   `json:"id"`
	Serial string   `json:"serial"`
	Addrs  []string `json:"addrs"`
}

// Neighbour is a peer a worker reached: the address, and the round trip
// of a request on an open connection.
type Neighbour struct {
	ID   string        `json:"id"`
	Addr string        `json:"addr"`
	RTT  time.Duration `json:"rtt"`
}

// peerHintsLocked are the workers to name to worker self: those that
// report, answer peers and aren't revoked.
func (c *Controller) peerHintsLocked(self string) []PeerHint {
	var out []PeerHint
	for _, w := range c.workers {
		if w.id == self || w.serial == "" || len(w.facts.PeerAddrs) == 0 || w.state() == Unreachable {
			continue
		}
		if _, revoked := c.revoked[w.serial]; revoked {
			continue
		}
		out = append(out, PeerHint{ID: w.id, Serial: w.serial, Addrs: w.facts.PeerAddrs})
	}
	slices.SortFunc(out, func(a, b PeerHint) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// peerAddrs are the addresses a worker listening at lis tells its peers:
// the one it listens at, or its own in the network.
func peerAddrs(lis net.Addr, pn *peerNet) []string {
	host, port, _ := net.SplitHostPort(lis.String())
	if ip := net.ParseIP(host); ip != nil && !ip.IsUnspecified() {
		return []string{lis.String()}
	}
	var out []string
	for _, ip := range pn.addrs {
		out = append(out, net.JoinHostPort(ip.String(), port))
	}
	return out
}

// neighbourState is what a worker knows of its peers.
type neighbourState struct {
	mu         sync.Mutex
	hints      map[string][]PeerHint // controller address → the peers it names
	neighbours []Neighbour
	renamed    chan struct{} // a controller named others
}

func newNeighbourState() *neighbourState {
	return &neighbourState{hints: map[string][]PeerHint{}, renamed: make(chan struct{}, 1)}
}

// set records the peers a controller names, nil for one out of reach.
func (p *neighbourState) set(addr string, hints *[]PeerHint) {
	if p == nil {
		return
	}
	p.mu.Lock()
	before := p.unionLocked()
	if hints == nil {
		delete(p.hints, addr)
	} else {
		p.hints[addr] = *hints
	}
	changed := !slices.EqualFunc(before, p.unionLocked(), func(a, b PeerHint) bool {
		return a.ID == b.ID && a.Serial == b.Serial && slices.Equal(a.Addrs, b.Addrs)
	})
	p.mu.Unlock()
	if changed {
		select {
		case p.renamed <- struct{}{}:
		default:
		}
	}
}

// unionLocked are the peers any controller names, by id.
func (p *neighbourState) unionLocked() []PeerHint {
	byID := map[string]PeerHint{}
	for _, addr := range slices.Sorted(maps.Keys(p.hints)) {
		for _, h := range p.hints[addr] {
			byID[h.ID] = h
		}
	}
	out := make([]PeerHint, 0, len(byID))
	for _, id := range slices.Sorted(maps.Keys(byID)) {
		out = append(out, byID[id])
	}
	return out
}

// named says whether a controller names the certificate's holder as a peer.
func (p *neighbourState) named(id Identity) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, hs := range p.hints {
		for _, h := range hs {
			if h.ID == id.ID && h.Serial == id.Serial {
				return true
			}
		}
	}
	return false
}

func (p *neighbourState) list() []Neighbour {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.neighbours)
}

// peerServerTLS answers peers: with this worker's certificate, to callers
// that show the fleet's and that a controller named.
func peerServerTLS(pk PKI, pool *x509.CertPool, peers *neighbourState) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			c, err := pk.TLSCert()
			return &c, err
		},
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  pool,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no certificate")
			}
			id, err := IdentityOf(cs.PeerCertificates[0])
			if err != nil {
				return err
			}
			if id.Role != RoleWorker || !peers.named(id) {
				return fmt.Errorf("%s is not a peer the controllers named", id.ID)
			}
			return nil
		},
	}
}

// peerClientTLS reaches peer h: its certificate must be the fleet's, a
// worker's, of h's id and serial. The address doesn't count, as with the
// controllers: a peer is reached by any address it has.
func peerClientTLS(pk PKI, pool *x509.CertPool, h PeerHint) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // replaced by VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the peer showed no certificate")
			}
			opts := x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
			if _, err := cs.PeerCertificates[0].Verify(opts); err != nil {
				return fmt.Errorf("the peer's certificate: %w", err)
			}
			id, err := IdentityOf(cs.PeerCertificates[0])
			if err != nil {
				return err
			}
			if id.Role != RoleWorker || id.ID != h.ID || id.Serial != h.Serial {
				return fmt.Errorf("the peer is %s (%s, serial %s), not the %s the controllers named", id.ID, id.Role, id.Serial, h.ID)
			}
			return nil
		},
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			c, err := pk.TLSCert()
			return &c, err
		},
	}
}

// peerHandler answers peers: who this worker is, and its shared caches.
func peerHandler(id string, stores func() []*cachedisk.Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /peer/v1/hello", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
	})
	shareHandlers(mux, stores)
	return mux
}

// probePeer reaches h at each of its addresses, and times a request on
// the open connection: the best of three. It answers the fastest address,
// or prev, the one in use, while that is about as fast.
func probePeer(ctx context.Context, pk PKI, pool *x509.CertPool, h PeerHint, prev string) (Neighbour, error) {
	var errs []string
	kept := Neighbour{}
	best := Neighbour{}
	for _, addr := range h.Addrs {
		tr := &http.Transport{TLSClientConfig: peerClientTLS(pk, pool, h), DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext}
		hc := &http.Client{Transport: tr, Timeout: 3 * time.Second}
		var rtt time.Duration
		var err error
		for i := range 4 {
			var req *http.Request
			req, err = http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+"/peer/v1/hello", nil)
			if err != nil {
				break
			}
			start := time.Now()
			var resp *http.Response
			if resp, err = hc.Do(req); err != nil {
				break
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				err = fmt.Errorf("%s: %s", addr, resp.Status)
				break
			}
			// The first opens the connection; the others time it.
			if d := time.Since(start); i > 0 && (rtt == 0 || d < rtt) {
				rtt = d
			}
		}
		tr.CloseIdleConnections()
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if addr == prev {
			kept = Neighbour{ID: h.ID, Addr: addr, RTT: rtt}
		}
		if best.ID == "" || rtt < best.RTT {
			best = Neighbour{ID: h.ID, Addr: addr, RTT: rtt}
		}
	}
	if best.ID == "" {
		return best, errors.New(strings.Join(errs, "; "))
	}
	if kept.ID != "" && kept.RTT <= 2*best.RTT+time.Millisecond {
		return kept, nil
	}
	return best, nil
}

// peerLoop answers peers at lis and checks the peers the controllers name,
// when they name others and every peerProbeEvery.
func (w *Worker) peerLoop(ctx context.Context, lis net.Listener, pool *x509.CertPool) {
	srv := &http.Server{Handler: peerHandler(w.cfg.ID, w.cacheStores), TLSConfig: peerServerTLS(w.pki, pool, w.peers),
		ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.ServeTLS(lis, "", "") }()
	defer srv.Close()
	t := time.NewTicker(peerProbeEvery)
	defer t.Stop()
	failing := map[string]string{} // peer → its last error, logged once
	missed := map[string]int{}     // peer → checks it failed in a row
	var soon <-chan time.Time
	for {
		w.peers.mu.Lock()
		hints := w.peers.unionLocked()
		prev := map[string]string{}
		for _, n := range w.peers.neighbours {
			prev[n.ID] = n.Addr
		}
		w.peers.mu.Unlock()
		found := make([]Neighbour, len(hints))
		errs := make([]error, len(hints))
		var wg sync.WaitGroup
		for i := range hints {
			// Only their addresses in this worker's network.
			hints[i].Addrs = w.peerNet.within(hints[i].Addrs)
		}
		for i, h := range hints {
			wg.Go(func() {
				pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				found[i], errs[i] = probePeer(pctx, w.pki, pool, h, prev[h.ID])
			})
		}
		wg.Wait()
		if ctx.Err() != nil {
			return
		}
		var out []Neighbour
		for i, h := range hints {
			if errs[i] != nil {
				// Logged once it fails twice: a peer just named may not
				// have been told of this worker yet.
				msg := errs[i].Error()
				if missed[h.ID]++; missed[h.ID] >= 2 && failing[h.ID] != msg {
					failing[h.ID] = msg
					w.log.Info("peer not reached", "peer", h.ID, "addrs", h.Addrs, "err", msg)
				}
				continue
			}
			delete(failing, h.ID)
			delete(missed, h.ID)
			out = append(out, found[i])
		}
		w.peers.mu.Lock()
		before := w.peers.neighbours
		w.peers.neighbours = out
		w.peers.mu.Unlock()
		if !slices.EqualFunc(before, out, func(a, b Neighbour) bool { return a.ID == b.ID && a.Addr == b.Addr }) {
			var names []string
			for _, n := range out {
				names = append(names, n.ID+" "+n.Addr+" "+n.RTT.Round(time.Microsecond).String())
			}
			w.log.Info("neighbours", "reached", names)
			w.poke()
			select {
			case w.shareKick <- struct{}{}:
			default:
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-soon:
		case <-w.peers.renamed:
			// The peers may hear of this worker a report later: a second
			// look then.
			soon = time.After(peerRetry)
		}
	}
}

// listenPeers finds the peer network and opens the peer listener, which
// takes connections only from it; nil when peer_listen is "off".
func (w *Worker) listenPeers() (net.Listener, error) {
	addr := w.cfg.PeerListen
	switch addr {
	case "off":
		return nil, nil
	case "":
		addr = ":" + DefaultPeerPort
	}
	ifs, err := localIfaces()
	if err != nil {
		return nil, err
	}
	pn, err := resolvePeerNet(w.cfg.PeerNetwork, ifs, routeIPs())
	if err != nil {
		return nil, err
	}
	if host, _, err := net.SplitHostPort(addr); err == nil && host != "" {
		if ip := net.ParseIP(host); ip == nil || (!ip.IsUnspecified() && !pn.contains(ip)) {
			return nil, fmt.Errorf("peer_listen %s is not in the peer network %s", addr, pn)
		}
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	w.peerNet = pn
	return netListener{lis, pn}, nil
}

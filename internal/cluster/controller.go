package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
)

// The controllers' timings.
const (
	pingEvery    = 3 * time.Second
	leaderTTL    = 10 * time.Second // a controller unheard of this long is not live
	unreachable  = 15 * time.Second // a worker unheard of this long is Unreachable
	tokenDefault = time.Hour
)

// Controller is one of the fleet's controllers: it accepts workers and
// controllers, and while it leads it finds jobs and places them.
type Controller struct {
	cfg     *ControllerConfig
	pki     PKI
	log     hclog.Logger
	version string

	// Sources run while this controller leads, until ctx ends.
	Sources func(ctx context.Context, c *Controller) error

	mu        sync.Mutex
	workers   map[string]*workerConn
	peers     map[string]peerState // controller id → what it last said
	leader    string
	tokens    map[string]TokenRecord
	revoked   map[string]Revoked     // serial → the certificate's holder
	drains    map[string]DrainRecord // worker id → whether it is drained
	wakeCh    chan struct{}          // closed on every worker status
	artifacts []Artifact
	epoch     string // this run's, in assignment revisions
	nextID    uint64
}

type peerState struct {
	seen      time.Time
	leader    string
	advertise []string
}

type workerConn struct {
	id       string
	serial   string
	facts    Facts
	fit      Fit
	report   Report
	seen     time.Time
	draining bool
	// assigns are the assignments this controller wrote for the worker;
	// rev counts their changes, and revWake is closed on each.
	assigns map[string]*assignment
	rev     uint64
	revWake chan struct{}
}

func (w *workerConn) state() string {
	switch {
	case w.seen.IsZero() || time.Since(w.seen) > unreachable:
		return Unreachable
	case w.draining:
		return Draining
	}
	return w.report.State
}

// NewController reads the controller's PKI and state.
func NewController(cfg *ControllerConfig, version string, log hclog.Logger) (*Controller, error) {
	p := PKI{Dir: cfg.Controller.PKIDir}
	if !p.HasCert() {
		return nil, fmt.Errorf("%s: no certificate; run fleetci init (the first controller) or fleetci join --controller", p.Dir)
	}
	cert, err := p.Cert()
	if err != nil {
		return nil, err
	}
	id, err := IdentityOf(cert)
	if err != nil {
		return nil, err
	}
	if id.Role != RoleController {
		return nil, fmt.Errorf("%s: the certificate is a %s's", p.Dir, id.Role)
	}
	if id.ID != cfg.Controller.ID {
		return nil, fmt.Errorf("controller.toml's id is %q, its certificate's %q", cfg.Controller.ID, id.ID)
	}
	if _, err := p.CAKey(); err != nil {
		return nil, fmt.Errorf("the CA key: %w", err)
	}
	c := &Controller{cfg: cfg, pki: p, log: log, version: version,
		workers: map[string]*workerConn{}, peers: map[string]peerState{},
		tokens: map[string]TokenRecord{}, revoked: map[string]Revoked{}, drains: map[string]DrainRecord{},
		wakeCh: make(chan struct{}), epoch: fmt.Sprintf("%x", time.Now().UnixNano())}
	if err := os.MkdirAll(cfg.Controller.StateDir, 0o700); err != nil {
		return nil, err
	}
	c.load()
	if c.artifacts, err = c.listArtifacts(); err != nil {
		return nil, err
	}
	return c, nil
}

// ID is the controller's id.
func (c *Controller) ID() string { return c.cfg.Controller.ID }

// Config is its config.
func (c *Controller) Config() *ControllerConfig { return c.cfg }

// Log is its logger.
func (c *Controller) Log() hclog.Logger { return c.log }

type savedState struct {
	Tokens  map[string]TokenRecord `json:"tokens"`
	Revoked map[string]Revoked     `json:"revoked"`
	Drains  map[string]DrainRecord `json:"drains,omitempty"`
}

func (c *Controller) statePath() string {
	return filepath.Join(c.cfg.Controller.StateDir, "controller-state.json")
}

func (c *Controller) load() {
	b, err := os.ReadFile(c.statePath())
	if err != nil {
		return
	}
	var s savedState
	if json.Unmarshal(b, &s) == nil {
		maps.Copy(c.tokens, s.Tokens)
		maps.Copy(c.revoked, s.Revoked)
		maps.Copy(c.drains, s.Drains)
	}
}

// saveLocked writes the tokens and revocations; c.mu is held.
func (c *Controller) saveLocked() {
	for k, t := range c.tokens {
		if time.Since(t.Expires) > 24*time.Hour {
			delete(c.tokens, k)
		}
	}
	for k, r := range c.revoked {
		if time.Now().After(r.Until) {
			delete(c.revoked, k)
		}
	}
	b, _ := json.Marshal(savedState{Tokens: c.tokens, Revoked: c.revoked, Drains: c.drains})
	tmp := c.statePath() + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, c.statePath())
	}
}

// listArtifacts finds the binaries joining workers install, with their sums.
func (c *Controller) listArtifacts() ([]Artifact, error) {
	var out []Artifact
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	paths := map[string]string{"fleetci": self}
	if d := c.cfg.Controller.Artifacts; d != "" {
		for _, name := range []string{"cloud-hypervisor", "fleeting-plugin-fleetingd"} {
			paths[name] = filepath.Join(d, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(paths)) {
		f, err := os.Open(paths[name])
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && name != "fleetci" {
				c.log.Warn("artifact missing: joining workers must have it already", "artifact", name, "path", paths[name])
				continue
			}
			return nil, err
		}
		h := sha256.New()
		n, err := io.Copy(h, f)
		f.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, Artifact{Name: name, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n})
	}
	return out, nil
}

func (c *Controller) artifactPath(name string) (string, bool) {
	if !slices.ContainsFunc(c.artifacts, func(a Artifact) bool { return a.Name == name }) {
		return "", false
	}
	if name == "fleetci" {
		p, err := os.Executable()
		return p, err == nil
	}
	return filepath.Join(c.cfg.Controller.Artifacts, name), true
}

// Run serves the fleet until ctx ends.
func (c *Controller) Run(ctx context.Context) error {
	tlsCfg, err := serverTLS(c.pki)
	if err != nil {
		return err
	}
	lis, err := net.Listen("tcp", c.cfg.Controller.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           c.handler(),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		HTTP2:             &http.HTTP2Config{SendPingTimeout: 10 * time.Second, PingTimeout: 5 * time.Second},
		ErrorLog:          c.log.StandardLogger(&hclog.StandardLoggerOptions{ForceLevel: hclog.Debug}),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close() // watches would hold a graceful shutdown for 30 s
	}()
	var wg sync.WaitGroup
	wg.Go(func() { c.pingLoop(ctx) })
	wg.Go(func() { c.leadLoop(ctx) })
	c.log.Info("controller serving", "id", c.ID(), "listen", c.cfg.Controller.Listen, "peers", c.cfg.Controller.Peers, "classes", slices.Sorted(maps.Keys(c.cfg.Classes)))
	err = srv.ServeTLS(lis, "", "")
	wg.Wait()
	if ctx.Err() != nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// pingLoop sends this controller's heartbeat to every peer.
func (c *Controller) pingLoop(ctx context.Context) {
	pool, err := c.pki.Pool()
	if err != nil {
		c.log.Error("ca", "err", err)
		return
	}
	tlsCfg := clientTLS(c.pki, pool)
	conns := map[string]*client{}
	defer func() {
		for _, cl := range conns {
			cl.close()
		}
	}()
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		var wg sync.WaitGroup
		for _, addr := range c.cfg.Controller.Peers {
			cl := conns[addr]
			if cl == nil {
				cl = newClient(addr, tlsCfg)
				conns[addr] = cl
			}
			wg.Go(func() {
				pctx, cancel := context.WithTimeout(ctx, pingEvery)
				defer cancel()
				var out pingMsg
				if err := cl.do(pctx, http.MethodPost, "/v1/ping", c.pingMsg(), &out); err == nil {
					c.merge(out)
				}
			})
		}
		wg.Wait()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Controller) pingMsg() pingMsg {
	c.mu.Lock()
	defer c.mu.Unlock()
	return pingMsg{ID: c.ID(), Leader: c.leader, Advertise: c.cfg.Controller.Advertise,
		Revoked: maps.Clone(c.revoked), Tokens: maps.Clone(c.tokens), Drains: maps.Clone(c.drains)}
}

// merge takes in what a peer said: it is live, and its tokens and
// revocations hold here too.
func (c *Controller) merge(p pingMsg) {
	if p.ID == "" || p.ID == c.ID() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.peers[p.ID] = peerState{seen: time.Now(), leader: p.Leader, advertise: p.Advertise}
	changed := false
	for k, r := range p.Revoked {
		if _, ok := c.revoked[k]; !ok {
			c.revoked[k] = r
			changed = true
		}
	}
	for k, t := range p.Tokens {
		mine, ok := c.tokens[k]
		if !ok || (mine.Used == "" && t.Used != "") {
			c.tokens[k] = t
			changed = true
		}
	}
	for id, d := range p.Drains {
		if mine, ok := c.drains[id]; !ok || d.At.After(mine.At) {
			c.drains[id] = d
			if w := c.workers[id]; w != nil {
				w.draining = d.Drained
			}
			changed = true
		}
	}
	if changed {
		c.saveLocked()
	}
}

// electWait is how long a controller that starts listens for a leader
// before it may claim the lead itself.
const electWait = pingEvery + time.Second

// elect works out who leads. A leader keeps the lead while it is live, as
// a Kubernetes lease holder does: a controller that comes back does not
// take it back, which would move every job in flight twice. With no live
// leader, the live controller with the lowest id takes it; of two that
// both lead, after a partition, the lower keeps it. A controller that has
// just started claims nothing until it has heard its peers.
func elect(self, current string, peers map[string]peerState, started, now time.Time) string {
	live := []string{self}
	claims := []string{}
	if current == self {
		claims = append(claims, self)
	}
	for id, p := range peers {
		if now.Sub(p.seen) >= leaderTTL {
			continue
		}
		live = append(live, id)
		if p.leader == id {
			claims = append(claims, id)
		}
	}
	if len(claims) > 0 {
		return slices.Min(claims)
	}
	if now.Sub(started) < electWait {
		return ""
	}
	return slices.Min(live)
}

// leadLoop works out who leads, and runs the sources while that is this
// controller.
func (c *Controller) leadLoop(ctx context.Context) {
	var stop context.CancelFunc
	var done chan struct{}
	started := time.Now()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		c.mu.Lock()
		leader := elect(c.ID(), c.leader, c.peers, started, time.Now())
		changed := leader != c.leader
		c.leader = leader
		c.mu.Unlock()
		if changed && leader != "" {
			c.log.Info("leader", "leader", leader, "self", leader == c.ID())
		}
		switch {
		case leader == c.ID() && stop == nil && c.Sources != nil:
			sctx, cancel := context.WithCancel(ctx)
			stop, done = cancel, make(chan struct{})
			go func(done chan struct{}) {
				defer close(done)
				// A start gives the workers time to connect and report.
				select {
				case <-sctx.Done():
					return
				case <-time.After(c.settleTime()):
				}
				c.log.Info("leading: finding and placing jobs")
				if err := c.Sources(sctx, c); err != nil && sctx.Err() == nil {
					c.log.Error("job sources stopped", "err", err)
				}
			}(done)
		case leader != c.ID() && stop != nil:
			c.log.Info("no longer leading: stopping the job sources", "leader", leader)
			stop()
			<-done
			stop = nil
		case stop != nil:
			select {
			case <-done:
				stop = nil // the sources failed; start them again
			default:
			}
		}
		select {
		case <-ctx.Done():
			if stop != nil {
				stop()
				<-done
			}
			return
		case <-t.C:
		}
	}
}

func (c *Controller) settleTime() time.Duration { return 6 * time.Second }

func (c *Controller) isRevoked(serial string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.revoked[serial]
	return ok
}

// CreateToken makes a join token for a worker or, with role controller, a
// controller.
func (c *Controller) CreateToken(role string, ttl time.Duration) (string, error) {
	ca, err := c.pki.CA()
	if err != nil {
		return "", err
	}
	if ttl <= 0 {
		ttl = tokenDefault
	}
	if role == "" {
		role = RoleWorker
	}
	t := NewToken(CAHash(ca))
	c.mu.Lock()
	c.tokens[t.ID] = TokenRecord{Hash: HashSecret(t.Secret), Role: role, Expires: time.Now().Add(ttl)}
	c.saveLocked()
	c.mu.Unlock()
	return t.String(), nil
}

// join signs a joining machine's CSR with its token, once the classes say
// it has something to run.
func (c *Controller) join(req JoinRequest) (JoinResponse, error) {
	tid, secret, ok := strings.Cut(req.Token, ".")
	if !ok || req.ID == "" || strings.ContainsAny(req.ID, "/ ") {
		return JoinResponse{}, errors.New("a join needs a token and a machine id without / or spaces")
	}
	c.mu.Lock()
	t, ok := c.tokens[tid]
	c.mu.Unlock()
	switch {
	case !ok || !SecretMatches(secret, t.Hash):
		return JoinResponse{}, errors.New("unknown token")
	case time.Now().After(t.Expires):
		return JoinResponse{}, errors.New("the token expired; make another with fleetci token create")
	case t.Used != "" && t.Used != req.ID:
		return JoinResponse{}, fmt.Errorf("the token was used by %s already", t.Used)
	}
	resp := JoinResponse{Role: t.Role, Manifest: c.manifest(req.Facts, t.Role)}
	if t.Role == RoleWorker && len(resp.Manifest.Fit.Classes) == 0 {
		var why []string
		for _, name := range slices.Sorted(maps.Keys(resp.Manifest.Fit.Refused)) {
			why = append(why, name+": "+resp.Manifest.Fit.Refused[name])
		}
		return JoinResponse{}, errors.New("no class fits this machine: " + strings.Join(why, "; "))
	}
	var err error
	if resp.Cert, err = c.pki.Sign(req.CSR, t.Role, req.ID, req.Facts.Hosts); err != nil {
		return JoinResponse{}, err
	}
	ca, err := c.pki.CA()
	if err != nil {
		return JoinResponse{}, err
	}
	resp.CA = ca.Raw
	if t.Role == RoleController {
		if resp.CAKey, err = c.pki.CAKeyDER(); err != nil {
			return JoinResponse{}, err
		}
	}
	c.mu.Lock()
	t.Used = req.ID
	c.tokens[tid] = t
	c.saveLocked()
	c.mu.Unlock()
	c.log.Info("machine joined", "id", req.ID, "role", t.Role, "classes", classNames(resp.Manifest.Fit.Classes))
	return resp, nil
}

func classNames(cs []Class) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

// manifest is what a machine with these facts installs and runs.
func (c *Controller) manifest(f Facts, role string) Manifest {
	m := Manifest{Version: c.version, Artifacts: c.artifacts, Controllers: c.controllers()}
	if role == RoleWorker {
		m.Fit = c.fit(f)
	}
	return m
}

// controllers are every controller's addresses, this one's first.
func (c *Controller) controllers() []string {
	out := slices.Clone(c.cfg.Controller.Advertise)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range slices.Sorted(maps.Keys(c.peers)) {
		for _, a := range c.peers[id].advertise {
			if !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	}
	for _, a := range c.cfg.Controller.Peers {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

func (c *Controller) fit(f Facts) Fit {
	fit := FitClasses(c.cfg.Classes, f)
	if c.cfg.GitHub != nil {
		fit.GitHub = true
		fit.Reach = append(fit.Reach, "https://github.com", "https://api.github.com", "https://pipelines.actions.githubusercontent.com")
	}
	if c.cfg.GitLab != nil {
		fit.Reach = append(fit.Reach, strings.TrimRight(c.cfg.GitLab.URL, "/")+"/")
	}
	return fit
}

func (c *Controller) isRevokedLocked(serial string) bool {
	_, ok := c.revoked[serial]
	return ok
}

// wake wakes everything waiting on the workers: for room, or for an
// assignment's status.
func (c *Controller) wake() {
	c.mu.Lock()
	close(c.wakeCh)
	c.wakeCh = make(chan struct{})
	c.mu.Unlock()
}

// changed is closed at the next worker status.
func (c *Controller) changed() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wakeCh
}

// Status is the fleet as this controller sees it.
func (c *Controller) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := Status{Controller: c.ID(), Leader: c.leader, Controllers: map[string]time.Time{c.ID(): time.Now()}}
	for id, p := range c.peers {
		st.Controllers[id] = p.seen
	}
	for _, id := range slices.Sorted(maps.Keys(c.workers)) {
		w := c.workers[id]
		st.Workers = append(st.Workers, WorkerStatus{ID: id, State: w.state(), Seen: w.seen, Facts: w.facts, Fit: w.fit, Report: w.report, Draining: w.draining})
	}
	return st
}

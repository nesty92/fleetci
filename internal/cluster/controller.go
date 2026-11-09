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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"google.golang.org/grpc"
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
	revoked   map[string]Revoked // serial → the certificate's holder
	watch     map[string]chan Event
	roomWake  chan struct{}
	artifacts []Artifact
}

type peerState struct {
	seen      time.Time
	leader    string
	advertise []string
}

type workerConn struct {
	id        string
	serial    string
	s         *stream
	facts     Facts
	fit       Fit
	report    Report
	seen      time.Time
	connected bool
	draining  bool
	asked     map[string]int // creates in flight, by class
}

func (w *workerConn) state() string {
	switch {
	case !w.connected || time.Since(w.seen) > unreachable:
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
		tokens: map[string]TokenRecord{}, revoked: map[string]Revoked{},
		watch: map[string]chan Event{}, roomWake: make(chan struct{})}
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
	b, _ := json.Marshal(savedState{Tokens: c.tokens, Revoked: c.revoked})
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
	srv := newServer(tlsCfg, c)
	go func() {
		<-ctx.Done()
		srv.Stop()
	}()
	var wg sync.WaitGroup
	wg.Go(func() { c.pingLoop(ctx) })
	wg.Go(func() { c.leadLoop(ctx) })
	c.log.Info("controller serving", "id", c.ID(), "listen", c.cfg.Controller.Listen, "peers", c.cfg.Controller.Peers, "classes", slices.Sorted(maps.Keys(c.cfg.Classes)))
	err = srv.Serve(lis)
	wg.Wait()
	if ctx.Err() != nil {
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
	conns := map[string]*grpc.ClientConn{}
	defer func() {
		for _, cc := range conns {
			cc.Close()
		}
	}()
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		for _, addr := range c.cfg.Controller.Peers {
			cc := conns[addr]
			if cc == nil {
				if cc, err = dial(addr, clientTLS(c.pki, pool)); err != nil {
					continue
				}
				conns[addr] = cc
			}
			pctx, cancel := context.WithTimeout(ctx, pingEvery)
			var out pingMsg
			err := call(pctx, cc, mPing, "ping", c.pingMsg(), &out)
			cancel()
			if err == nil {
				c.merge(out)
			}
		}
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
		Revoked: maps.Clone(c.revoked), Tokens: maps.Clone(c.tokens)}
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
			for _, w := range c.workers {
				if w.serial == k && w.s != nil {
					w.connected = false
				}
			}
		}
	}
	for k, t := range p.Tokens {
		mine, ok := c.tokens[k]
		if !ok || (mine.Used == "" && t.Used != "") {
			c.tokens[k] = t
			changed = true
		}
	}
	if changed {
		c.saveLocked()
	}
}

// leadLoop works out who leads: the live controller with the lowest id. It
// runs the sources while that is this one.
func (c *Controller) leadLoop(ctx context.Context) {
	var stop context.CancelFunc
	var done chan struct{}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		leader := c.ID()
		c.mu.Lock()
		for id, p := range c.peers {
			if time.Since(p.seen) < leaderTTL && id < leader {
				leader = id
			}
		}
		changed := leader != c.leader
		c.leader = leader
		c.mu.Unlock()
		if changed {
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

// unary serves Join, Ping and Admin.
func (c *Controller) unary(ctx context.Context, method string, in *Msg) (*Msg, error) {
	id, hasID := peerIdentity(ctx)
	if hasID && c.isRevoked(id.Serial) {
		return reply(nil, errors.New("this certificate is revoked"))
	}
	switch method {
	case mJoin:
		var req JoinRequest
		if err := in.Decode(&req); err != nil {
			return nil, err
		}
		return reply(c.join(req))
	case mPing:
		if !hasID || id.Role != RoleController {
			return reply(nil, errors.New("ping is for controllers"))
		}
		var p pingMsg
		if err := in.Decode(&p); err != nil {
			return nil, err
		}
		if p.ID != id.ID {
			return reply(nil, errors.New("ping names another controller"))
		}
		c.merge(p)
		return reply(c.pingMsg(), nil)
	case mAdmin:
		if !hasID || id.Role != RoleController {
			return reply(nil, errors.New("fleetci's commands need a controller's certificate"))
		}
		var req adminReq
		if err := in.Decode(&req); err != nil {
			return nil, err
		}
		return reply(c.admin(ctx, req))
	}
	return nil, fmt.Errorf("no method %s", method)
}

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

// connect serves one worker's stream.
func (c *Controller) connect(ss grpc.ServerStream) error {
	id, ok := peerIdentity(ss.Context())
	if !ok || id.Role != RoleWorker {
		return errors.New("connect is for workers, with their certificate")
	}
	if c.isRevoked(id.Serial) {
		return errors.New("this certificate is revoked")
	}
	s := newStream(ss)
	first := new(Msg)
	if err := ss.RecvMsg(first); err != nil {
		return err
	}
	var h Hello
	if first.Kind != "hello" || first.Decode(&h) != nil {
		return errors.New("a worker's stream starts with hello")
	}
	fit := c.fit(h.Facts)
	if err := s.tell("fit", fit); err != nil {
		return err
	}
	c.mu.Lock()
	w := c.workers[id.ID]
	draining := w != nil && w.draining
	w = &workerConn{id: id.ID, serial: id.Serial, s: s, facts: h.Facts, fit: fit, report: h.Report,
		seen: time.Now(), connected: true, draining: draining, asked: map[string]int{}}
	c.workers[id.ID] = w
	c.mu.Unlock()
	if draining {
		_ = s.tell("drain", true)
	}
	c.log.Info("worker connected", "worker", id.ID, "classes", classNames(fit.Classes), "refused", fit.Refused)
	c.wakeRoom()
	err := s.serve(func(m *Msg) { c.fromWorker(w, m) })
	c.mu.Lock()
	if c.workers[id.ID] == w {
		w.connected = false
	}
	c.mu.Unlock()
	c.log.Warn("worker stream ended", "worker", id.ID, "err", err)
	return nil
}

func (c *Controller) fromWorker(w *workerConn, m *Msg) {
	switch m.Kind {
	case "report":
		var r Report
		if m.Decode(&r) != nil {
			return
		}
		c.mu.Lock()
		if c.isRevokedLocked(w.serial) {
			w.connected = false
		}
		w.report, w.seen = r, time.Now()
		c.mu.Unlock()
		c.wakeRoom()
	case "event":
		var e Event
		if m.Decode(&e) != nil {
			return
		}
		c.mu.Lock()
		ch := c.watch[w.id+"/"+e.VM]
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- e:
			default:
			}
		}
	case "renew":
		var req renewReq
		_ = m.Decode(&req)
		cert, err := c.pki.Sign(req.CSR, RoleWorker, w.id, nil)
		_ = w.s.answer(m, renewResp{Cert: cert}, err)
		if err == nil {
			c.log.Info("worker certificate renewed", "worker", w.id)
		}
	default:
		if m.ID != 0 {
			_ = w.s.answer(m, nil, fmt.Errorf("unknown request %q", m.Kind))
		}
	}
}

func (c *Controller) isRevokedLocked(serial string) bool {
	_, ok := c.revoked[serial]
	return ok
}

// wakeRoom wakes everything waiting for room.
func (c *Controller) wakeRoom() {
	c.mu.Lock()
	close(c.roomWake)
	c.roomWake = make(chan struct{})
	c.mu.Unlock()
}

func (c *Controller) roomChanged() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.roomWake
}

// fetch streams an artifact to a machine that joined.
func (c *Controller) fetch(in *Msg, ss grpc.ServerStream) error {
	if _, ok := peerIdentity(ss.Context()); !ok {
		return errors.New("fetch needs a certificate: join first")
	}
	var name string
	_ = in.Decode(&name)
	path, ok := c.artifactPath(name)
	if !ok {
		return fmt.Errorf("no artifact %q", name)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			m, merr := newMsg("data", buf[:n])
			if merr != nil {
				return merr
			}
			if err := ss.SendMsg(m); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return ss.SendMsg(&Msg{Kind: "eof"})
		}
		if err != nil {
			return err
		}
	}
}

// admin runs one of fleetci's commands.
func (c *Controller) admin(ctx context.Context, req adminReq) (any, error) {
	switch req.Op {
	case "token":
		t, err := c.CreateToken(req.Role, req.TTL)
		if err != nil {
			return nil, err
		}
		return map[string]any{"token": t, "controllers": c.controllers()}, nil
	case "status":
		return c.Status(), nil
	case "drain", "undrain":
		c.mu.Lock()
		w := c.workers[req.Worker]
		if w != nil {
			w.draining = req.Op == "drain"
		}
		c.mu.Unlock()
		if w == nil {
			return nil, fmt.Errorf("no worker %q", req.Worker)
		}
		if w.connected {
			_ = w.s.tell("drain", req.Op == "drain")
		}
		return nil, nil
	case "revoke":
		c.mu.Lock()
		w := c.workers[req.Worker]
		if w == nil {
			c.mu.Unlock()
			return nil, fmt.Errorf("no worker %q", req.Worker)
		}
		c.revoked[w.serial] = Revoked{ID: w.id, Until: time.Now().Add(certLifetime)}
		w.connected = false
		c.saveLocked()
		c.mu.Unlock()
		c.log.Warn("worker certificate revoked", "worker", req.Worker)
		return nil, nil
	}
	return nil, fmt.Errorf("unknown command %q", req.Op)
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

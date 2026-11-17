package fleet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

// errReclaimNotice fails the heartbeat of a VM the cloud is taking back, so
// taskscaler places no new job on it and removes it once it is empty.
var errReclaimNotice = errors.New("the cloud is taking this vm back")

// The instance metadata service, as the VM itself reaches it.
const (
	imdsAddr = "169.254.169.254:80"
	imdsBase = "http://" + imdsAddr + "/latest"
)

// imdsEvery is how often a VM's metadata is read, as AWS's node termination
// handler does in its IMDS mode.
const imdsEvery = 2 * time.Second

// imdsQuiet is how often it is read while it has never answered: a
// metadata service turned off, or one slow to start. AWS gives two
// minutes' notice of a reclaim.
const imdsQuiet = 30 * time.Second

// A tunnel reaches addresses from inside one VM: fleetci's own HTTP
// requests to the metadata service go through the VM's SSH server, on the
// instance's shared connection, so nothing runs on the VM and its image
// needs nothing.
type tunnel struct {
	dial  func(ctx context.Context, network, addr string) (net.Conn, error)
	lost  <-chan struct{} // closed once the connection is gone
	close func()
}

type opener func(ctx context.Context, id string) (tunnel, error)

// sshTunnel forwards through the instance's shared connection.
func sshTunnel(conns *vmConns) opener {
	return func(ctx context.Context, id string) (tunnel, error) {
		client, lost, err := conns.get(id).Client(ctx)
		if err != nil {
			return tunnel{}, err
		}
		// The connection is the instance's: the watch leaves it open.
		return tunnel{dial: client.DialContext, lost: lost, close: func() {}}, nil
	}
}

var (
	errNoMetadata   = errors.New("the vm has no instance metadata")
	errNoForwarding = errors.New("the vm's ssh server does not forward tcp (AllowTcpForwarding)")
	errTunnelLost   = errors.New("the ssh connection to the vm is gone")
)

// reclaimWatch watches a class's VMs for reclaim notices.
type reclaimWatch struct {
	open      opener
	rebalance bool
	log       hclog.Logger
	base      string        // imdsBase; tests point it elsewhere
	every     time.Duration // imdsEvery
	quiet     time.Duration // imdsQuiet
	retry     time.Duration // first wait after a lost connection
	timeout   time.Duration // for one metadata request

	mu      sync.Mutex
	notices map[string]string // VM id → the notice, while watched
	// noted, if set, is told the kind of each notice, once per VM.
	noted  func(kind string)
	stops  map[string]context.CancelFunc
	closed bool
	wg     sync.WaitGroup
}

func newReclaimWatch(open opener, rebalance bool, log hclog.Logger) *reclaimWatch {
	return &reclaimWatch{open: open, rebalance: rebalance, log: log,
		base: imdsBase, every: imdsEvery, quiet: imdsQuiet, retry: time.Second, timeout: 5 * time.Second,
		notices: map[string]string{}, stops: map[string]context.CancelFunc{}}
}

// start watches id until stop or close. Watching it twice does nothing.
func (w *reclaimWatch) start(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.stops[id]; ok || w.closed {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.stops[id] = cancel
	w.wg.Go(func() { w.watch(ctx, id) })
}

// watch polls id's metadata, over a new connection after one is lost,
// until ctx ends or the VM turns out to have nothing to watch.
func (w *reclaimWatch) watch(ctx context.Context, id string) {
	log := w.log.With("instance", id)
	var seen bool // the metadata service answered once
	wait := w.retry
	for {
		started := time.Now()
		t, err := w.open(ctx, id)
		if err == nil {
			err = w.poll(ctx, id, t, &seen, log)
			t.close()
		}
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, errNoMetadata):
			log.Info("not watching the vm for reclaim", "reason", err)
			return
		case errors.Is(err, errNoForwarding):
			log.Warn("not watching the vm for reclaim; only the plugin's own check applies", "reason", err)
			return
		}
		if time.Since(started) > time.Minute {
			wait = w.retry
		}
		log.Debug("reclaim watch lost, trying again", "err", err, "in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, 30*time.Second)
	}
}

// poll reads the notices every w.every until the tunnel is lost; every
// w.quiet while the service has never answered and keeps not answering.
func (w *reclaimWatch) poll(ctx context.Context, id string, t tunnel, seen *bool, log hclog.Logger) error {
	transport := &http.Transport{DialContext: t.dial, MaxIdleConnsPerHost: 1}
	defer transport.CloseIdleConnections()
	m := &imds{client: &http.Client{Transport: transport, Timeout: w.timeout}, base: w.base}

	// A read in flight ends with the tunnel: a channel being opened as
	// the connection drops may never hear of it, and would wait out
	// w.timeout before the watch connects again.
	reads, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-t.lost:
			cancel()
		case <-reads.Done():
		}
	}()

	failures := 0
	for {
		action, rebalance, err := m.notices(reads)
		var refused *ssh.OpenChannelError
		switch {
		case err == nil:
			*seen, failures = true, 0
			w.saw(id, "interruption", action, log)
			w.saw(id, "rebalance", rebalance, log)
		case ctx.Err() != nil:
			return ctx.Err()
		case closed(t.lost):
			return fmt.Errorf("%w: %w", errTunnelLost, err)
		case errors.As(err, &refused) && refused.Reason == ssh.Prohibited:
			return fmt.Errorf("%w: %w", errNoForwarding, err)
		case errors.As(err, &refused) && !*seen:
			// The VM's ssh server could not reach the address at all.
			return fmt.Errorf("%w: %w", errNoMetadata, err)
		case busy(err), !absent(err):
			// A service under load or restarting says so, and a
			// connection that resets is trouble on the way; neither is
			// a VM without a metadata service.
			log.Debug("reading instance metadata", "err", err)
		default:
			// No answer in time: a service turned off, or one slow to
			// start, or a slow way to it. Only the first is for good, and
			// they look alike, so it is asked less often, not given up on.
			failures++
			if failures == 3 && !*seen {
				log.Info("the metadata service doesn't answer; asking less often", "every", w.quiet, "err", err)
			} else {
				log.Debug("reading instance metadata", "err", err)
			}
		}

		every := w.every
		if !*seen && failures >= 3 {
			every = w.quiet
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.lost:
			return errTunnelLost
		case <-time.After(every):
		}
	}
}

func closed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// saw takes one notice read from id's metadata; "" is none.
func (w *reclaimWatch) saw(id, kind, value string, log hclog.Logger) {
	if value == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, watched := w.stops[id]; !watched || w.notices[id] != "" {
		return
	}
	if kind == "rebalance" && !w.rebalance {
		// Noted once: a seen recommendation is kept as "", not as a notice.
		if _, noted := w.notices[id]; !noted {
			w.notices[id] = ""
			w.tell(kind)
			log.Info("rebalance recommendation; the class does not act on it", "recommendation", value)
		}
		return
	}
	w.notices[id] = kind + " " + value
	w.tell(kind)
	log.Warn("the cloud is taking this vm back; no new jobs on it, running ones go on", "notice", kind, "detail", value)
}

func (w *reclaimWatch) tell(kind string) {
	if w.noted != nil {
		w.noted(kind)
	}
}

// draining is how many watched VMs have a notice: they take no new jobs.
func (w *reclaimWatch) draining() int {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, v := range w.notices {
		if v != "" {
			n++
		}
	}
	return n
}

// notice is what the cloud said about id, or "".
func (w *reclaimWatch) notice(id string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.notices[id]
}

// stop ends the watch of id and forgets its notice.
func (w *reclaimWatch) stop(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if cancel, ok := w.stops[id]; ok {
		cancel()
		delete(w.stops, id)
	}
	delete(w.notices, id)
}

// close ends every watch and waits for them.
func (w *reclaimWatch) close() {
	w.mu.Lock()
	w.closed = true
	for id, cancel := range w.stops {
		cancel()
		delete(w.stops, id)
	}
	w.mu.Unlock()
	w.wg.Wait()
}

// imds is an IMDSv2 client: a session token first, then reads that carry
// it.
type imds struct {
	client  *http.Client
	base    string
	token   string
	expires time.Time
}

const imdsTokenTTL = 5 * time.Minute

// notices reads the spot interruption notice and the rebalance
// recommendation; "" is none.
func (m *imds) notices(ctx context.Context) (action, rebalance string, err error) {
	if action, err = m.get(ctx, "meta-data/spot/instance-action"); err != nil {
		return "", "", err
	}
	if rebalance, err = m.get(ctx, "meta-data/events/recommendations/rebalance"); err != nil {
		return "", "", err
	}
	return action, rebalance, nil
}

// get reads path; one that doesn't exist is "".
func (m *imds) get(ctx context.Context, path string) (string, error) {
	for attempt := 0; ; attempt++ {
		if m.token == "" || time.Now().After(m.expires) {
			if err := m.renew(ctx); err != nil {
				return "", err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.base+"/"+path, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("X-aws-ec2-metadata-token", m.token)
		status, body, err := m.do(req)
		switch {
		case err != nil:
			return "", err
		case status == http.StatusOK:
			return strings.TrimSpace(body), nil
		case status == http.StatusNotFound:
			return "", nil
		case status == http.StatusUnauthorized && attempt == 0:
			m.token = "" // expired early; renew once
		default:
			return "", statusError{path, status}
		}
	}
}

func (m *imds) renew(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, m.base+"/api/token", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", fmt.Sprint(int(imdsTokenTTL.Seconds())))
	status, body, err := m.do(req)
	if err != nil {
		return err
	}
	if status != http.StatusOK || body == "" {
		return statusError{"api/token", status}
	}
	m.token, m.expires = body, time.Now().Add(imdsTokenTTL-time.Minute)
	return nil
}

// statusError is an answer from the metadata service other than the one
// asked for.
type statusError struct {
	path   string
	status int
}

func (e statusError) Error() string {
	return fmt.Sprintf("instance metadata %s: status %d", e.path, e.status)
}

// busy says whether err is the metadata service saying it is throttling or
// failing for now, rather than that it is not there.
func busy(err error) bool {
	var s statusError
	return errors.As(err, &s) && (s.status == http.StatusTooManyRequests || s.status >= 500)
}

// absent says whether err is what a VM without IMDS gives: no answer in
// time, or an answer that isn't IMDSv2's.
func absent(err error) bool {
	var s statusError
	var timeout interface{ Timeout() bool }
	return errors.As(err, &s) || errors.Is(err, context.DeadlineExceeded) ||
		errors.As(err, &timeout) && timeout.Timeout()
}

func (m *imds) do(req *http.Request) (int, string, error) {
	resp, err := m.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, string(body), err
}

// vmGroup is the instance group taskscaler sees. It drops an instance's
// shared connection, and ends its reclaim watch, once the instance is
// gone, and fails the heartbeat of an instance with a reclaim notice.
type vmGroup struct {
	provider.InstanceGroup
	conns *vmConns
	w     *reclaimWatch // nil: the class isn't watched
	died  *deaths       // nil: not counted
	ups   *upTimes      // nil: not kept
}

func (g vmGroup) Heartbeat(ctx context.Context, id string) error {
	if g.w != nil {
		if n := g.w.notice(id); n != "" {
			return fmt.Errorf("%w: %s", errReclaimNotice, n)
		}
	}
	return g.InstanceGroup.Heartbeat(ctx, id)
}

func (g vmGroup) Update(ctx context.Context, fn func(string, provider.State)) error {
	return g.InstanceGroup.Update(ctx, func(id string, state provider.State) {
		if state == provider.StateDeleted {
			g.forget(id)
			if g.died != nil {
				g.died.deleted(id)
			}
		}
		fn(id, state)
	})
}

func (g vmGroup) Decrease(ctx context.Context, ids []string) ([]string, error) {
	if g.died != nil {
		g.died.removing(ids)
	}
	removed, err := g.InstanceGroup.Decrease(ctx, ids)
	for _, id := range removed {
		g.forget(id)
	}
	return removed, err
}

// forget lets go of an instance that is gone.
func (g vmGroup) forget(id string) {
	if g.w != nil {
		g.w.stop(id)
	}
	g.conns.drop(id)
	if g.ups != nil {
		g.ups.forget(id)
	}
}

// deaths counts a class's instances that the plugin reports deleted
// without fleetci having removed them: VMs that died, or cloud instances
// taken away. A Decrease is remembered for a while, as a plugin may report
// the deletion it asked for an update or two later.
type deaths struct {
	mu      sync.Mutex
	asked   map[string]time.Time
	n       int64
	log     hclog.Logger
	keepFor time.Duration
	nowFunc func() time.Time
}

func newDeaths(log hclog.Logger) *deaths {
	return &deaths{asked: map[string]time.Time{}, log: log, keepFor: 15 * time.Minute, nowFunc: time.Now}
}

func (d *deaths) removing(ids []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.nowFunc()
	for id, at := range d.asked {
		if now.Sub(at) > d.keepFor {
			delete(d.asked, id)
		}
	}
	for _, id := range ids {
		d.asked[id] = now
	}
}

func (d *deaths) deleted(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.asked[id]; ok {
		delete(d.asked, id)
		return
	}
	d.n++
	d.log.Warn("instance gone without fleetci removing it", "instance", id)
}

func (d *deaths) count() int64 {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.n
}

// Package chaos injects faults into tests: a TCP proxy that refuses, cuts,
// black-holes, slows or chops the connections through it, as a plan says.
//
// A plan is a string of bytes, each one a decision, so Go's fuzzing can
// drive it: `go test` replays the seed plans and `go test -fuzz` looks for
// new ones. Once its bytes run out, or Calm is called, a plan injects
// nothing more, so a test can check the system recovers.
package chaos

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Plan is a run's fault decisions, and what came of them: the scenario a
// failure prints.
type Plan struct {
	mu    sync.Mutex
	b     []byte
	i     int
	calm  bool
	start time.Time
	trace []event
}

// event is one step of the scenario, a fault or not, and when it came.
type event struct {
	at    time.Duration
	what  string
	fault bool
}

func NewPlan(b []byte) *Plan { return &Plan{b: append([]byte(nil), b...), start: time.Now()} }

// Pick decides among n outcomes; 0 is always "no fault", and is what a
// calm or used-up plan picks.
func (p *Plan) Pick(n int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calm || p.i >= len(p.b) || n <= 1 {
		return 0
	}
	v := int(p.b[p.i]) % n
	p.i++
	return v
}

// Calm stops all further faults.
func (p *Plan) Calm() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.calm {
		p.calm = true
		p.add(false, "calm: no faults from here on")
	}
}

// Note records a fault a test injected itself, in words, for the scenario.
func (p *Plan) Note(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.add(true, fmt.Sprintf(format, args...))
}

// Step records something the plan decided that is not a fault, such as
// when the test closes what it tests, for the scenario.
func (p *Plan) Step(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.add(false, fmt.Sprintf(format, args...))
}

func (p *Plan) add(fault bool, what string) {
	p.trace = append(p.trace, event{at: time.Since(p.start), what: what, fault: fault})
}

func (p *Plan) isCalm() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calm
}

// String is the scenario: each fault and step with when it came, or that
// there were no faults, so a failure is not the network's doing, and the
// plan's bytes, to replay it.
func (p *Plan) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var b strings.Builder
	faulted := false
	for _, e := range p.trace {
		faulted = faulted || e.fault
	}
	if faulted {
		b.WriteString("chaos scenario:")
	} else {
		b.WriteString("chaos: no faults were injected before the failure, so the network did not cause it:")
	}
	for _, e := range p.trace {
		fmt.Fprintf(&b, "\n\t+%-8v %s", e.at.Round(time.Millisecond), e.what)
	}
	fmt.Fprintf(&b, "\nreplay with CHAOS_PLAN=%s", hex.EncodeToString(p.b))
	return b.String()
}

// Report logs the scenario when t fails, with how to replay it.
func (p *Plan) Report(t testing.TB) {
	t.Cleanup(func() {
		if t.Failed() {
			name, _, _ := strings.Cut(t.Name(), "/")
			t.Logf("%s go test -run '^%s$' .", p.String(), name)
		}
	})
}

// Seeds adds a fuzz test's plans: the fixed ones, the plan in CHAOS_PLAN
// (hex, as a failure prints it) to replay one, and a few random ones, so
// every run also tries plans no one wrote down.
func Seeds(f *testing.F, fixed ...[]byte) {
	for _, b := range fixed {
		f.Add(b)
	}
	if h := os.Getenv("CHAOS_PLAN"); h != "" {
		b, err := hex.DecodeString(h)
		if err != nil {
			f.Fatalf("CHAOS_PLAN: %v", err)
		}
		f.Add(b)
	}
	for range 4 {
		b := make([]byte, 48)
		_, _ = rand.Read(b)
		f.Add(b)
	}
}

// Fault is what the proxy does to one connection.
type Fault int

const (
	Pass      Fault = iota // forward as it is
	Refuse                 // close at once
	Cut                    // forward n bytes each way, then close both sides
	BlackHole              // forward n bytes, then drop everything, open, as a VM that vanished
	Slow                   // delay every chunk a few milliseconds
	Chop                   // forward in pieces of a few bytes
	CutLater               // forward for n*4ms, then close both sides
	HoleLater              // forward for n*4ms, then drop everything, open
	faults
)

// describe says what f with param does to a connection, in words.
func (f Fault) describe(param int) string {
	later := time.Duration(param) * 4 * time.Millisecond
	switch f {
	case Pass:
		return "passed untouched"
	case Refuse:
		return "refused"
	case Cut:
		return fmt.Sprintf("cut after %d bytes each way", param*16)
	case BlackHole:
		return fmt.Sprintf("went silent after %d bytes each way, left open", param*16)
	case Slow:
		return fmt.Sprintf("slowed by %dms a chunk", param%16+1)
	case Chop:
		return fmt.Sprintf("chopped into %d-byte pieces", param%8+1)
	case CutLater:
		return fmt.Sprintf("cut after %v", later)
	case HoleLater:
		return fmt.Sprintf("went silent after %v, left open", later)
	}
	return f.String()
}

func (f Fault) String() string {
	return [...]string{"pass", "refuse", "cut", "blackhole", "slow", "chop", "cutlater", "holelater"}[f]
}

// Proxy forwards TCP connections to a target, with the faults its plan
// picks for each.
type Proxy struct {
	Addr   string
	target string
	plan   *Plan

	mu       sync.Mutex
	accepted int
	open     int
	n        int
	links    map[*link]bool
	done     bool
}

// NewProxy starts a proxy to target, closed when t ends.
func NewProxy(t testing.TB, p *Plan, target string) *Proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	x := &Proxy{Addr: ln.Addr().String(), target: target, plan: p, links: map[*link]bool{}}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		// Close what is still open, black holes too, so a test that
		// failed on a hang still ends.
		ln.Close()
		x.mu.Lock()
		x.done = true
		for l := range x.links {
			l.close()
		}
		x.mu.Unlock()
		wg.Wait()
	})
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() { x.serve(c) })
		}
	})
	return x
}

// Open is how many connections through the proxy are open.
func (x *Proxy) Open() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.open
}

// Accepted is how many connections the proxy took.
func (x *Proxy) Accepted() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.accepted
}

func (x *Proxy) serve(down net.Conn) {
	fault, param := Fault(x.plan.Pick(int(faults))), x.plan.Pick(256)
	x.mu.Lock()
	x.accepted++
	x.n++
	id := x.n
	x.mu.Unlock()
	switch {
	case fault != Pass:
		x.plan.Note("connection %d: %s", id, fault.describe(param))
	case !x.plan.isCalm():
		x.plan.Step("connection %d: %s", id, fault.describe(param))
	}
	if fault == Refuse {
		down.Close()
		return
	}
	up, err := net.Dial("tcp", x.target)
	if err != nil {
		down.Close()
		return
	}
	l := &link{fault: fault, param: param, down: down, up: up}
	x.mu.Lock()
	if x.done {
		x.mu.Unlock()
		l.close()
		return
	}
	x.open++
	x.links[l] = true
	x.mu.Unlock()

	switch fault {
	case CutLater:
		later := time.AfterFunc(time.Duration(param)*4*time.Millisecond, l.close)
		defer later.Stop()
	case HoleLater:
		later := time.AfterFunc(time.Duration(param)*4*time.Millisecond, func() {
			l.mu.Lock()
			l.holed = true
			l.mu.Unlock()
		})
		defer later.Stop()
	}
	var wg sync.WaitGroup
	wg.Go(func() { l.pipe(up, down) })
	wg.Go(func() { l.pipe(down, up) })
	wg.Wait()
	x.mu.Lock()
	x.open--
	delete(x.links, l)
	x.mu.Unlock()
}

// link is one connection through the proxy, both ways.
type link struct {
	fault    Fault
	param    int
	down, up net.Conn

	mu    sync.Mutex
	holed bool // a black hole: nothing gets through any more
	sent  [2]int
}

func (l *link) close() {
	l.down.Close()
	l.up.Close()
}

// pipe forwards src to dst with the link's fault, until either side
// closes, and then closes both.
func (l *link) pipe(dst, src net.Conn) {
	defer l.close()
	dir := 0
	if src == l.up {
		dir = 1
	}
	limit := l.param * 16
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			l.mu.Lock()
			if l.holed {
				chunk = nil
			}
			last := false
			if (l.fault == Cut || l.fault == BlackHole) && chunk != nil && l.sent[dir]+len(chunk) >= limit {
				chunk = chunk[:limit-l.sent[dir]]
				last = true
				if l.fault == BlackHole {
					l.holed = true
				}
			}
			l.sent[dir] += len(chunk)
			l.mu.Unlock()
			if werr := l.write(dst, chunk); werr != nil {
				return
			}
			if last && l.fault == Cut {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (l *link) write(dst net.Conn, b []byte) error {
	switch l.fault {
	case Slow:
		time.Sleep(time.Duration(l.param%16+1) * time.Millisecond)
	case Chop:
		size := l.param%8 + 1
		for len(b) > 0 {
			piece := b[:min(size, len(b))]
			if _, err := dst.Write(piece); err != nil {
				return err
			}
			b = b[len(piece):]
		}
		return nil
	}
	if len(b) == 0 {
		return nil
	}
	_, err := dst.Write(b)
	return err
}

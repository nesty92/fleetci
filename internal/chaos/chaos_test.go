package chaos

import (
	"bytes"
	"errors"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// talker is a server that writes greeting to each connection, then reads
// until the other side closes, counting what it got.
type talker struct {
	addr     string
	accepted atomic.Int32
	closed   atomic.Int32 // connections whose other side went
	got      atomic.Int64
}

var greeting = bytes.Repeat([]byte("0123456789"), 100) // 1000 bytes

func newTalker(t *testing.T) *talker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &talker{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.accepted.Add(1)
			go func() {
				defer c.Close()
				_, _ = c.Write(greeting)
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					s.got.Add(int64(n))
					if err != nil {
						break
					}
				}
				s.closed.Add(1)
			}()
		}
	}()
	return s
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// readFor reads c until EOF or for d, and says which.
func readFor(c net.Conn, d time.Duration) ([]byte, bool) {
	_ = c.SetReadDeadline(time.Now().Add(d))
	var got []byte
	buf := make([]byte, 4096)
	for {
		n, err := c.Read(buf)
		got = append(got, buf[:n]...)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return got, false
		}
		if err != nil {
			return got, true
		}
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAPlanPicksFromItsBytesThenNothing(t *testing.T) {
	p := NewPlan([]byte{7, 9})
	if a, b, c := p.Pick(5), p.Pick(256), p.Pick(5); a != 2 || b != 9 || c != 0 {
		t.Fatalf("picked %d %d %d, want 2 9 0", a, b, c)
	}
	p = NewPlan([]byte{3, 3, 3})
	p.Calm()
	if got := p.Pick(5); got != 0 {
		t.Fatalf("a calm plan picked %d", got)
	}
}

func TestPassForwardsBothWays(t *testing.T) {
	s := newTalker(t)
	x := NewProxy(t, NewPlan([]byte{byte(Pass)}), s.addr)
	c := dial(t, x.Addr)
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	got, _ := readFor(c, 300*time.Millisecond)
	if !bytes.Equal(got, greeting) {
		t.Fatalf("got %d bytes, want the greeting", len(got))
	}
	c.Close()
	eventually(t, "the server never saw the close", func() bool { return s.closed.Load() == 1 })
	if s.got.Load() != 5 {
		t.Fatalf("the server got %d bytes, want 5", s.got.Load())
	}
	eventually(t, "the proxy kept a closed connection", func() bool { return x.Open() == 0 })
}

func TestRefuseClosesAtOnce(t *testing.T) {
	s := newTalker(t)
	x := NewProxy(t, NewPlan([]byte{byte(Refuse)}), s.addr)
	got, eof := readFor(dial(t, x.Addr), time.Second)
	if !eof || len(got) != 0 {
		t.Fatalf("refused connection gave %d bytes, eof %v", len(got), eof)
	}
	if x.Accepted() != 1 || s.accepted.Load() != 0 {
		t.Fatalf("proxy took %d, server %d; want 1 and 0", x.Accepted(), s.accepted.Load())
	}
}

func TestCutForwardsSomeThenCloses(t *testing.T) {
	s := newTalker(t)
	x := NewProxy(t, NewPlan([]byte{byte(Cut), 2}), s.addr) // after 2*16 bytes
	got, eof := readFor(dial(t, x.Addr), time.Second)
	if !eof || !bytes.Equal(got, greeting[:32]) {
		t.Fatalf("cut connection gave %d bytes, eof %v; want 32 then eof", len(got), eof)
	}
	eventually(t, "the server's side was left open", func() bool { return s.closed.Load() == 1 })
}

func TestABlackHoleSwallowsEverythingAndStaysOpen(t *testing.T) {
	s := newTalker(t)
	x := NewProxy(t, NewPlan([]byte{byte(BlackHole), 1}), s.addr) // after 16 bytes
	c := dial(t, x.Addr)
	got, eof := readFor(c, 300*time.Millisecond)
	if eof || !bytes.Equal(got, greeting[:16]) {
		t.Fatalf("black hole gave %d bytes, eof %v; want 16 and no eof", len(got), eof)
	}
	if _, err := c.Write([]byte("lost")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if s.got.Load() != 0 || s.closed.Load() != 0 {
		t.Fatal("bytes got through the black hole")
	}
	c.Close()
	eventually(t, "closing the client left the server's side open", func() bool { return s.closed.Load() == 1 })
	eventually(t, "the proxy kept a closed connection", func() bool { return x.Open() == 0 })
}

func TestSlowAndChopDeliverEverything(t *testing.T) {
	for _, f := range []Fault{Slow, Chop} {
		s := newTalker(t)
		x := NewProxy(t, NewPlan([]byte{byte(f), 3}), s.addr)
		start := time.Now()
		got, _ := readFor(dial(t, x.Addr), time.Second)
		if !bytes.Equal(got, greeting) {
			t.Fatalf("%v: got %d bytes, want the greeting intact", f, len(got))
		}
		if f == Slow && time.Since(start) < 3*time.Millisecond {
			t.Fatalf("slow was not slow: %v", time.Since(start))
		}
	}
}

func TestACalmPlanPassesEverything(t *testing.T) {
	s := newTalker(t)
	p := NewPlan(bytes.Repeat([]byte{byte(Refuse)}, 10))
	x := NewProxy(t, p, s.addr)
	p.Calm()
	for range 3 {
		got, _ := readFor(dial(t, x.Addr), 300*time.Millisecond)
		if !bytes.Equal(got, greeting) {
			t.Fatal("a calm plan still faulted")
		}
	}
}

func TestCutLaterClosesAfterAWhile(t *testing.T) {
	s := newTalker(t)
	x := NewProxy(t, NewPlan([]byte{byte(CutLater), 25}), s.addr) // after 100ms
	c := dial(t, x.Addr)
	start := time.Now()
	got, eof := readFor(c, 2*time.Second)
	if !eof || !bytes.Equal(got, greeting) {
		t.Fatalf("got %d bytes, eof %v; want the greeting, then eof", len(got), eof)
	}
	if d := time.Since(start); d < 80*time.Millisecond {
		t.Fatalf("cut after %v, want about 100ms", d)
	}
	eventually(t, "the server's side was left open", func() bool { return s.closed.Load() == 1 })
}

func TestHoleLaterSwallowsWhatComesAfterAWhile(t *testing.T) {
	s := newTalker(t)
	x := NewProxy(t, NewPlan([]byte{byte(HoleLater), 25}), s.addr) // after 100ms
	c := dial(t, x.Addr)
	got, eof := readFor(c, 50*time.Millisecond)
	if eof || !bytes.Equal(got, greeting) {
		t.Fatalf("before the hole: %d bytes, eof %v", len(got), eof)
	}
	if _, err := c.Write([]byte("early")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bytes before the hole never arrived", func() bool { return s.got.Load() == 5 })
	time.Sleep(150 * time.Millisecond)
	if _, err := c.Write([]byte("late")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := s.got.Load(); n != 5 || s.closed.Load() != 0 {
		t.Fatalf("after the hole the server got %d bytes, closed %d", n, s.closed.Load())
	}
	if _, eof := readFor(c, 100*time.Millisecond); eof {
		t.Fatal("a black hole closed the connection")
	}
	c.Close()
	eventually(t, "closing the client left the server's side open", func() bool { return s.closed.Load() == 1 })
}

func TestAFailureSaysWhatChaosDid(t *testing.T) {
	s := newTalker(t)
	p := NewPlan([]byte{byte(Cut), 2, byte(Pass), 0})
	x := NewProxy(t, p, s.addr)
	for range 2 {
		readFor(dial(t, x.Addr), 100*time.Millisecond)
	}
	eventually(t, "the proxy never took both", func() bool { return x.Accepted() == 2 })
	p.Note("metadata service: answered 500")
	p.Calm()

	got := p.String()
	want := []string{
		"connection 1: cut after 32 bytes each way",
		"connection 2: passed untouched",
		"metadata service: answered 500",
		"calm: no faults from here on",
		"CHAOS_PLAN=02020000",
	}
	at := 0
	for _, w := range want {
		i := strings.Index(got[at:], w)
		if i < 0 {
			t.Fatalf("the scenario lacks %q, or has it out of order:\n%s", w, got)
		}
		at += i + len(w)
	}
	if !strings.Contains(got, "+") || !strings.Contains(got, "ms") {
		t.Fatalf("the scenario does not say when each fault struck:\n%s", got)
	}
}

func TestAFailureWithoutFaultsSaysTheNetworkDidNotCauseIt(t *testing.T) {
	for _, plan := range [][]byte{nil, {byte(Pass), 0, byte(Pass), 9}} {
		s := newTalker(t)
		p := NewPlan(plan)
		x := NewProxy(t, p, s.addr)
		readFor(dial(t, x.Addr), 100*time.Millisecond)
		readFor(dial(t, x.Addr), 100*time.Millisecond)
		if got := p.String(); !strings.Contains(got, "no faults were injected") {
			t.Fatalf("plan %x injected nothing, but the scenario says:\n%s", plan, got)
		}
	}
	p := NewPlan([]byte{byte(Refuse), 0})
	NewProxy(t, p, newTalker(t).addr)
	p.Note("metadata service: answered 429")
	if got := p.String(); strings.Contains(got, "no faults were injected") {
		t.Fatalf("a fault was injected, but the scenario says:\n%s", got)
	}
}

func TestEachFaultIsSaidInWords(t *testing.T) {
	seen := map[string]Fault{}
	for f := range faults {
		d := f.describe(25)
		if d == "" || strings.ContainsAny(d, "_") {
			t.Fatalf("%v is described as %q", f, d)
		}
		if g, ok := seen[d]; ok {
			t.Fatalf("%v and %v are both %q", f, g, d)
		}
		seen[d] = f
	}
	for f, want := range map[Fault]string{Cut: "400 bytes", BlackHole: "400 bytes", CutLater: "100ms", HoleLater: "100ms", Slow: "10ms", Chop: "2-byte"} {
		if d := f.describe(25); !strings.Contains(d, want) {
			t.Fatalf("%v(25) is %q, want it to say %s", f, d, want)
		}
	}
}

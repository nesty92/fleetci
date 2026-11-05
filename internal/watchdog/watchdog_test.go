package watchdog

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newWatch(alive *bool, restarts *int, c *clock) *watch {
	cfg := Config{
		Plugin:     "/usr/local/bin/fleeting-plugin-fleetci",
		Restart:    func(context.Context) error { *restarts++; return nil },
		Interval:   10 * time.Second,
		Grace:      30 * time.Second,
		MinBackoff: time.Minute,
		MaxBackoff: 10 * time.Minute,
		alive:      func(string) bool { return *alive },
		now:        c.now,
	}
	return &watch{cfg: cfg, log: hclog.NewNullLogger(), backoff: cfg.MinBackoff}
}

// run checks every 10 s for d.
func run(w *watch, c *clock, d time.Duration) {
	for end := c.t.Add(d); !c.t.After(end); c.add(10 * time.Second) {
		w.check(context.Background())
	}
}

func TestRestartsAfterGrace(t *testing.T) {
	alive, restarts, c := true, 0, &clock{time.Unix(0, 0)}
	w := newWatch(&alive, &restarts, c)
	run(w, c, time.Minute)
	if restarts != 0 {
		t.Fatalf("restarted a running plugin %d times", restarts)
	}
	alive = false
	run(w, c, 20*time.Second)
	if restarts != 0 {
		t.Fatal("restarted within the grace")
	}
	run(w, c, 10*time.Second)
	if restarts != 1 {
		t.Fatalf("restarts %d after 30 s gone, want 1", restarts)
	}
}

func TestBacksOffWhileThePluginStaysGone(t *testing.T) {
	alive, restarts, c := false, 0, &clock{time.Unix(0, 0)}
	w := newWatch(&alive, &restarts, c)
	run(w, c, 30*time.Minute)
	// at 30 s, then 1, 2, 4, 8 min later (+30 s grace each): 5, not 60
	if restarts < 4 || restarts > 6 {
		t.Fatalf("%d restarts in 30 min, want about 5", restarts)
	}
}

func TestBackoffResetsAfterAHealthyRun(t *testing.T) {
	alive, restarts, c := false, 0, &clock{time.Unix(0, 0)}
	w := newWatch(&alive, &restarts, c)
	run(w, c, 5*time.Minute)
	alive = true
	run(w, c, 11*time.Minute)
	if w.backoff != time.Minute {
		t.Fatalf("backoff %v after a healthy run, want 1m", w.backoff)
	}
	before := restarts
	alive = false
	run(w, c, 30*time.Second)
	if restarts != before+1 {
		t.Fatal("no prompt restart after the reset")
	}
}

func TestPluginAlive(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	if PluginAlive(sleep) {
		t.Skip("a sleep already runs here")
	}
	cmd := exec.Command(sleep, "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if !PluginAlive(sleep) {
		t.Fatal("running process not seen")
	}
	if !PluginAlive("sleep") {
		t.Fatal("running process not seen by its bare name")
	}
	_ = cmd.Process.Signal(os.Kill)
	time.Sleep(100 * time.Millisecond) // a zombie until Wait
	if PluginAlive(sleep) {
		t.Fatal("a zombie counted as alive")
	}
	_ = cmd.Wait()
}

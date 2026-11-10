package fleet

import (
	"context"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

// TestDeaths: an instance reported deleted is counted as died unless
// fleetci asked for its removal, and an old ask is forgotten.
func TestDeaths(t *testing.T) {
	d := newDeaths(hclog.NewNullLogger())
	now := time.Now()
	d.nowFunc = func() time.Time { return now }
	r := &reporting{id: "i-1", state: provider.StateDeleted}
	g := vmGroup{r, newVMConns(newCloud(), false, hclog.NewNullLogger()), nil, d}
	update := func() { _ = g.Update(context.Background(), func(string, provider.State) {}) }

	update()
	if d.count() != 1 {
		t.Fatalf("died on its own: %d", d.count())
	}
	_, _ = g.Decrease(context.Background(), []string{"i-1"})
	update()
	if d.count() != 1 {
		t.Fatalf("removed by fleetci: %d", d.count())
	}
	update() // its ask is used up: reported again, it counts
	if d.count() != 2 {
		t.Fatalf("again: %d", d.count())
	}

	d.removing([]string{"i-2"})
	now = now.Add(time.Hour)
	d.removing(nil)
	if len(d.asked) != 0 {
		t.Fatalf("old asks kept: %v", d.asked)
	}
	if (*deaths)(nil).count() != 0 {
		t.Fatal("nil")
	}
}

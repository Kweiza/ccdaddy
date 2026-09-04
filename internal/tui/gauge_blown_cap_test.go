package tui

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/theme"
	"github.com/Kweiza/ccdaddy/internal/usage"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// blownCapRow is built from a COMPUTED Headroom rather than a hand-written one,
// so the case cannot assert the shape it was handed. It is a live fleet's row:
// a five-hour window with room, the all-model weekly at 80, and the Fable cap
// gone, measured against hover-shaped thresholds — a pace target is unclamped,
// so a window far through its own cycle is held to a figure above 100.
func blownCapRow(t *testing.T, fablePct float64) view.Row {
	t.Helper()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	win := func(v float64, d time.Duration) usage.Window {
		at := now.Add(d)
		return usage.NewWindow(&v, &at)
	}
	at := now.Add(40 * time.Hour)
	s := &usage.Snapshot{
		FiveHour: win(0, 4*time.Hour),
		SevenDay: win(80, 40*time.Hour),
		Limits: []usage.Limit{usage.LimitFor(usage.LimitInput{
			Kind: "weekly_scoped", Model: "Fable", Percent: &fablePct, ResetsAt: &at,
		})},
	}
	thr := strategy.Thresholds{Default: 80, PerWindow: map[usage.WindowName]float64{
		usage.WindowFiveHour:                              28,
		usage.WindowSevenDay:                              117,
		usage.ScopedWindowName(usage.ScopeModel, "Fable"): 117,
	}}
	return view.Row{HasEntry: true, Entry: usage.Entry{Snapshot: s}, Headroom: strategy.HeadroomOf(s, thr)}
}

// The regression 0.10.0 shipped. Widening the ACCOUNT verdict made Empty() false
// for this row — rightly, it can still serve every model but Fable — and the
// band below then read the floor's slack, which a pace target above 100 makes
// POSITIVE. A full bar drawn off a week that is gone, painted green.
func TestAFullBarDrawnOffABlownCapIsNeverPaintedOK(t *testing.T) {
	r := blownCapRow(t, 100)

	// The control: the ACCOUNT verdict lets this row through, because it can
	// still serve every model but Fable. Whatever the band then says, a bar
	// drawn at 100% must not read as healthy.
	if empty, known := r.Empty(); !known || empty {
		t.Fatalf("Empty() = (%v, %v), want (false, true) — the account can still serve other models", empty, known)
	}
	if got := r.UsedLabel(); got != "100%" {
		t.Fatalf("UsedLabel() = %q, want 100%% — the bar is full", got)
	}

	if got := gaugeRole(r); got != theme.RoleGaugeOver {
		t.Errorf("gaugeRole = %v, want RoleGaugeOver — a full bar off a week that is gone must never read as healthy", got)
	}
}

// The clause on its own, aimed where the clamp cannot reach it.
//
// HeadroomFor now clamps an empty window's slack to nothing positive, so a
// Headroom it built can no longer arrive here with a full bar and a healthy
// band -- the band alone would paint this row over. That closed the door
// upstream and left this clause as the second lock, which is worth keeping:
// a Headroom built by hand, by a fixture or by any future path that does not
// go through that loop, is not clamped by anything.
//
// So the fixture is a hand-built Headroom: reported window at 100%, slack well
// past the band. Only the emptiness clause can catch it.
func TestTheGaugeAsksTheWindowItDrewEvenWhenTheSlackLooksHealthy(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	at := now.Add(40 * time.Hour)
	full := 100.0
	s := &usage.Snapshot{
		SevenDay: usage.NewWindow(&full, &at),
		FiveHour: usage.NewWindow(func() *float64 { v := 10.0; return &v }(), &at),
	}
	r := view.Row{HasEntry: true, Entry: usage.Entry{Snapshot: s}, Headroom: strategy.Headroom{
		Known: true, Binding: usage.WindowFiveHour, Pct: 90, Slack: 40, Threshold: 50,
		MinPct: 0, MinWindow: usage.WindowSevenDay,
		MinAnyModelPct: 0, MinAnyModelWindow: usage.WindowSevenDay,
		HasFloor: true, Floor: usage.WindowSevenDay, FloorSlack: 40, FloorThreshold: 140,
	}}

	if slack, _, ok := r.ReportedSlack(); !ok || slack <= warnBand {
		t.Fatalf("ReportedSlack() = %v — this fixture must reach the band with room to spare", slack)
	}
	if got := gaugeRole(r); got != theme.RoleGaugeOver {
		t.Errorf("gaugeRole = %v, want RoleGaugeOver — the bar was drawn at 100%%", got)
	}
}

// The complement, so the fix cannot be made by painting every row over: a cap
// that is merely nearly gone still gets the band it earned.
func TestACapThatIsNotYetGoneStillTakesTheBand(t *testing.T) {
	r := blownCapRow(t, 99)

	if r.ReportedEmpty() {
		t.Fatal("ReportedEmpty() = true at 99% — only a window with nothing left is empty")
	}
	if got := gaugeRole(r); got == theme.RoleGaugeOver {
		t.Errorf("gaugeRole = RoleGaugeOver at 99%% — the band, not the emptiness clause, owns this row")
	}
}

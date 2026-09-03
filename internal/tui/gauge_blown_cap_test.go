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

	// The control: this row is the one the fix is about only because the
	// account verdict lets it through and the band would say OK.
	if empty, known := r.Empty(); !known || empty {
		t.Fatalf("Empty() = (%v, %v), want (false, true) — the account can still serve other models", empty, known)
	}
	if slack, _, ok := r.ReportedSlack(); !ok || slack <= warnBand {
		t.Fatalf("ReportedSlack() = %v — the fixture must reach the band with room to spare", slack)
	}
	if got := r.UsedLabel(); got != "100%" {
		t.Fatalf("UsedLabel() = %q, want 100%% — the bar is full", got)
	}

	if got := gaugeRole(r); got != theme.RoleGaugeOver {
		t.Errorf("gaugeRole = %v, want RoleGaugeOver — a full bar off a week that is gone must never read as healthy", got)
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

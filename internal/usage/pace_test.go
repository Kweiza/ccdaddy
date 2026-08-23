package usage

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// weekly builds a seven-day window that resets at `reset` and is `pct` consumed.
func weekly(pct float64, reset time.Time) Window {
	return Window{Present: true, pct: pct, hasPct: true, reset: reset, hasTime: true}
}

var week = 7 * 24 * time.Hour

func TestPaceComparesUsageAgainstElapsedTime(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	// Half the week gone.
	reset := now.Add(week / 2)

	p := PaceOf(WindowSevenDay, weekly(70, reset), now)
	if !p.OK() {
		t.Fatalf("OK() = false, reason %v", p.Reason)
	}
	if p.ExpectedPct != 50 {
		t.Errorf("ExpectedPct = %v, want 50 — half a week elapsed is half the quota", p.ExpectedPct)
	}
	if p.ActualPct != 70 {
		t.Errorf("ActualPct = %v, want 70", p.ActualPct)
	}
	if !p.AheadOfPace {
		t.Error("AheadOfPace = false; 70% used against 50% elapsed is ahead")
	}
}

func TestPaceReportsBehindPace(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	p := PaceOf(WindowSevenDay, weekly(20, now.Add(week/2)), now)

	if !p.OK() {
		t.Fatalf("OK() = false, reason %v", p.Reason)
	}
	if p.AheadOfPace {
		t.Error("AheadOfPace = true; 20% used against 50% elapsed is behind")
	}
}

// Exactly on pace is not ahead of it. The boundary decides whether a dashboard
// flags an account that is spending precisely as fast as its week allows.
func TestPaceDoesNotCallExactlyOnPaceAhead(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	p := PaceOf(WindowSevenDay, weekly(50, now.Add(week/2)), now)

	if !p.OK() {
		t.Fatalf("OK() = false, reason %v", p.Reason)
	}
	if p.ActualPct != p.ExpectedPct {
		t.Fatalf("the fixture is not exactly on pace: %v vs %v", p.ActualPct, p.ExpectedPct)
	}
	if p.AheadOfPace {
		t.Error("AheadOfPace = true; spending exactly as fast as the week allows is on pace, not ahead")
	}
}

// A utilization past 100 is an already-exhausted window. It projects exhaustion
// at now, not at a timestamp in the past, which would render as "ran out three
// days ago" on a window that is still open.
func TestProjectionReportsAnOverspentWindowAsExhaustedNow(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	p := PaceOf(WindowSevenDay, weekly(140, now.Add(week/2)), now)

	proj, ok := p.Projection()
	if !ok {
		t.Fatal("Projection() reported nothing")
	}
	if !proj.ExhaustionAt.Equal(now) {
		t.Errorf("ExhaustionAt = %s, want %s — an overspent window is exhausted now, not in the past", proj.ExhaustionAt, now)
	}
	if proj.WillLastToReset {
		t.Error("WillLastToReset = true for a window already past 100%")
	}
}

// The 24-hour suppression is load-bearing: just after a reset, elapsed time is
// tiny, so almost any usage reads as "far ahead" and the dashboard cries wolf
// every Monday.
func TestPaceIsSuppressedForTheFirstDayAfterAReset(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		elapsed time.Duration
		wantOK  bool
	}{
		{"one minute in", time.Minute, false},
		{"just under a day", 24*time.Hour - time.Second, false},
		{"exactly a day", 24 * time.Hour, true},
		{"just over a day", 24*time.Hour + time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reset := now.Add(week - tc.elapsed)
			p := PaceOf(WindowSevenDay, weekly(90, reset), now)
			if got := p.OK(); got != tc.wantOK {
				t.Fatalf("OK() = %v (reason %v), want %v", got, p.Reason, tc.wantOK)
			}
			if !tc.wantOK && p.Reason != PaceTooEarly {
				t.Errorf("Reason = %v, want PaceTooEarly", p.Reason)
			}
		})
	}
}

// A null or unparseable resets_at leaves elapsed unknowable. Pace must go quiet,
// never divide by zero and never take the 1970 epoch as a window start — which
// would report infinite overage on every account.
func TestPaceGoesQuietWithoutAReset(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	w := Window{Present: true, pct: 90, hasPct: true}

	p := PaceOf(WindowSevenDay, w, now)
	if p.OK() {
		t.Fatal("OK() = true for a window with no reset")
	}
	if p.Reason != PaceNoReset {
		t.Errorf("Reason = %v, want PaceNoReset", p.Reason)
	}
	if p.ExpectedPct != 0 || p.AheadOfPace {
		t.Errorf("a suppressed pace must report nothing: %+v", p)
	}
}

func TestPaceGoesQuietWithoutAUtilization(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	w := Window{Present: true, reset: now.Add(week / 2), hasTime: true}

	p := PaceOf(WindowSevenDay, w, now)
	if p.OK() {
		t.Fatal("OK() = true for a window with no utilization")
	}
	if p.Reason != PaceNoUtilization {
		t.Errorf("Reason = %v, want PaceNoUtilization", p.Reason)
	}
}

// §7.5 scopes pace to the weekly windows. The five-hour window is far too short
// for a 24-hour suppression to leave anything, and cinder_cove is not a window
// at all.
func TestPaceOnlyAppliesToWeeklyWindows(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	for _, name := range []WindowName{WindowFiveHour, WindowCinderCove} {
		p := PaceOf(name, weekly(50, now.Add(time.Hour)), now)
		if p.OK() {
			t.Errorf("%s: OK() = true; pace is a weekly-window measure", name)
		}
		if p.Reason != PaceNotWeekly {
			t.Errorf("%s: Reason = %v, want PaceNotWeekly", name, p.Reason)
		}
	}
	for _, name := range []WindowName{WindowSevenDay, WindowSevenDayOAuthApps, WindowSevenDayOpus, WindowSevenDaySonnet} {
		p := PaceOf(name, weekly(50, now.Add(week/2)), now)
		if !p.OK() {
			t.Errorf("%s: OK() = false (reason %v); every seven-day window paces", name, p.Reason)
		}
	}
}

// A reset that is further out than the window is long means the clock moved, or
// the endpoint did. Either way there is no elapsed time to divide by.
func TestPaceGoesQuietWhenTheWindowHasNotStarted(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	p := PaceOf(WindowSevenDay, weekly(50, now.Add(week+time.Hour)), now)

	if p.OK() {
		t.Fatal("OK() = true for a window that has not started yet")
	}
	if p.Reason != PaceWindowNotStarted {
		t.Errorf("Reason = %v, want PaceWindowNotStarted", p.Reason)
	}
}

// A reset already in the past is a window the endpoint has not rolled over yet.
// Elapsed is capped at the window length rather than running past 100% expected.
func TestPaceCapsExpectedAtAFullWindow(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	p := PaceOf(WindowSevenDay, weekly(50, now.Add(-time.Hour)), now)

	if !p.OK() {
		t.Fatalf("OK() = false, reason %v", p.Reason)
	}
	if p.ExpectedPct != 100 {
		t.Errorf("ExpectedPct = %v, want 100 — a fully elapsed window expects the whole quota", p.ExpectedPct)
	}
}

func TestProjectionExtrapolatesTheCurrentBurn(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	// Half a week gone, half the quota spent: the burn lands exactly on reset.
	p := PaceOf(WindowSevenDay, weekly(50, now.Add(week/2)), now)

	proj, ok := p.Projection()
	if !ok {
		t.Fatal("Projection() reported nothing for a window with a measurable burn")
	}
	want := now.Add(week / 2)
	if !proj.ExhaustionAt.Equal(want) {
		t.Errorf("ExhaustionAt = %s, want %s", proj.ExhaustionAt, want)
	}
	if !proj.WillLastToReset {
		t.Error("WillLastToReset = false; exhaustion exactly at the reset still lasts")
	}
}

func TestProjectionReportsAWindowThatRunsOutEarly(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	p := PaceOf(WindowSevenDay, weekly(80, now.Add(week/2)), now)

	proj, ok := p.Projection()
	if !ok {
		t.Fatal("Projection() reported nothing")
	}
	if !proj.ExhaustionAt.Before(now.Add(week / 2)) {
		t.Errorf("ExhaustionAt = %s; 80%% spent in half a week runs out before the reset", proj.ExhaustionAt)
	}
	if proj.WillLastToReset {
		t.Error("WillLastToReset = true for a window projected to exhaust early")
	}
}

func TestProjectionReportsNothingWithoutABurn(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	p := PaceOf(WindowSevenDay, weekly(0, now.Add(week/2)), now)

	if proj, ok := p.Projection(); ok {
		t.Errorf("Projection() = %+v; nothing spent means no rate to extrapolate", proj)
	}
}

func TestProjectionReportsNothingWhenPaceIsSuppressed(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	p := PaceOf(WindowSevenDay, weekly(90, now.Add(week-time.Hour)), now)

	if proj, ok := p.Projection(); ok {
		t.Errorf("Projection() = %+v; the first day's numbers are exactly the ones too noisy to extrapolate", proj)
	}
}

// §7.5: the projection stays --json-only. A linear extrapolation against bursty
// real usage is too rough to present as fact, and the next reviewer's instinct is
// to surface it "helpfully". Keeping it off Pace's own field set is what makes
// that a deliberate act rather than an accident, and this test is the guard.
func TestPaceCarriesNoProjectionFields(t *testing.T) {
	tp := reflect.TypeOf(Pace{})
	for i := 0; i < tp.NumField(); i++ {
		f := tp.Field(i)
		// Only exported fields matter: an unexported one is unreachable from a
		// renderer in another package, which is exactly the containment this
		// test is here to keep.
		if !f.IsExported() {
			continue
		}
		name := f.Name
		for _, banned := range []string{"Project", "Exhaust", "WillLast"} {
			if strings.Contains(name, banned) {
				t.Errorf("Pace.%s: the projection is --json-only (spec §7.5) and must stay behind Pace.Projection(), "+
					"so a human renderer ranging over Pace's fields cannot pick it up", name)
			}
		}
	}
}

func TestSnapshotPacesEveryWeeklyWindow(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	s := &Snapshot{
		FiveHour:       weekly(10, now.Add(time.Hour)),
		SevenDay:       weekly(70, now.Add(week/2)),
		SevenDayOpus:   weekly(20, now.Add(week/2)),
		SevenDaySonnet: Window{},
	}

	got := s.Pace(now)
	if p, ok := got[WindowSevenDay]; !ok || !p.AheadOfPace {
		t.Errorf("Pace()[seven_day] = %+v, %v; want an ahead-of-pace reading", p, ok)
	}
	if p, ok := got[WindowSevenDayOpus]; !ok || p.AheadOfPace {
		t.Errorf("Pace()[seven_day_opus] = %+v, %v; want a behind-pace reading", p, ok)
	}
	if _, ok := got[WindowFiveHour]; ok {
		t.Error("Pace() included five_hour; pace is a weekly measure")
	}
	if _, ok := got[WindowSevenDaySonnet]; ok {
		t.Error("Pace() included a window the response never carried")
	}
}

// An account whose binding cap is a per-model weekly one is exactly the account
// a pace reading is most useful for, so limits[] has to reach §7.5 too.
func TestPaceCoversTheScopedWindows(t *testing.T) {
	now := mustTime(t, "2026-08-24T00:00:00Z")
	reset := mustTime(t, "2026-08-27T00:00:00Z")
	pct := 90.0

	snap := &Snapshot{
		SevenDay: NewWindow(&pct, &reset),
		Limits: []Limit{
			LimitFor(LimitInput{Kind: "weekly_scoped", Group: "model", Model: "Fable",
				Percent: &pct, ResetsAt: &reset}),
			// A five-hour-shaped cap cannot arrive here: usage admits only
			// weekly_scoped entries, so every scoped window is paceable.
			LimitFor(LimitInput{Kind: "weekly_scoped", Group: "surface", Surface: "Cowork",
				Percent: &pct, ResetsAt: &reset}),
		},
	}

	got := snap.Pace(now)
	for _, name := range []WindowName{
		WindowSevenDay,
		"weekly_scoped:model:Fable",
		"weekly_scoped:surface:Cowork",
	} {
		p, ok := got[name]
		if !ok {
			t.Fatalf("Pace() has no reading for %q: %v", name, got)
		}
		if !p.OK() || !p.AheadOfPace {
			t.Errorf("Pace()[%q] = %+v; 90%% spent four days into a week is ahead of pace", name, p)
		}
	}

	if !IsWeekly("weekly_scoped:model:Fable") {
		t.Error("a scoped window does not report as weekly")
	}
	if IsWeekly(WindowFiveHour) || IsWeekly(WindowCinderCove) {
		t.Error("five_hour or cinder_cove reports as weekly")
	}
}

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

var (
	week     = 7 * 24 * time.Hour
	fiveHour = 5 * time.Hour
)

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

// Pace covers every window ccdad has a length for. cinder_cove is the one
// exception and always will be: its resets_at is an expiry rather than a
// rollover, so there is no window start to measure elapsed time from.
func TestPaceCoversEveryWindowWithAKnownLength(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	p := PaceOf(WindowCinderCove, weekly(50, now.Add(time.Hour)), now)
	if p.OK() {
		t.Error("cinder_cove: OK() = true; it has no window length to measure against")
	}
	if p.Reason != PaceNoWindowLength {
		t.Errorf("cinder_cove: Reason = %v, want PaceNoWindowLength", p.Reason)
	}

	if p := PaceOf(WindowFiveHour, weekly(50, now.Add(time.Hour)), now); !p.OK() {
		t.Errorf("five_hour: OK() = false (reason %v); a five-hour window paces", p.Reason)
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

// The five-hour window paces too. Its length is 18000 s in the same table the
// seven-day windows come from, so the only thing that ever stopped it was a
// suppression written as a constant 24 hours instead of as a share of the
// window.
func TestPaceCoversTheFiveHourWindow(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	// Four hours into a five-hour window: 80% of it has run.
	p := PaceOf(WindowFiveHour, weekly(90, now.Add(time.Hour)), now)

	if !p.OK() {
		t.Fatalf("OK() = false, reason %v", p.Reason)
	}
	if p.ExpectedPct != 80 {
		t.Errorf("ExpectedPct = %v, want 80 — four hours of five is four fifths of the quota", p.ExpectedPct)
	}
	if p.ActualPct != 90 {
		t.Errorf("ActualPct = %v, want 90", p.ActualPct)
	}
	if !p.AheadOfPace {
		t.Error("AheadOfPace = false; 90% spent against 80% elapsed is ahead")
	}
	if _, ok := p.Projection(); !ok {
		t.Error("Projection() reported nothing for a five-hour window with a measurable burn")
	}
}

// The suppression is a SHARE of the window, one seventh, and not a fixed 24
// hours. A seventh reproduces the day a seven-day window was held quiet for
// exactly (604800/7 = 86400) and gives the five-hour window about 43 minutes,
// which is what lets a five-hour window pace at all.
func TestPaceIsSuppressedForTheFirstSeventhOfAWindow(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		window  WindowName
		length  time.Duration
		elapsed time.Duration
		wantOK  bool
	}{
		{"weekly, one minute in", WindowSevenDay, week, time.Minute, false},
		{"weekly, just under a day", WindowSevenDay, week, 24*time.Hour - time.Second, false},
		{"weekly, exactly a day", WindowSevenDay, week, 24 * time.Hour, true},
		{"weekly, just over a day", WindowSevenDay, week, 24*time.Hour + time.Second, true},
		{"five-hour, one minute in", WindowFiveHour, fiveHour, time.Minute, false},
		{"five-hour, just under a seventh", WindowFiveHour, fiveHour, fiveHour/7 - time.Nanosecond, false},
		{"five-hour, exactly a seventh", WindowFiveHour, fiveHour, fiveHour / 7, true},
		{"five-hour, an hour in", WindowFiveHour, fiveHour, time.Hour, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reset := now.Add(tc.length - tc.elapsed)
			p := PaceOf(tc.window, weekly(90, reset), now)
			if got := p.OK(); got != tc.wantOK {
				t.Fatalf("OK() = %v (reason %v), want %v", got, p.Reason, tc.wantOK)
			}
			if !tc.wantOK && p.Reason != PaceTooEarly {
				t.Errorf("Reason = %v, want PaceTooEarly", p.Reason)
			}
		})
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

// The projection stays out of every human-facing view. A linear extrapolation
// against bursty real usage is too rough to present as fact, and the next
// reviewer's instinct is to surface it "helpfully". Keeping it off Pace's own
// field set is what makes that a deliberate act rather than an accident, and
// this test is the guard.
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
				t.Errorf("Pace.%s: the projection is kept out of every human-facing view "+
					"and must stay behind Pace.Projection(), so a renderer ranging over "+
					"Pace's fields cannot pick it up", name)
			}
		}
	}
}

func TestSnapshotPacesEveryWindowItCanMeasure(t *testing.T) {
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
	if p, ok := got[WindowFiveHour]; !ok || p.AheadOfPace {
		t.Errorf("Pace()[five_hour] = %+v, %v; four hours into a five-hour window, 10%% spent is behind pace", p, ok)
	}
	if _, ok := got[WindowSevenDaySonnet]; ok {
		t.Error("Pace() included a window the response never carried")
	}
}

// An account whose binding cap is a per-model weekly one is exactly the account
// a pace reading is most useful for, so limits[] has to be paced too.
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
	// IsWeekly is a question about PERISHABILITY and is not the length table:
	// five_hour has a length and paces, and is still not weekly quota. Merging
	// the two would make every account's soonest "weekly" reset its five-hour
	// rollover, and consume-first would spend against a cap that comes back in
	// under five hours instead of the week's quota that is about to be lost.
	if IsWeekly(WindowFiveHour) || IsWeekly(WindowCinderCove) {
		t.Error("five_hour or cinder_cove reports as weekly")
	}
}

// A projection far enough out overflows the conversion to time.Duration, and an
// overflow here does not lose precision -- it wraps the instant into the PAST
// and flips WillLastToReset to the opposite of the truth. That answer reaches a
// switch decision through strategy's projectedExhaustion, so it has to saturate
// rather than wrap.
func TestAVeryDistantProjectionSaturatesRatherThanWrapping(t *testing.T) {
	now := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	length := 7 * 24 * time.Hour
	// 65% of the way through the week, with almost nothing spent: the burn rate
	// extrapolates centuries out.
	reset := now.Add(length - time.Duration(0.65*float64(length)))
	pct := 0.003
	p := PaceOf(WindowSevenDay, NewWindow(&pct, &reset), now)

	proj, ok := p.Projection()
	if !ok {
		t.Fatal("no projection; this test needs one to overflow")
	}
	if !proj.ExhaustionAt.After(now) {
		t.Errorf("ExhaustionAt = %v, which is before now (%v): the conversion wrapped",
			proj.ExhaustionAt, now)
	}
	if !proj.WillLastToReset {
		t.Errorf("WillLastToReset = false at %v%% used; the window outlasts its reset by centuries", pct)
	}
}

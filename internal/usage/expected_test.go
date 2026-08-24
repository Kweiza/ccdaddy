package usage

import (
	"math"
	"testing"
	"time"
)

// resetAt builds a window that reported a reset instant and nothing else. The
// share elapsed is a question about the clock, so utilization is deliberately
// absent from every fixture here.
func resetAt(at time.Time) Window { return NewWindow(nil, &at) }

// The elapsed share is a separate question from the pace VERDICT, and it has to
// be answerable in the window's first hours where the verdict is suppressed. A
// threshold derived from it is not a claim about the account, it is an
// instruction to hand the next session to a peer, and doing that early costs
// nothing while a peer has room.
func TestExpectedPctAnswersInsideTheSuppressionWindow(t *testing.T) {
	at := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	// Two hours into a seven-day window: far inside the day PaceOf holds quiet.
	w := weekly(30, at.Add(week-2*time.Hour))

	if p := PaceOf(WindowSevenDay, w, at); p.Reason != PaceTooEarly {
		t.Fatalf("PaceOf said %v; this fixture is here to be suppressed", p.Reason)
	}
	got, ok := ExpectedPct(WindowSevenDay, w, at)
	if !ok {
		t.Fatal("ExpectedPct() reported nothing two hours into a seven-day window")
	}
	want := 2.0 / (7 * 24) * 100
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("ExpectedPct() = %v, want %v", got, want)
	}
}

// The three refusals, and each is a different fact about the window rather than
// a small number. A zero here would read as "the window has just reset", which
// is the most generous answer there is.
func TestExpectedPctRefusesRatherThanGuessing(t *testing.T) {
	at := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	if _, ok := ExpectedPct(WindowCinderCove, resetAt(at.Add(time.Hour)), at); ok {
		t.Error("cinder_cove has no window length, so there is no share of it to be through")
	}
	// The same window with its expiry BEHIND us, which is the case the negative
	// guard below cannot stand in for: a length of zero divides a capped zero
	// elapsed by itself and hands back NaN with ok true.
	if got, ok := ExpectedPct(WindowCinderCove, resetAt(at.Add(-time.Hour)), at); ok {
		t.Errorf("ExpectedPct() = %v for an expired cinder_cove grant; a missing length is a refusal, not a division", got)
	}
	if _, ok := ExpectedPct(WindowSevenDay, NewWindow(nil, nil), at); ok {
		t.Error("a window that reported no reset has no start to measure from")
	}
	if _, ok := ExpectedPct(WindowFiveHour, resetAt(at.Add(6*time.Hour)), at); ok {
		t.Error("a reset further out than the window is long is a clock problem, not a share")
	}
}

// A reset already in the past is a window the endpoint has not rolled over yet.
// Running past a full window would report more quota elapsed than exists.
func TestExpectedPctCapsAtAFullWindow(t *testing.T) {
	at := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	got, ok := ExpectedPct(WindowFiveHour, resetAt(at.Add(-time.Hour)), at)
	if !ok || got != 100 {
		t.Errorf("ExpectedPct() = %v, %v; want 100 for a window whose reset is behind us", got, ok)
	}
}

// Two spellings of one share, pinned together. They are separate functions
// because one is suppressed and one is not, and the day the cap or the window
// table moves under only one of them, this is what fails.
func TestExpectedPctAgreesWithTheShareThatPaceReports(t *testing.T) {
	at := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name   string
		window WindowName
		w      Window
	}{
		{"a seven-day window half gone", WindowSevenDay, weekly(70, at.Add(week/2))},
		{"a five-hour window an hour from its reset", WindowFiveHour, weekly(70, at.Add(time.Hour))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := PaceOf(tc.window, tc.w, at)
			if !p.OK() {
				t.Fatalf("PaceOf() said %v; this fixture has to be past suppression", p.Reason)
			}
			got, ok := ExpectedPct(tc.window, tc.w, at)
			if !ok || got != p.ExpectedPct {
				t.Errorf("ExpectedPct() = %v, %v; PaceOf().ExpectedPct = %v", got, ok, p.ExpectedPct)
			}
		})
	}
}

package usage

import (
	"math"
	"testing"
	"time"
)

var burnNow = time.Date(2026, 9, 5, 15, 0, 0, 0, time.UTC)

func sample(pct float64, at time.Time, reset time.Time) BindingSample {
	return BindingSample{Window: WindowFiveHour, Pct: pct, At: at, Reset: reset}
}

func closeTo(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.0001 {
		t.Errorf("rate = %v, want %v", got, want)
	}
}

// The rate is the delta between two readings over the time between them, in
// points of the window per minute. Measured on this fleet: 0% to 100% of a
// five-hour window in 18m28s, which is 5.4 points a minute.
func TestTheBurnRateIsTheDeltaBetweenTwoReadings(t *testing.T) {
	reset := burnNow.Add(3 * time.Hour)
	prev := sample(0, burnNow, reset)
	cur := sample(100, burnNow.Add(18*time.Minute+28*time.Second), reset)

	got, ok := BurnPerMin(prev, cur)
	if !ok {
		t.Fatal("no rate from two readings of the same window")
	}
	closeTo(t, got, 100/18.4667)
}

// A window that rolled over between the two readings has no rate to report. The
// delta across a rollover is a reset, not a burn, and reading it as one would
// hand the engine a large negative rate or -- worse, after the new window is
// spent against -- a small positive one that understates by the whole window.
func TestARolloverBetweenTheReadingsYieldsNoRate(t *testing.T) {
	prev := sample(96, burnNow, burnNow.Add(2*time.Minute))
	cur := sample(4, burnNow.Add(10*time.Minute), burnNow.Add(5*time.Hour))
	if _, ok := BurnPerMin(prev, cur); ok {
		t.Error("reported a rate across a rollover")
	}
}

// The same, from the other side: the reset is unchanged but the number fell.
// Something reset that this build did not see, and inventing a negative burn
// from it would tell the projection the account is filling up.
func TestAFallingReadingYieldsNoRate(t *testing.T) {
	reset := burnNow.Add(3 * time.Hour)
	prev := sample(80, burnNow, reset)
	cur := sample(12, burnNow.Add(5*time.Minute), reset)
	if _, ok := BurnPerMin(prev, cur); ok {
		t.Error("reported a rate from a falling reading")
	}
}

// An idle account burns nothing, and zero is a MEASUREMENT rather than an
// absence: it is what says "a session is not running here", which is exactly
// what a candidate's own rate should say.
func TestAnIdleAccountReportsZeroRatherThanNothing(t *testing.T) {
	reset := burnNow.Add(3 * time.Hour)
	prev := sample(40, burnNow, reset)
	cur := sample(40, burnNow.Add(9*time.Minute), reset)

	got, ok := BurnPerMin(prev, cur)
	if !ok {
		t.Fatal("an unchanged reading is a measured zero, not a missing one")
	}
	closeTo(t, got, 0)
}

// No baseline, no rate. The first reading after a start has nothing to subtract
// from, and a zero At is never an instant -- it is the year 1.
func TestWithoutABaselineThereIsNoRate(t *testing.T) {
	reset := burnNow.Add(3 * time.Hour)
	if _, ok := BurnPerMin(BindingSample{}, sample(40, burnNow, reset)); ok {
		t.Error("reported a rate with no previous reading")
	}
}

// Two readings at the same instant divide by zero. So does a clock that moved
// backwards.
func TestReadingsThatDoNotAdvanceTheClockYieldNoRate(t *testing.T) {
	reset := burnNow.Add(3 * time.Hour)
	prev := sample(10, burnNow, reset)
	if _, ok := BurnPerMin(prev, sample(20, burnNow, reset)); ok {
		t.Error("reported a rate from two readings at one instant")
	}
	if _, ok := BurnPerMin(prev, sample(20, burnNow.Add(-time.Minute), reset)); ok {
		t.Error("reported a rate from a clock that moved backwards")
	}
}

// A sample with no reset on either side is still comparable: the rollover test
// is what the reset is FOR, and a window that never named one cannot be shown
// to have rolled. Refusing here would silence the rate for every window whose
// reset the endpoint has not filled in yet, which is the state a fresh window
// is in and the state the projection most needs an answer for.
func TestAWindowWithNoResetOnEitherSideStillReportsARate(t *testing.T) {
	prev := sample(10, burnNow, time.Time{})
	cur := sample(40, burnNow.Add(10*time.Minute), time.Time{})

	got, ok := BurnPerMin(prev, cur)
	if !ok {
		t.Fatal("no rate from two readings that never named a reset")
	}
	closeTo(t, got, 3)
}

// How long the fleet would last at a given rate, which is the question the
// projection asks of every candidate: not "will this account run out on its own"
// but "how long would it carry the session that is running now".
func TestMinutesLeftIsRoomOverRate(t *testing.T) {
	left, ok := MinutesAtRate(27, 5.4)
	if !ok {
		t.Fatal("no answer from a positive rate")
	}
	closeTo(t, left, 5)
}

// A rate of zero means nothing is being spent, so nothing runs out. That is an
// absence of an answer and never "it lasts forever expressed as a number",
// because a caller comparing durations would read the zero as "already out".
func TestNothingIsSpentSoNothingRunsOut(t *testing.T) {
	if _, ok := MinutesAtRate(27, 0); ok {
		t.Error("answered a duration for a rate of zero")
	}
	if _, ok := MinutesAtRate(27, -1); ok {
		t.Error("answered a duration for a negative rate")
	}
}

// An account already out stays out: zero minutes, and the answer exists so a
// caller can tell it from "cannot say".
func TestAnAccountWithNoRoomLastsNoMinutes(t *testing.T) {
	left, ok := MinutesAtRate(0, 5.4)
	if !ok {
		t.Fatal("no answer for an account with no room")
	}
	closeTo(t, left, 0)
	left, ok = MinutesAtRate(-8, 5.4)
	if !ok {
		t.Fatal("no answer for an account past its limit")
	}
	closeTo(t, left, 0)
}

// Two readings of DIFFERENT windows are not a rate. The binding window moves on
// its own: a five-hour window that rolls over stops being the tightest, and the
// next reading is of the weekly window instead. Subtracting a fresh weekly 5%
// from a spent five-hour 96% is arithmetic on two unrelated quantities.
func TestTwoDifferentWindowsAreNotAPair(t *testing.T) {
	at := burnNow.Add(3 * time.Hour)
	prev := BindingSample{Window: WindowFiveHour, Pct: 20, At: burnNow, Reset: at}
	cur := BindingSample{Window: WindowSevenDay, Pct: 60, At: burnNow.Add(10 * time.Minute), Reset: at}
	if _, ok := BurnPerMin(prev, cur); ok {
		t.Error("reported a rate across two different windows")
	}
}

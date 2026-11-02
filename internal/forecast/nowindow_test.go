package forecast

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// weekSeat is a verified Ultra seat: five_hour reads null and the week is measured.
func weekSeat(uuid string, idx int, now time.Time, pct float64) Input {
	reset := now.Add(72 * time.Hour)
	return Input{
		UUID: uuid, Idx: idx, Eligible: true,
		Snapshot: &usage.Snapshot{SevenDay: usage.NewWindow(pf(pct), pt(reset))},
		Series:   weeklySeries(now, 4*time.Hour, []float64{pct - 2, pct - 1, pct}, reset, usage.WindowSevenDay),
	}
}

// A fleet with no five-hour window has no five-hour limit to run out of, and more
// readings will never settle a question nobody is asking.
func TestAFleetWithNoFiveHourWindowHasNoFiveHourAxis(t *testing.T) {
	now := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	got := Of([]Input{weekSeat("a", 1, now, 40), weekSeat("b", 2, now, 10)}, now)
	if got.FiveHour.Verdict != VerdictNoWindow {
		t.Errorf("FiveHour.Verdict = %v, want VerdictNoWindow", got.FiveHour.Verdict)
	}
	if got.Weekly.Verdict == VerdictNoWindow {
		t.Error("the week was reported as a window the fleet does not have")
	}
}

// One seat still on a temporary five-hour cap is enough for the axis to exist.
func TestOneSeatCarryingFiveHourKeepsTheAxis(t *testing.T) {
	now := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	capped := weekSeat("b", 2, now, 10)
	capped.Snapshot.FiveHour = usage.NewWindow(pf(0), pt(now.Add(time.Hour)))
	got := Of([]Input{weekSeat("a", 1, now, 40), capped}, now)
	if got.FiveHour.Verdict == VerdictNoWindow {
		t.Error("a fleet with a five-hour window on one seat was told it has none")
	}
}

// A window that came back unreadable is a limit nobody can see, which is a
// different sentence from a limit that is not there.
func TestAFiveHourWindowNobodyCouldReadIsNotAMissingOne(t *testing.T) {
	now := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	seat := weekSeat("a", 1, now, 40)
	seat.Snapshot.FiveHour = usage.NewWindow(nil, nil)
	if got := Of([]Input{seat}, now); got.FiveHour.Verdict == VerdictNoWindow {
		t.Error("an unreadable five-hour window was reported as no window")
	}
}

// An empty fleet says nothing about any axis: there is nobody to have no window.
func TestAnEmptyFleetIsNotMissingAFiveHourWindow(t *testing.T) {
	now := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	if got := Of(nil, now); got.FiveHour.Verdict != VerdictUnknown {
		t.Errorf("FiveHour.Verdict = %v, want VerdictUnknown", got.FiveHour.Verdict)
	}
}

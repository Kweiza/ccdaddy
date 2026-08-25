// This file is package view_test rather than package view, and it is the only
// one in this directory that is. Timestamp exists so that the packages that
// render a moment spell one absolute layout once, so the test that matters is
// the one those packages can write: through the exported surface, with nothing
// unexported in reach. Its siblings stay in package view because they reach
// unexported state on Row.
package view_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/forecast"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// The zone is part of the rendering, not part of the machine. A test that
// asserts a bare local string passes in the author's zone and prints a
// different hour in CI, where TZ is unset.
func TestTimestampAlwaysCarriesItsZone(t *testing.T) {
	// time.Local is pinned for the duration of this test, and without that pin
	// the nil row below rules nothing out. Nothing sets TZ in CI, so time.Local
	// is UTC there, and an implementation that resolved nil to time.Local would
	// render identically to one that resolves it to UTC -- measured: with that
	// substitution in place the nil assertion passes under TZ=UTC and fails
	// only on a machine whose zone happens not to be UTC. Pinning makes the row
	// decide the same thing everywhere. No test in this package calls
	// t.Parallel(), so the assignment is contained; Cleanup puts it back.
	saved := time.Local
	time.Local = time.FixedZone("XYZ", -7*3600)
	t.Cleanup(func() { time.Local = saved })

	at := time.Date(2026, 8, 27, 5, 10, 0, 0, time.UTC)
	kst := time.FixedZone("KST", 9*3600)
	if got, want := view.Timestamp(at, kst), "2026-08-27 14:10 KST"; got != want {
		t.Errorf("Timestamp(kst) = %q, want %q", got, want)
	}
	if got, want := view.Timestamp(at, time.UTC), "2026-08-27 05:10 UTC"; got != want {
		t.Errorf("Timestamp(utc) = %q, want %q", got, want)
	}
	// A nil location is the caller's bug, not a reason to print a wrong hour.
	if got, want := view.Timestamp(at, nil), "2026-08-27 05:10 UTC"; got != want {
		t.Errorf("Timestamp(nil) = %q, want %q", got, want)
	}
}

// The line is how status, list and the dashboard all agree on one wording, and
// the empty string is how all three know not to print anything at all. A
// machine with no history behind it must produce nothing here: a line saying
// "holds" on no evidence is the one output this whole feature exists to refuse.
func TestTheRunwayLineIsEmptyWithoutABasis(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	// Basis.Known false with two axes that both claim to hold: only the basis
	// may decide this, or a cold machine prints a promise.
	f := forecast.Fleet{
		FiveHour: forecast.Axis{Verdict: forecast.VerdictHolds},
		Weekly:   forecast.Axis{Verdict: forecast.VerdictHolds},
	}
	if got := view.RunwayLine(f, now, time.UTC); got != "" {
		t.Fatalf("RunwayLine = %q on a fleet with no basis; the empty string is how three renderers know not to print", got)
	}
}

// The line carries verdicts, not rates: a rate is per axis and two of them do
// not fit a line that also has to carry the answer. It carries no percentage
// either, because four existing tests forbid the substrings a percentage would
// bring with it into ccdad status's human output.
func TestTheRunwayLineNamesTheAxisThatRunsDryFirst(t *testing.T) {
	// Fifty-four hours before the dry moment, so the relative span in the line
	// is checkable by hand against the absolute one beside it.
	now := time.Date(2026, 8, 24, 23, 10, 0, 0, time.UTC)
	kst := time.FixedZone("KST", 9*3600)
	dry := time.Date(2026, 8, 27, 5, 10, 0, 0, time.UTC)
	f := forecast.Fleet{
		Basis:    forecast.Basis{Observed: 3*time.Hour + 51*time.Minute, Known: true},
		FiveHour: forecast.Axis{Burn: forecast.Band{Low: 61, Known: true}, Verdict: forecast.VerdictHolds},
		Weekly: forecast.Axis{
			Burn: forecast.Band{Low: 4.2, Known: true}, Verdict: forecast.VerdictRunsDry,
			DryAt: dry, HasDryAt: true,
		},
		// The fleet run burns both axes at once, so it can never outlast the
		// axis that empties first. Here it agrees with the weekly one, which is
		// the ordinary case and the one that must not add a third clause.
		Both: forecast.Axis{Verdict: forecast.VerdictRunsDry, DryAt: dry, HasDryAt: true},
	}
	got := view.RunwayLine(f, now, kst)
	want := "7d dry 2026-08-27 14:10 KST (2d6h)  ·  5h holds  ·  basis 3h51m"
	if got != want {
		t.Fatalf("RunwayLine =\n\t%q\nwant\n\t%q", got, want)
	}
	// A rate on this line would drag a percentage onto ccdad status's human
	// stdout, where four tests forbid one.
	if strings.ContainsAny(got, "%") || strings.Contains(got, "pp/h") {
		t.Errorf("RunwayLine = %q; the line carries verdicts, not rates", got)
	}
}

// Both axes holding is one claim, not two, and it says at what: "at this rate"
// is the whole qualification the measurement supports.
func TestTheRunwayLineSaysBothWhenBothHold(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	f := forecast.Fleet{
		Basis:    forecast.Basis{Observed: 3*time.Hour + 51*time.Minute, Known: true},
		FiveHour: forecast.Axis{Verdict: forecast.VerdictHolds},
		Weekly:   forecast.Axis{Verdict: forecast.VerdictHolds},
		Both:     forecast.Axis{Verdict: forecast.VerdictHolds},
	}
	if got, want := view.RunwayLine(f, now, time.UTC), "holds on both axes at this rate  ·  basis 3h51m"; got != want {
		t.Fatalf("RunwayLine = %q, want %q", got, want)
	}
}

// An axis whose two runs disagreed has no verdict, and the line has to say so
// rather than leave it out. Omitting it would let a reader take the axis that
// IS reported as covering the fleet, which is the fail-open reading of a
// tri-state that this repository does not permit anywhere else.
func TestAnUndecidedAxisIsPrintedRatherThanOmitted(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	f := forecast.Fleet{
		Basis:    forecast.Basis{Observed: 40 * time.Minute, Known: true},
		FiveHour: forecast.Axis{Verdict: forecast.VerdictUnknown},
		Weekly:   forecast.Axis{Verdict: forecast.VerdictHolds},
		Both:     forecast.Axis{Verdict: forecast.VerdictUnknown},
	}
	got := view.RunwayLine(f, now, time.UTC)
	if !strings.Contains(got, "5h ") {
		t.Fatalf("RunwayLine = %q, which says nothing about the five-hour axis; silence there reads as agreement with the axis that is named", got)
	}
	if strings.Contains(got, "5h holds") {
		t.Fatalf("RunwayLine = %q, which promises an axis whose two runs disagreed", got)
	}
}

// The fleet run burns both axes at once, so it has strictly more burn and
// strictly more ways to end than either axis alone: it can empty a fleet whose
// two axes each hold on their own. Letting "holds on both axes" stand over that
// fleet would be the one fail-open reading this line must not permit.
func TestAFleetThatEmptiesWithBothAxesBurningIsNotReportedAsHolding(t *testing.T) {
	now := time.Date(2026, 8, 24, 23, 10, 0, 0, time.UTC)
	dry := time.Date(2026, 8, 27, 5, 10, 0, 0, time.UTC)
	f := forecast.Fleet{
		Basis:    forecast.Basis{Observed: 4 * time.Hour, Known: true},
		FiveHour: forecast.Axis{Verdict: forecast.VerdictHolds},
		Weekly:   forecast.Axis{Verdict: forecast.VerdictHolds},
		Both:     forecast.Axis{Verdict: forecast.VerdictRunsDry, DryAt: dry, HasDryAt: true},
	}
	got := view.RunwayLine(f, now, time.UTC)
	if strings.Contains(got, "holds on both axes") {
		t.Fatalf("RunwayLine = %q; both axes held separately and the fleet still empties", got)
	}
	if !strings.Contains(got, "2026-08-27 05:10 UTC") {
		t.Fatalf("RunwayLine = %q, which names no moment for a fleet the arithmetic empties", got)
	}
}

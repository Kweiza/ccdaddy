package view_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/forecast"
	"github.com/Kweiza/ccdaddy/internal/view"
)

var weekOnlyNow = time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)

func weekOnlyFleet(weekly, both forecast.Verdict) forecast.Fleet {
	return forecast.Fleet{
		Basis:    forecast.Basis{Observed: 4 * time.Hour, Known: true},
		FiveHour: forecast.Axis{Verdict: forecast.VerdictNoWindow},
		Weekly:   forecast.Axis{Verdict: weekly},
		Both:     forecast.Axis{Verdict: both},
	}
}

// One limit is not "both", and an axis that does not exist has no doubt to report.
// This is the line a fleet of verified Ultra seats would otherwise carry forever.
func TestARunwayLineWithNoFiveHourAxisHoldsOnTheOneItHas(t *testing.T) {
	got := view.RunwayLine(weekOnlyFleet(forecast.VerdictHolds, forecast.VerdictHolds), weekOnlyNow, time.UTC)
	if want := "holds at this rate  ·  basis 4h00m"; got != want {
		t.Fatalf("RunwayLine = %q, want %q", got, want)
	}
}

func TestARunwayLineWithNoFiveHourAxisStillReportsADryWeek(t *testing.T) {
	f := weekOnlyFleet(forecast.VerdictRunsDry, forecast.VerdictRunsDry)
	f.Weekly.HasDryAt, f.Weekly.DryAt = true, weekOnlyNow.Add(6*time.Hour)
	f.Both.HasDryAt, f.Both.DryAt = true, weekOnlyNow.Add(6*time.Hour)
	got := view.RunwayLine(f, weekOnlyNow, time.UTC)
	if !strings.Contains(got, "7d dry") {
		t.Errorf("RunwayLine = %q, want the week named dry", got)
	}
	if strings.Contains(got, "5h") {
		t.Errorf("RunwayLine = %q names an axis the fleet does not have", got)
	}
}

func TestTheCompactRunwayHasNoClauseForAnAbsentAxis(t *testing.T) {
	parts := view.CompactRunwayLines(weekOnlyFleet(forecast.VerdictHolds, forecast.VerdictHolds), weekOnlyNow, time.UTC)
	got := strings.Join(parts, "|")
	if want := "7d holds|basis 4h00m"; got != want {
		t.Errorf("compact = %q, want %q", got, want)
	}
}

// The doubt on an axis that DOES exist still has to show.
func TestAnUndecidedWeekStillSaysSoWithNoFiveHourAxis(t *testing.T) {
	got := view.RunwayLine(weekOnlyFleet(forecast.VerdictUnknown, forecast.VerdictUnknown), weekOnlyNow, time.UTC)
	if !strings.Contains(got, "7d cannot tell yet") {
		t.Errorf("RunwayLine = %q", got)
	}
}

func TestAnAbsentAxisVerdictIsNoQuantityRatherThanUnreadable(t *testing.T) {
	a := forecast.Axis{Verdict: forecast.VerdictNoWindow}
	if got := view.RunwayVerdict(a, weekOnlyNow, time.UTC); got != view.NoQuantity {
		t.Errorf("RunwayVerdict = %q, want %q", got, view.NoQuantity)
	}
}

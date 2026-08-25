package forecast

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/history"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// sampleAt builds one sample carrying the same reading under both the five-hour
// and the seven-day window name.
//
// One reading filed under two names rather than a fourth argument: every test
// here measures exactly one name and none of them cares what the other window
// is doing, so a name parameter would be the same constant repeated down every
// fixture. Carrying both also keeps a fixture usable when a test switches which
// axis it asks about, which is how the five-hour and seven-day cases below came
// to share one builder.
func sampleAt(at time.Time, pct float64, reset time.Time) history.Sample {
	r := history.Reading{Pct: pct, Reset: reset}
	return history.Sample{
		At: at,
		Windows: map[usage.WindowName]history.Reading{
			usage.WindowFiveHour: r,
			usage.WindowSevenDay: r,
		},
	}
}

// The sub-second part of resets_at comes from the server's clock at request
// time, not from the window's anchor. Two readings of one account's five-hour
// window 72 minutes apart on 2026-08-25 differed only below the second:
//
//	08:10:38  five_hour  resets_at 2026-08-25T01:50:00.308482Z
//	09:22:39  five_hour  resets_at 2026-08-25T01:50:00.320288Z
//
// A rollover test keyed on equality therefore splits every consecutive pair,
// sums over an empty set of pairs, and reports 0.0 points per hour forever --
// while every surface prints "holds" on a fleet being drained and the whole
// suite stays green, because a unit test that fixes the reset to a constant
// never sees it.
func TestSubSecondJitterInResetDoesNotSplitASeries(t *testing.T) {
	base := time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)
	reset := time.Date(2026, 8, 25, 14, 0, 0, 0, time.UTC)
	jittered := make([]history.Sample, 0, 5)
	steady := make([]history.Sample, 0, 5)
	for i := range 5 {
		at := base.Add(time.Duration(i) * 30 * time.Minute)
		pct := float64(10 + 2*i)
		jittered = append(jittered, sampleAt(at, pct, reset.Add(time.Duration(i)*137*time.Microsecond)))
		steady = append(steady, sampleAt(at, pct, reset))
	}
	from, to := base, base.Add(2*time.Hour)
	gotJ, coverJ, _, okJ := windowRate(jittered, usage.WindowSevenDay, from, to)
	gotS, coverS, _, okS := windowRate(steady, usage.WindowSevenDay, from, to)
	if !okJ || !okS {
		t.Fatalf("ok = %v, %v; both series are long enough to rate", okJ, okS)
	}
	if gotJ != gotS || coverJ != coverS {
		t.Fatalf("jitter changed the measurement: filled %v vs %v, cover %v vs %v", gotJ, gotS, coverJ, coverS)
	}
	if gotS != 8 {
		t.Fatalf("filled = %v, want 8 (10 to 18 over four steps)", gotS)
	}
}

// A rollover costs nothing and the rise after it is counted. It is detected
// rather than read: a drop in the percentage, or a reset that moved forward by
// at least half the window's length.
func TestARolloverCostsNothingAndTheRiseAfterItCounts(t *testing.T) {
	base := time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)
	r1 := base.Add(2 * time.Hour)
	r2 := r1.Add(5 * time.Hour)
	s := []history.Sample{
		sampleAt(base, 80, r1),
		sampleAt(base.Add(30*time.Minute), 90, r1),
		sampleAt(base.Add(2*time.Hour+time.Minute), 3, r2), // rolled over
		sampleAt(base.Add(3*time.Hour), 9, r2),
	}
	filled, _, _, ok := windowRate(s, usage.WindowFiveHour, base, base.Add(4*time.Hour))
	if !ok {
		t.Fatal("ok = false")
	}
	// 10 before the rollover, 6 after it, and the fall of 87 costs nothing.
	if filled != 16 {
		t.Fatalf("filled = %v, want 16", filled)
	}
}

// The percentage arm of the rollover test, asserted on the predicate itself.
//
// It cannot be asserted through windowRate: the clamp in that loop already
// discards a negative difference, so a fall of 87 contributes zero whether the
// arm fires or not, and removing the arm leaves every rate in this file
// unchanged. The arm still has to exist, because it is the only signal left
// when no reset was reported -- a reading whose resets_at this build could not
// read has the zero time, which cannot be differenced against anything.
func TestADropInPercentageIsARolloverWhenNoResetCanSaySo(t *testing.T) {
	const fiveHours = 5 * time.Hour
	if !rolledOver(history.Reading{Pct: 90}, history.Reading{Pct: 3}, fiveHours) {
		t.Error("a fall from 90 to 3 with no reported reset was not read as a rollover")
	}
	if rolledOver(history.Reading{Pct: 3}, history.Reading{Pct: 90}, fiveHours) {
		t.Error("a rise from 3 to 90 with no reported reset was read as a rollover; nothing but a fall or a moved reset is one")
	}
	r := time.Date(2026, 8, 25, 14, 0, 0, 0, time.UTC)
	if rolledOver(history.Reading{Pct: 10, Reset: r}, history.Reading{Pct: 12, Reset: r}, fiveHours) {
		t.Error("an ordinary rise inside one window was read as a rollover")
	}
}

// An equal-percentage rollover is invisible to a drop test, which is why the
// reset half of the predicate exists.
func TestAnEqualPercentageRolloverIsStillARollover(t *testing.T) {
	base := time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)
	r1 := base.Add(time.Hour)
	r2 := r1.Add(5 * time.Hour)
	s := []history.Sample{
		sampleAt(base, 40, r1),
		sampleAt(base.Add(30*time.Minute), 40, r1),
		sampleAt(base.Add(70*time.Minute), 40, r2), // same pct, new window
		sampleAt(base.Add(2*time.Hour), 41, r2),
	}
	filled, _, _, _ := windowRate(s, usage.WindowFiveHour, base, base.Add(4*time.Hour))
	if filled != 1 {
		t.Fatalf("filled = %v, want 1", filled)
	}
}

// The rollover that makes the reset arm load-bearing: one where the percentage
// RISES across the boundary.
//
// A rollover whose percentage is equal on both sides contributes zero with the
// arm and zero without it, so the equal case above cannot tell the two apart --
// it documents the intent and catches nothing. Only a strictly rising pair does,
// and it is the realistic shape: an account burning hard crosses its rollover
// and the next poll finds the fresh window already further along than the old
// one was. Counting that difference would credit the new window's consumption
// against the old window's balance, which is not a measurement of either.
func TestARolloverThatRaisesThePercentageIsStillARollover(t *testing.T) {
	base := time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)
	r1 := base.Add(time.Hour)
	r2 := r1.Add(5 * time.Hour)
	s := []history.Sample{
		sampleAt(base, 20, r1),
		sampleAt(base.Add(30*time.Minute), 22, r1),
		sampleAt(base.Add(70*time.Minute), 30, r2), // higher pct, new window
		sampleAt(base.Add(2*time.Hour), 33, r2),
	}
	filled, _, _, ok := windowRate(s, usage.WindowFiveHour, base, base.Add(4*time.Hour))
	if !ok {
		t.Fatal("ok = false")
	}
	// 2 before the rollover and 3 after it. Reading the pair across the boundary
	// as consumption would add 8 more.
	if filled != 5 {
		t.Fatalf("filled = %v, want 5", filled)
	}
}

// Below the gates there is no rate, and no rate is not a zero.
func TestTheGatesRefuseRatherThanReportZero(t *testing.T) {
	base := time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)
	reset := base.Add(6 * time.Hour)
	two := []history.Sample{sampleAt(base, 10, reset), sampleAt(base.Add(time.Hour), 12, reset)}
	if _, _, _, ok := windowRate(two, usage.WindowSevenDay, base, base.Add(4*time.Hour)); ok {
		t.Error("two samples produced a rate; three are required, because two hold one difference and one difference of a whole-percent field is a quantisation step")
	}
	burst := []history.Sample{
		sampleAt(base, 10, reset),
		sampleAt(base.Add(time.Minute), 11, reset),
		sampleAt(base.Add(3*time.Minute), 12, reset),
	}
	if _, _, _, ok := windowRate(burst, usage.WindowSevenDay, base, base.Add(4*time.Hour)); ok {
		t.Error("a three-minute burst produced a rate; the twenty-minute span floor must refuse it")
	}
}

// A window absent from the newest sample is unmeasured, not a rate frozen from
// stale evidence.
func TestAWindowMissingFromTheNewestSampleIsUnmeasured(t *testing.T) {
	base := time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)
	reset := base.Add(6 * time.Hour)
	s := []history.Sample{
		sampleAt(base, 10, reset),
		sampleAt(base.Add(time.Hour), 12, reset),
		sampleAt(base.Add(2*time.Hour), 14, reset),
		{At: base.Add(3 * time.Hour)}, // the window dropped out of the response
	}
	if _, _, _, ok := windowRate(s, usage.WindowSevenDay, base, base.Add(4*time.Hour)); ok {
		t.Error("a window absent from the newest sample was still rated")
	}
}

// Consumption is summed over one shared span, not rates over their own. An
// account with forty minutes of coverage and one with four hours must give the
// fleet the sum of their consumption over the shared span -- summing their RATES
// projects the short account's burst as though the fleet had sustained it for
// four hours.
func TestConsumptionIsSummedOverOneSharedSpan(t *testing.T) {
	// A: 8 points over 4 hours = 2 points per hour on its own.
	// B: 4 points over 40 minutes = 6 points per hour on its own.
	// Summed rates: 8. Summed consumption over the shared four hours: 3.
	got := fleetBand(12, 2, 4*time.Hour)
	if got.Low != 3 {
		t.Fatalf("Low = %v, want 3 — 12 points over the shared four hours", got.Low)
	}
	// The band's upper bound carries one quantisation step per contributor.
	if got.High != 3.5 {
		t.Fatalf("High = %v, want 3.5 — (12+2)/4", got.High)
	}

	// The divisor is the span that was observed, not the span that was asked
	// for. A fleet watched for two hours has two hours of evidence, and the
	// four-hour figure above cannot tell the two divisors apart because they
	// are the same number there.
	half := fleetBand(12, 2, 2*time.Hour)
	if half.Low != 6 || half.High != 7 {
		t.Fatalf("fleetBand(12, 2, 2h) = %v/%v, want 6/7 — dividing by the four-hour window instead of the observed span halves a rate that was measured over half the time", half.Low, half.High)
	}

	// No contributors and no span are not a rate of zero.
	for _, c := range []struct {
		name         string
		filled       float64
		contributors int
		span         time.Duration
	}{
		{"nobody contributed", 0, 0, 4 * time.Hour},
		{"no time was observed", 12, 2, 0},
		{"the span ran backwards", 12, 2, -time.Hour},
	} {
		if b := fleetBand(c.filled, c.contributors, c.span); b.Known {
			t.Errorf("%s: Known = true (%v/%v); an unmeasured fleet is not an idle one", c.name, b.Low, b.High)
		}
	}
}

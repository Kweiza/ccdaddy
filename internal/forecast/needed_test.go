package forecast

import (
	"fmt"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// The answer is a simulation result, not burn divided by replenishment. The
// closed form is the comparison a verdict is already forbidden to be built on,
// and a fleet carrying both would be free to print "runs dry" and "you have
// enough accounts" on adjacent lines.
//
// The two disagree whenever an account supplies replenishment it cannot
// deliver. Here four of the six accounts are out on the weekly axis and nothing
// brings them back, so the comparison counts six accounts' five-hour supply --
// 6 x 100/5 h = 120 points an hour against 30 burned, which reads as "no more
// accounts needed at all" -- while the rotation can only ever reach two of
// them. Five accounts of this fleet run dry; it needs all six.
func TestNeededIsASimulationResultAndNotTheComparison(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	fiveReset := now.Add(5 * time.Hour)
	in := make([]Input, 0, 6)
	for i := range 6 {
		// Nineteen points over the four-hour span on every account: 114 points
		// filled is 28.5 an hour, and six contributors put the upper bound the
		// search runs at at (114+6)/4 = 30 exactly.
		snap := &usage.Snapshot{FiveHour: usage.NewWindow(pf(0), pt(fiveReset))}
		if i >= 2 {
			// Out on the weekly axis, with a reading that carried no
			// resets_at. Such a window is burned and never rolled over, so
			// these four are out for the whole horizon -- and it is their
			// WEEKLY window that strands them, so the five-hour run cannot
			// hand them work either.
			snap.SevenDay = usage.NewWindow(pf(100), nil)
		}
		in = append(in, Input{
			UUID: fmt.Sprintf("uuid-%d", i), Idx: i + 1, Eligible: true,
			Snapshot: snap,
			Series:   weeklySeries(now, 4*time.Hour, []float64{0, 9.5, 19}, fiveReset, usage.WindowFiveHour),
		})
	}
	f := Of(in, now)

	if f.FiveHour.Burn.High != 30 {
		t.Fatalf("five-hour burn band = %v/%v, want an upper bound of 30", f.FiveHour.Burn.Low, f.FiveHour.Burn.High)
	}
	if f.AccountsUsable != 6 {
		t.Fatalf("AccountsUsable = %d, want 6 -- an account that is out is still one the run had to work with", f.AccountsUsable)
	}
	if !f.HasNeeded {
		t.Fatal("HasNeeded = false on a measured fleet")
	}
	if f.AccountsNeeded <= 2 {
		t.Fatalf("AccountsNeeded = %d; burn divided by one account's replenishment is 30/20 = 2, and the run needs more than that because four of the six seats supply nothing", f.AccountsNeeded)
	}
}

// needed is computed at the upper bound of the measured band, for the same
// reason "holds" is: it has to be the number that is PROVABLY enough. A fleet
// told it needs two accounts and given two must not then run dry because the
// measurement was coarse.
//
// One account replenishes the five-hour axis at 100/5 h = 20 points an hour, so
// this band straddles the boundary of what a single account can carry: the low
// end says one account is enough and the high end says it is not.
func TestNeededIsComputedAtTheUpperBoundOfTheBand(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	reset := now.Add(30 * time.Minute)
	// 79.5 points filled over the four-hour span, one contributor: the band is
	// 19.875 and 20.125, one end either side of 20.
	series := weeklySeries(now, 4*time.Hour, []float64{0, 39.75, 79.5}, reset, usage.WindowFiveHour)
	snap := &usage.Snapshot{FiveHour: usage.NewWindow(pf(79.5), pt(reset))}
	f := Of([]Input{{UUID: "uuid-a", Idx: 1, Eligible: true, Snapshot: snap, Series: series}}, now)

	if f.FiveHour.Burn.Low != 19.875 || f.FiveHour.Burn.High != 20.125 {
		t.Fatalf("burn band = %v/%v, want 19.875/20.125", f.FiveHour.Burn.Low, f.FiveHour.Burn.High)
	}
	if !f.HasNeeded {
		t.Fatal("HasNeeded = false on a measured fleet")
	}
	if f.AccountsNeeded != 2 {
		t.Fatalf("AccountsNeeded = %d, want 2 -- the low end of the band asks for one account and the high end asks for two, and the promise has to survive the high end", f.AccountsNeeded)
	}
}

// A hypothetical account carries the window names the fleet's own accounts
// carry. A scoped weekly cap is a real constraint a new seat would arrive with,
// and a hypothetical account without one answers a question about a different
// fleet: it would absorb the fleet's scoped burn for free, because the run
// charges the live account only the windows that account has.
//
// The two fleets here are identical but for the cap, and the cap is three times
// tighter than the plain weekly window beside it -- so the fleet carrying it
// needs some three times the seats. A hypothetical account that did not carry
// it would leave the answer near the uncapped fleet's.
func TestAHypotheticalAccountCarriesTheFleetsOwnWindows(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	reset := now.Add(168 * time.Hour)
	scoped := usage.ScopedWindowName(usage.ScopeModel, "Fable")

	uncapped := make([]Input, 0, 2)
	capped := make([]Input, 0, 2)
	for i := range 2 {
		// Three points of seven_day over the four-hour span on each account:
		// 6 filled over two contributors is a band of 1.5 and 2.0.
		plain := []float64{0, 1.5, 3}
		uncapped = append(uncapped, Input{
			UUID: fmt.Sprintf("uuid-%d", i), Idx: i + 1, Eligible: true,
			Snapshot: snapshotWithWeekly(0, reset),
			Series:   weeklySeries(now, 4*time.Hour, plain, reset, usage.WindowSevenDay),
		})
		// The same fleet, plus a scoped cap rising eleven points over the same
		// span: 22 filled over two contributors is a band of 5.5 and 6.0.
		snap := snapshotWithWeekly(0, reset)
		snap.Limits = []usage.Limit{usage.LimitFor(usage.LimitInput{
			Kind: "weekly_scoped", Model: "Fable", Percent: pf(0), ResetsAt: pt(reset),
		})}
		capped = append(capped, Input{
			UUID: fmt.Sprintf("uuid-%d", i), Idx: i + 1, Eligible: true,
			Snapshot: snap,
			Series: namedSeries(now, 4*time.Hour, reset, map[usage.WindowName][]float64{
				usage.WindowSevenDay: plain,
				scoped:               {0, 5.5, 11},
			}),
		})
	}

	loose, tight := Of(uncapped, now), Of(capped, now)
	if !loose.HasNeeded || !tight.HasNeeded {
		t.Fatalf("HasNeeded = %v and %v, want both measured", loose.HasNeeded, tight.HasNeeded)
	}
	if tight.AccountsNeeded <= 2*loose.AccountsNeeded {
		t.Fatalf("the capped fleet needs %d accounts and the uncapped one %d; a cap three times tighter than the window beside it needs some three times the seats, and an answer this close to the uncapped fleet's is one where the seats being added do not carry the cap",
			tight.AccountsNeeded, loose.AccountsNeeded)
	}
}

// The spare count is the same search run downward, so a fleet that holds learns
// how much slack it has and not merely that it has some.
//
// Three accounts replenish the five-hour axis at 60 points an hour against 15
// burned, and one alone replenishes at 20: this fleet holds on a third of
// itself.
func TestAHoldingFleetReportsTheSmallestCountThatStillHolds(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	fiveReset := now.Add(5 * time.Hour)
	in := make([]Input, 0, 3)
	for i := range 3 {
		// Nineteen points each over the four-hour span: 57 filled over three
		// contributors is a band of 14.25 and 15.0.
		in = append(in, Input{
			UUID: fmt.Sprintf("uuid-%d", i), Idx: i + 1, Eligible: true,
			Snapshot: &usage.Snapshot{FiveHour: usage.NewWindow(pf(0), pt(fiveReset))},
			Series:   weeklySeries(now, 4*time.Hour, []float64{0, 9.5, 19}, fiveReset, usage.WindowFiveHour),
		})
	}
	f := Of(in, now)

	if f.FiveHour.Verdict != VerdictHolds {
		t.Fatalf("FiveHour.Verdict = %v, want VerdictHolds -- this fixture is about a fleet with room to spare", f.FiveHour.Verdict)
	}
	if !f.HasNeeded {
		t.Fatal("HasNeeded = false on a measured fleet")
	}
	if f.AccountsNeeded != 1 {
		t.Fatalf("AccountsNeeded = %d, want 1 -- one account replenishes 20 points an hour against 15 burned, so the search downward has to reach it", f.AccountsNeeded)
	}
	if f.AccountsNeeded >= f.AccountsUsable {
		t.Fatalf("AccountsNeeded = %d of %d usable; a fleet that holds on fewer accounts than it has must say how many fewer", f.AccountsNeeded, f.AccountsUsable)
	}
}

// The search is bounded, and past the bound the honest answer is the bound.
//
// At 256 accounts the weekly axis replenishes 256 x 100/168 h = 152 points an
// hour, and this fixture burns 202: no count inside the bound can hold, so the
// search reaches the ceiling. A fleet that appears to need more than that has a
// measurement problem rather than a purchasing one, and saying so is more use
// than a number.
func TestAnAbsurdBurnReportsTheBoundRatherThanANumber(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	reset := now.Add(167 * time.Hour)
	in := make([]Input, 0, 8)
	for i := range 8 {
		// A whole weekly window burned inside the four-hour span, on eight
		// accounts: 800 points filled over eight contributors is a band of 200
		// and 202. The level comes from the snapshot and the slope from the
		// series, which is why a fleet reading zero can carry a furious rate --
		// and an absurd measured rate is exactly what the bound is for.
		in = append(in, Input{
			UUID: fmt.Sprintf("uuid-%d", i), Idx: i + 1, Eligible: true,
			Snapshot: snapshotWithWeekly(0, reset),
			Series:   weeklySeries(now, 4*time.Hour, []float64{0, 50, 100}, reset, usage.WindowSevenDay),
		})
	}
	f := Of(in, now)

	if f.Weekly.Burn.High != 202 {
		t.Fatalf("weekly burn band = %v/%v, want an upper bound of 202", f.Weekly.Burn.Low, f.Weekly.Burn.High)
	}
	if !f.HasNeeded {
		t.Fatal("HasNeeded = false; a fleet past the bound still has an answer, and the answer is the bound")
	}
	if !f.NeededCapped {
		t.Fatalf("NeededCapped = false with AccountsNeeded = %d; a count reported as exact is a count somebody can go and buy", f.AccountsNeeded)
	}
	if f.AccountsNeeded != maxNeeded {
		t.Fatalf("AccountsNeeded = %d, want the bound %d", f.AccountsNeeded, maxNeeded)
	}
}

// Which axis sets the figure is MEASURED. The two axes imply counts in the
// ratio of their measured rates against 20 and 100/168 points an hour, and
// nothing in the design fixes which is larger -- so both fleets here are
// ordinary and they name different axes.
func TestTheBindingAxisIsMeasuredAndCanBeEither(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	fiveReset, weeklyReset := now.Add(5*time.Hour), now.Add(168*time.Hour)

	// Fleet of two, burning 30 points an hour of the five-hour axis and half a
	// point of the weekly one. Two accounts replenish the five-hour axis at 40
	// and one at 20, so the five-hour axis is what asks for the second seat;
	// half a point an hour is 84 points a week, which one account carries.
	fiveBound := fleetOfTwo(now, fiveReset, weeklyReset,
		[]float64{0, 29.5, 59}, []float64{0, 0, 0})
	// The same fleet burning 5 points an hour of the five-hour axis -- one seat
	// carries that -- and 4 of the weekly one, which is 672 points a week and
	// takes seven.
	weeklyBound := fleetOfTwo(now, fiveReset, weeklyReset,
		[]float64{0, 4.5, 9}, []float64{0, 3.5, 7})

	for _, c := range []struct {
		name string
		in   []Input
		want usage.WindowName
	}{
		{"five-hour", fiveBound, usage.WindowFiveHour},
		{"weekly", weeklyBound, usage.WindowSevenDay},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := Of(c.in, now)
			if !f.HasNeeded {
				t.Fatal("HasNeeded = false on a measured fleet")
			}
			if !f.HasNeededBy {
				t.Fatalf("HasNeededBy = false with AccountsNeeded = %d; one axis was still failing one account below the answer", f.AccountsNeeded)
			}
			if f.NeededBy != c.want {
				t.Errorf("NeededBy = %q, want %q -- the binding axis is read off the run and not fixed by the design", f.NeededBy, c.want)
			}
		})
	}
}

// fleetOfTwo is two accounts carrying both axes, each following its own column
// of percentages. Two contributors over the four-hour span put every band's
// upper end half a point above its lower one.
func fleetOfTwo(now, fiveReset, weeklyReset time.Time, five, weekly []float64) []Input {
	in := make([]Input, 0, 2)
	for i := range 2 {
		in = append(in, Input{
			UUID: fmt.Sprintf("uuid-%d", i), Idx: i + 1, Eligible: true,
			Snapshot: &usage.Snapshot{
				FiveHour: usage.NewWindow(pf(0), pt(fiveReset)),
				SevenDay: usage.NewWindow(pf(0), pt(weeklyReset)),
			},
			Series: namedSeries(now, 4*time.Hour, weeklyReset, map[usage.WindowName][]float64{
				usage.WindowFiveHour: five,
				usage.WindowSevenDay: weekly,
			}),
		})
	}
	return in
}

// A fleet nobody has enough readings for gets no count at all. Zero would read
// as "you need no more accounts", which is the one answer the evidence cannot
// carry -- and it is the answer a reader would act on.
func TestAFleetWithNoBasisReportsNoCountRatherThanZero(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	reset := now.Add(72 * time.Hour)
	// A snapshot and no series at all: the levels are readable and no rate is.
	f := Of([]Input{{UUID: "uuid-a", Idx: 1, Eligible: true, Snapshot: snapshotWithWeekly(40, reset)}}, now)

	if f.Basis.Known {
		t.Fatal("the fixture measured something; it is meant to have no basis at all")
	}
	if f.AccountsUsable != 1 {
		t.Errorf("AccountsUsable = %d, want 1 -- the account is readable, which is a different question from whether it was measured", f.AccountsUsable)
	}
	if f.HasNeeded {
		t.Errorf("HasNeeded = true with AccountsNeeded = %d on a fleet with no measured rate", f.AccountsNeeded)
	}
	if f.HasNeededBy || f.NeededCapped {
		t.Errorf("HasNeededBy = %v and NeededCapped = %v, want both false where there was nothing to search", f.HasNeededBy, f.NeededCapped)
	}
}

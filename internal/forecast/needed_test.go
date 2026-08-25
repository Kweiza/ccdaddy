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
// The two disagree whenever an account supplies replenishment it cannot deliver.
// Four of the six accounts here are out on the weekly axis and nothing brings
// them back, so the closed form counts six seats' five-hour supply and the
// rotation can only ever reach two of them.
//
// The fleet is deliberately SHORT, so the answer comes from appending seats the
// fleet does not have. That is what keeps this test a test of the MECHANISM: the
// upward search never slices the fleet, so nothing here turns on which of its own
// accounts a downward search would have dropped first. A fixture that held would
// be answered by that ordering instead, and would fail a pure-simulation build
// that ordered seats the other way -- accusing it of being the comparison for a
// reason that has nothing to do with the comparison.
//
// The arithmetic: 49.125 points an hour burned, one account replenishes 20, so
// the closed form asks for ceil(49.125/20) = 3. The run asks for 7, because four
// of the fleet's six seats supply nothing and it takes three live ones to carry
// the rate.
func TestNeededIsASimulationResultAndNotTheComparison(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	fiveReset := now.Add(5 * time.Hour)
	in := make([]Input, 0, 6)
	for i := range 6 {
		// 31.75 points over the four-hour span on every account: 190.5 points
		// filled is 47.625 an hour, and six contributors put the upper bound the
		// search runs at at (190.5+6)/4 = 49.125.
		snap := &usage.Snapshot{FiveHour: usage.NewWindow(pf(0), pt(fiveReset))}
		if i >= 2 {
			// Out on the weekly axis, with a reading that carried no resets_at.
			// Such a window is burned and never rolled over, so these four are
			// out for the whole horizon -- and it is their WEEKLY window that
			// strands them, so the five-hour run cannot hand them work either.
			snap.SevenDay = usage.NewWindow(pf(100), nil)
		}
		in = append(in, Input{
			UUID: fmt.Sprintf("uuid-%d", i), Idx: i + 1, Eligible: true,
			Snapshot: snap,
			Series:   weeklySeries(now, 4*time.Hour, []float64{0, 15.875, 31.75}, fiveReset, usage.WindowFiveHour),
		})
	}
	f := Of(in, now)

	if f.FiveHour.Burn.High != 49.125 {
		t.Fatalf("five-hour burn band = %v/%v, want an upper bound of 49.125", f.FiveHour.Burn.Low, f.FiveHour.Burn.High)
	}
	if f.Both.Verdict != VerdictRunsDry {
		t.Fatalf("Both.Verdict = %v, want VerdictRunsDry -- this fixture is about a fleet that has to be given seats it does not have", f.Both.Verdict)
	}
	if f.AccountsUsable != 6 {
		t.Fatalf("AccountsUsable = %d, want 6 -- an account that is out is still one the run had to work with", f.AccountsUsable)
	}
	if !f.HasNeeded {
		t.Fatal("HasNeeded = false on a measured fleet")
	}
	if f.AccountsNeeded != 7 {
		t.Fatalf("AccountsNeeded = %d, want 7; burn divided by one account's replenishment is 49.125/20 = 3, and the run needs four seats more than that because four of the six it has supply nothing", f.AccountsNeeded)
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

// AccountsUsable counts the accounts the ROTATION can reach and nobody else. It
// is the left half of the rendered accounts line and the denominator the
// parenthetical is read against, so an account counted into it that the run
// never had is a seat the fleet does not have -- and the figure understates how
// many the owner must go and buy, which is fail-open on the one number this
// feature exists to print.
//
// Three populations, all non-empty here, because len(inputs), len(readable) and
// len(usable) are the same number on any fleet where they are not: an ineligible
// account is quota the rotation cannot switch to, and an account with no
// readable window is quota nobody can see. Input.Eligible's zero value excludes
// for that reason.
//
// The subtraction is asserted beside the count because it is the cross-check the
// field's own documentation offers and the one a consumer of the JSON is told to
// make. Neither PointsTotal nor a replenishment denominator is that check --
// uuid-five below carries a five-hour window and no weekly one, so it is in this
// count and in neither of those.
func TestAccountsUsableCountsOnlyTheSeatsTheRotationHad(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	fiveReset, weeklyReset := now.Add(5*time.Hour), now.Add(168*time.Hour)
	fiveSeries := weeklySeries(now, 4*time.Hour, []float64{0, 5, 10}, fiveReset, usage.WindowFiveHour)

	in := []Input{
		// Two seats the run had: eligible, readable, on both axes.
		{UUID: "uuid-a", Idx: 1, Eligible: true, Snapshot: &usage.Snapshot{
			FiveHour: usage.NewWindow(pf(10), pt(fiveReset)),
			SevenDay: usage.NewWindow(pf(10), pt(weeklyReset)),
		}, Series: fiveSeries},
		{UUID: "uuid-b", Idx: 2, Eligible: true, Snapshot: &usage.Snapshot{
			FiveHour: usage.NewWindow(pf(10), pt(fiveReset)),
			SevenDay: usage.NewWindow(pf(10), pt(weeklyReset)),
		}, Series: fiveSeries},
		// A third, carrying five_hour and no weekly window at all. It is a seat
		// the run had, and it is in neither PointsTotal nor the weekly
		// replenishment denominator.
		{UUID: "uuid-five", Idx: 3, Eligible: true, Snapshot: &usage.Snapshot{
			FiveHour: usage.NewWindow(pf(10), pt(fiveReset)),
		}, Series: fiveSeries},
		// No snapshot: readable of nothing. Never chosen as live, so counting
		// it would promise capacity nobody can see.
		{UUID: "uuid-unreadable", Idx: 4, Eligible: true},
		// Perfectly readable and the rotation cannot switch to it, which is
		// unreachable by a different route and by the same amount.
		{UUID: "uuid-ineligible", Idx: 5, Snapshot: &usage.Snapshot{
			FiveHour: usage.NewWindow(pf(10), pt(fiveReset)),
			SevenDay: usage.NewWindow(pf(10), pt(weeklyReset)),
		}, Series: fiveSeries},
	}
	f := Of(in, now)

	if f.AccountsUsable != 3 {
		t.Errorf("AccountsUsable = %d, want 3 of %d inputs -- one is ineligible and one is readable of nothing, and neither is a seat the run had", f.AccountsUsable, len(in))
	}
	if f.Basis.Accounts != 5 || f.Basis.Unreadable != 1 || f.Basis.Ineligible != 1 {
		t.Fatalf("Basis = %d accounts, %d unreadable, %d ineligible; want 5, 1, 1", f.Basis.Accounts, f.Basis.Unreadable, f.Basis.Ineligible)
	}
	if got := f.Basis.Accounts - f.Basis.Unreadable - f.Basis.Ineligible; f.AccountsUsable != got {
		t.Errorf("AccountsUsable = %d and the basis subtracts to %d; the two are the cross-check a consumer is told to make", f.AccountsUsable, got)
	}
	// The two figures a reader must NOT check it against, pinned so that the
	// documented non-identity stays a measured fact rather than a claim.
	if f.PointsTotal/100 == float64(f.AccountsUsable) {
		t.Errorf("PointsTotal is %v over %d usable accounts; this fixture exists because an account with no weekly window is in one count and not the other", f.PointsTotal, f.AccountsUsable)
	}
	if f.Weekly.Replenish == replenish(f.AccountsUsable, usage.WindowSevenDay) {
		t.Errorf("the weekly replenishment is measured over %d accounts, the same as AccountsUsable; the fixture's five-hour-only account is meant to separate them", f.AccountsUsable)
	}
}

// The search is bounded in WORK and not only in its answer, and that is a
// separate promise from maxNeeded's. One question here is a whole run of the
// rotation over the candidate fleet, and a run costs time that grows faster than
// the fleet it is handed -- so a walk that added one seat per question would
// spend the ceiling's worth of the most expensive runs there are. Every command
// that prints a fleet measures one of these synchronously and the dashboard
// remeasures on a timer, so the cost is paid in front of a person.
//
// Both halves are swept across every threshold the predicate could have, because
// a bound that holds on one fixture is a bound nobody has measured. The answer is
// asserted alongside the count of questions: a search that got cheap by
// answering a different question is not the trade being made here.
func TestTheSearchAsksALogarithmicNumberOfQuestions(t *testing.T) {
	// Twice the base-two logarithm of the ceiling: the doubling ladder takes at
	// most log2 rungs to bracket an answer and the halving takes at most log2
	// more to find it inside the bracket. 2 x log2(256) = 16.
	const ceiling, budget = 256, 16

	t.Run("climbing", func(t *testing.T) {
		for want := 2; want <= ceiling; want++ {
			calls := 0
			// A fleet of one that does not hold, and a threshold that walks
			// across the whole range: supply rises with the count, so the
			// predicate is false below the threshold and true from it up.
			got, capped := climb(1, ceiling, func(n int) bool { calls++; return n >= want })
			if got != want || capped {
				t.Fatalf("climb found %d (capped %v), want %d -- the ladder has to land on the smallest count that holds, not merely on one that does", got, capped, want)
			}
			if calls > budget {
				t.Fatalf("climb asked the run %d questions to find %d; the budget is %d, and a walk of one seat per question would ask %d", calls, want, budget, want-1)
			}
		}
	})

	t.Run("descending", func(t *testing.T) {
		for want := 1; want <= ceiling; want++ {
			calls := 0
			got := descend(ceiling, func(n int) bool { calls++; return n >= want })
			if got != want {
				t.Fatalf("descend found %d, want %d -- the smallest count that still holds is what the spare figure is read off", got, want)
			}
			if calls > budget {
				t.Fatalf("descend asked the run %d questions to find %d; the budget is %d", calls, want, budget)
			}
		}
	})

	// A fleet nothing inside the ceiling can carry is reported as the ceiling
	// and flagged, and it costs a ladder rather than a walk to find that out.
	calls := 0
	got, capped := climb(1, ceiling, func(int) bool { calls++; return false })
	if got != ceiling || !capped {
		t.Errorf("climb over a predicate nothing satisfies returned %d (capped %v), want %d and true", got, capped, ceiling)
	}
	if calls > budget {
		t.Errorf("climb asked %d questions to reach the ceiling; the budget is %d", calls, budget)
	}
}

// Counting downward drops the fleet's ROOMIEST seats and keeps its tightest, and
// that ordering is a decision about money rather than an implementation detail.
// The count it produces is the count a reader stops paying for, and the seats
// that look spare on any one reading are the ones whose reset this build could
// not read -- a field that was missing from one response, not an account that is
// permanently gone. Keeping the tightest answers "how few accounts hold at this
// rate whichever ones I keep"; keeping the roomiest would answer "how few hold if
// the ones I give up are exactly the spent ones", which is advice nobody can act
// on and which invites cancelling a subscription on the strength of a field
// nobody read.
//
// This is asserted on its own because the mechanism test above cannot see it: its
// fleet is short, so it is answered by appending seats and never by dropping
// them. Without this fixture the drop order is unpinned in both directions.
//
// Three accounts at 30 points an hour. Two are readable and empty; the third's
// five-hour window reads 100 with no resets_at, so nothing inside the horizon
// brings it back. Two live seats replenish 40 and carry the rate; one replenishes
// 20 and does not. Keeping the roomiest two would report that this fleet holds on
// two of its three and has one to spare.
func TestTheDownwardSearchKeepsTheFleetsTightestSeats(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	fiveReset := now.Add(5 * time.Hour)
	// 39 points over the four-hour span on each of three accounts: 117 filled is
	// 29.25 an hour, and three contributors put the upper bound at (117+3)/4 = 30.
	series := weeklySeries(now, 4*time.Hour, []float64{0, 19.5, 39}, fiveReset, usage.WindowFiveHour)
	in := make([]Input, 0, 3)
	for i := range 3 {
		snap := &usage.Snapshot{FiveHour: usage.NewWindow(pf(0), pt(fiveReset))}
		if i == 2 {
			// Spent, with a resets_at this build could not read.
			snap = &usage.Snapshot{FiveHour: usage.NewWindow(pf(100), nil)}
		}
		in = append(in, Input{UUID: fmt.Sprintf("uuid-%d", i), Idx: i + 1, Eligible: true, Snapshot: snap, Series: series})
	}
	f := Of(in, now)

	if f.FiveHour.Burn.High != 30 {
		t.Fatalf("five-hour burn band = %v/%v, want an upper bound of 30", f.FiveHour.Burn.Low, f.FiveHour.Burn.High)
	}
	if f.Both.Verdict != VerdictHolds {
		t.Fatalf("Both.Verdict = %v, want VerdictHolds -- a fleet that does not hold is answered by appending seats and never by dropping them", f.Both.Verdict)
	}
	if f.AccountsUsable != 3 {
		t.Fatalf("AccountsUsable = %d, want 3", f.AccountsUsable)
	}
	if f.AccountsNeeded != 3 {
		t.Fatalf("AccountsNeeded = %d of %d usable, want 3 and no spare; dropping the tightest seat first reports one to spare, which tells an owner to stop paying for an account whose reset was merely unreadable", f.AccountsNeeded, f.AccountsUsable)
	}
}

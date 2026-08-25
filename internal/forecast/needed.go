package forecast

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// maxNeeded bounds the search for how many accounts a fleet needs.
//
// It is not a termination guard. Replenishment is linear in the number of
// accounts and burn is not a function of the fleet -- adding an account does not
// make anyone type more -- so supply scales, demand does not, and a count that
// holds always exists.
//
// It is a sanity ceiling. At 256 accounts the weekly axis replenishes
// 256 x 100/168 h = 152 percentage points an hour, roughly twenty-five
// account-weeks a day. A fleet that appears to need more than that has a
// measurement problem rather than a purchasing one, and reporting the bound and
// saying it is a bound is more use than a number nobody can act on.
//
// The work the search does is bounded separately, by climb rather than by this
// figure, and it has to be: one run costs time that grows faster than the fleet
// it is handed. Measured on this tree over accounts carrying five_hour, the four
// weekly names and three weekly_scoped caps -- eight windows each -- one run
// takes 100 us at 6 accounts, 5.1 ms at 64 and 236 ms at 256. Asking one
// question per candidate would therefore put minutes between a user and the
// screen, which is why climb asks a logarithmic number of them instead. An
// ordinary six-account fleet of that shape answers in 6 ms end to end; the worst
// case measured -- every window burned whole inside the shortest span a
// measurement can be made over -- is 1.5 s.
//
// That worst case is bounded by minCover and NOT by the four-hour measured span.
// A rate is consumption divided by the span actually OBSERVED, so six accounts
// each burning a whole weekly window inside a 20-minute observation measure
// (600 + 6) points over a third of an hour = 1818 points an hour, twelve times
// the 151.5 the same burn spread over four hours would report.
const maxNeeded = 256

// need is the search's answer, in the shape Fleet publishes it.
type need struct {
	count  int
	has    bool
	by     usage.WindowName
	hasBy  bool
	capped bool
}

// needFor is how many accounts this fleet would have to hold for both axes to
// survive the horizon at the measured rate.
//
// The answer comes from the SIMULATION -- the same runs the verdicts come from,
// asked a third question -- and never from burn divided by replenishment. That
// closed form is the comparison a verdict is forbidden to be built on, and a
// fleet carrying both mechanisms would be free to print "runs dry" and "you have
// enough accounts" on adjacent lines. The two really do disagree: an account out
// on the other axis, an account nobody can read, and a window with a level and
// no reported reset all contribute to the comparison's supply and supply nothing
// to the rotation.
//
// It runs at the upper end of the measured band, for the same reason "holds"
// does: the figure has to be the one that is PROVABLY enough. A fleet told it
// needs nine accounts and given nine must not then run dry because the
// measurement was coarse.
//
// basis false answers nothing rather than zero. A run over an unmeasured fleet
// reaches the horizon without spending a thing, so a search over one would
// report that a single account is plenty -- a promise resting on no reading.
func needFor(sim []simAccount, rates map[usage.WindowName]float64, five, weekly, both []usage.WindowName, now time.Time, basis bool) need {
	if !basis || len(sim) == 0 {
		return need{}
	}
	seats := tightestFirst(sim)
	fresh := freshWindows(sim)
	nextIdx := 1
	for i := range sim {
		if sim[i].idx >= nextIdx {
			nextIdx = sim[i].idx + 1
		}
	}
	holds := func(n int, scope []usage.WindowName) bool {
		_, dry, _ := simulateScoped(fleetOf(seats, fresh, nextIdx, n), rates, scope, now)
		return !dry
	}

	// The ceiling is never below the fleet's own size. A fleet larger than the
	// bound that still does not hold is past the point the bound was chosen to
	// call out, and answering with a count smaller than the one the fleet
	// already has would read as a fleet that needs to shrink.
	ceiling := max(maxNeeded, len(seats))

	holdsBoth := func(n int) bool { return holds(n, both) }

	out := need{has: true}
	if holdsBoth(len(seats)) {
		// The same search downward, so a fleet that holds learns how much slack
		// it has and not merely that it has some.
		out.count = descend(len(seats), holdsBoth)
	} else {
		// Upward, to the first count that survives the horizon. Reaching the
		// ceiling without one is reported as the ceiling and flagged, so nothing
		// downstream can read the bound as a count somebody could go and buy.
		out.count, out.capped = climb(len(seats), ceiling, holdsBoth)
	}

	// The binding axis is read off the largest count that did NOT hold -- the
	// one below the answer, or the ceiling itself when the search reached it.
	// An axis that was still failing there is the axis that asked for the seat.
	//
	// Both failing, or neither, leaves the figure unattributed on purpose: it
	// came from the two axes together, and naming one of them would report a
	// fact the run did not produce. Below one account there is no such count at
	// all, because a fleet of nobody says nothing about either axis.
	probe := out.count - 1
	if out.capped {
		probe = out.count
	}
	if probe >= 1 {
		fiveHolds, weeklyHolds := holds(probe, five), holds(probe, weekly)
		switch {
		case !fiveHolds && weeklyHolds:
			out.by, out.hasBy = usage.WindowFiveHour, true
		case fiveHolds && !weeklyHolds:
			// The axis's representative window name, the one its replenishment
			// figure is measured on. The weekly axis is a SET of windows that
			// all meter the same consumption, so no one of them is the axis --
			// this names it the way the rest of the package does.
			out.by, out.hasBy = usage.WindowSevenDay, true
		}
	}
	return out
}

// climb is the smallest count above `from` whose predicate holds, and whether
// the ceiling was reached without one.
//
// The candidates are walked in a DOUBLING ladder and the interval that ladder
// lands in is then halved, rather than one seat being added at a time. The bound
// that buys is on the WORK and not merely on the answer, which is the reason it
// is here: one predicate call is a whole run of the rotation, and every command
// that prints a fleet measures one synchronously while the dashboard remeasures
// on a timer. A walk of one seat per question puts up to a ceiling's worth of
// runs between a user and their screen; a ladder and a halving ask at most twice
// the base-two logarithm of the ceiling, which is 16 questions at 256.
//
// What comes back is always a count the predicate was asked about and answered
// yes to. Supply is linear in the count and burn is not a function of the fleet,
// so a count that holds holds at every larger count and the halving lands on the
// smallest one; if that ever failed the halving would land ABOVE the smallest
// rather than below it, asking for a seat too many rather than promising on one
// too few.
func climb(from, ceiling int, holds func(int) bool) (int, bool) {
	lo := from
	for step := 1; lo < ceiling; step *= 2 {
		hi := min(lo+step, ceiling)
		if holds(hi) {
			return halve(lo, hi, holds), false
		}
		lo = hi
	}
	return ceiling, true
}

// descend is the smallest count in [1, from] whose predicate holds, given that
// `from`'s does.
//
// It stops at one rather than zero: a fleet of no accounts runs dry because
// there is nobody to be live, which is an artefact of having nobody rather than
// anything the rate did.
func descend(from int, holds func(int) bool) int {
	if from <= 1 || holds(1) {
		return 1
	}
	return halve(1, from, holds)
}

// halve is the smallest count in (lo, hi] whose predicate holds, given that
// hi's does and lo's does not. The two ends keep those two answers all the way
// down, so the count returned is one the predicate answered yes to.
func halve(lo, hi int, holds func(int) bool) int {
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		if holds(mid) {
			hi = mid
		} else {
			lo = mid
		}
	}
	return hi
}

// tightestFirst is the fleet ordered by least room, ties by ascending index.
//
// The order is what the search drops from when it counts downward, and dropping
// the roomiest first is the fail-closed direction: the seats it keeps are the
// fleet's tightest, so the count it reports is one the fleet can meet without
// cancelling exactly the right accounts. Keeping the roomiest instead would
// report spare capacity that only exists if the accounts given up are the spent
// ones -- and the count a reader acts on is the count of accounts they stop
// paying for.
//
// The objection to answer is that this keeps seats the run itself never makes
// live -- an account at 100 on a window whose reading carried no resets_at is
// out for the whole horizon and supplies nothing -- while dropping seats that
// carry the fleet. That is true and it is the point. Such an account is a
// resets_at this build could not read, which simWindow's own doc says is a
// missing field rather than a dead account: the seat comes back, and the reading
// that would prove it did not arrive. So the two orders answer two different
// questions. This one answers "how few accounts hold at this rate whichever ones
// I keep". The other answers "how few hold if the ones I give up are exactly the
// ones that happened to read as spent this minute", which is a subscription
// cancelled on the strength of a field nobody read.
//
// Counting UP is not the mirror of this and does not have the same problem: it
// appends seats rather than choosing among the fleet's own, and a seat somebody
// buys really does arrive fresh.
func tightestFirst(sim []simAccount) []simAccount {
	out := slices.Clone(sim)
	slices.SortStableFunc(out, func(a, b simAccount) int {
		if r := cmp.Compare(a.minRoom(), b.minRoom()); r != 0 {
			return r
		}
		return cmp.Compare(a.idx, b.idx)
	})
	return out
}

// freshWindows is the window set a hypothetical account arrives with: every
// window name the fleet's own accounts carry, with the length each of them runs
// for.
//
// The union, because inventing a window nobody has or omitting one everybody
// has answers a question about a different fleet. A scoped weekly cap is the
// case that costs money: a new seat arrives with the cap the others have, and
// the run charges the live account only the windows that account carries -- so a
// hypothetical account without the cap would absorb the fleet's scoped burn for
// free and report a fleet holding on seats that cannot carry it.
func freshWindows(sim []simAccount) map[usage.WindowName]time.Duration {
	out := make(map[usage.WindowName]time.Duration)
	for i := range sim {
		for n, w := range sim[i].windows {
			out[n] = w.length
		}
	}
	return out
}

// fleetOf is the fleet at one candidate count: the tightest n of the fleet's own
// accounts, or all of them with hypothetical accounts appended.
//
// A hypothetical account is at zero with NO reset, which is what a fresh account
// is: the endpoint reports no resets_at for a window nothing has spent, and the
// run starts such a window's cycle at the first charge that lands on it. Giving
// one a reset here instead would hand it a rollover before anything had used it.
//
// The new indices sit above every real one, so a hypothetical seat is chosen
// last among equals and the order the run picks the fleet's own accounts in does
// not change as seats are added.
func fleetOf(seats []simAccount, fresh map[usage.WindowName]time.Duration, nextIdx, n int) []simAccount {
	if n <= len(seats) {
		return seats[:n]
	}
	out := make([]simAccount, len(seats), n)
	copy(out, seats)
	for i := len(seats); i < n; i++ {
		windows := make(map[usage.WindowName]simWindow, len(fresh))
		for name, length := range fresh {
			windows[name] = simWindow{length: length}
		}
		out = append(out, simAccount{
			uuid:    fmt.Sprintf("hypothetical-%d", i),
			idx:     nextIdx + i,
			windows: windows,
		})
	}
	return out
}

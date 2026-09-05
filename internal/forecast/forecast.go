package forecast

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Kweiza/ccdaddy/internal/history"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// Verdict is what a measured rate implies for one axis.
//
// VerdictUnknown is the zero value on purpose, so a value that was never
// decided reads as undecided rather than as a promise. It covers two states
// that must never be rendered as "holds": there is no basis to decide from, and
// the two runs of the measured band disagreed. Both mean the evidence does not
// carry a claim; neither means the fleet is safe.
type Verdict uint8

const (
	VerdictUnknown Verdict = iota
	VerdictHolds
	VerdictRunsDry
)

// Basis is what the measurement was made from, so a reader can weigh it. Every
// surface that prints a rate prints this beside it: a four-hour rate is a
// speedometer, and a speedometer read for twenty minutes says less than one
// read for four hours.
type Basis struct {
	// Window is how far back a rate is measured, which is fixed.
	Window time.Duration

	// Observed is the span the readings actually reach across, which is
	// usually shorter -- a daemon started twenty minutes ago has twenty
	// minutes of evidence -- and is what every rate here was divided by.
	Observed time.Duration

	// Readings is how many samples fell inside the window across the fleet.
	Readings int

	// Accounts is the fleet size, before any of the exclusions below.
	Accounts int

	// Unmeasured is readable accounts that cleared no contribution gate, and
	// they are absent from every sum rather than counted as burning nothing.
	Unmeasured int

	// Unreadable is accounts whose current reading carried no window at all.
	// They are counted here and NOWHERE else: an unreadable account is
	// excluded because nobody can see it, not because it is spent, and an
	// account is counted under one of these two headings or neither, so a
	// reader can add them against Accounts.
	Unreadable int

	// Ineligible is accounts the rotation cannot hand work to -- see
	// Input.Eligible. They are excluded before anything is measured, so they
	// appear here and in no other figure on this page, and Accounts less this
	// and the two headings above it is what was actually measured.
	Ineligible int

	// Known is false when nothing was measured at all. It is not a rate of
	// zero: a fleet nobody has enough readings for is not a fleet burning
	// nothing.
	Known bool
}

// Axis is one axis's answer: how fast it is being spent, how fast it comes
// back, and whether that holds.
type Axis struct {
	// Burn is the fleet's measured consumption of this axis.
	Burn Band

	// Replenish is what the axis gives back, in percentage points per hour:
	// one window's worth per readable account per window length.
	//
	// It is an EXPLANATION of the verdict and never its source. It counts an
	// account that is out on the other axis, which supplies nothing, and a
	// window with no reported reset, which supplies its current room once and
	// never again; the run below counts neither. Presenting it as the decision
	// would promise capacity the fleet cannot reach.
	Replenish float64

	// Verdict is the answer, and it comes from the run and from nothing else.
	Verdict Verdict

	// DryAt is when the run's own burn left no account able to take work, and
	// HasDryAt is whether there is such a moment. They are meaningful only
	// when Verdict is VerdictRunsDry.
	DryAt    time.Time
	HasDryAt bool
}

// AccountRow is one account's line in the per-account block: which window binds
// it, how much of that window is left, how fast this account itself spent it,
// and when the run first found the account out.
type AccountRow struct {
	UUID string

	// Window is the window Left and Burn describe -- the account's least-room
	// weekly one, which is the window the fleet's points are counted on.
	//
	// HasWindow is whether the account reported a weekly window at all. When
	// it is false, Window, Left and Burn carry NO QUANTITY -- not an unknown
	// one -- and the account is out of PointsLeft and PointsTotal. It still
	// gets a row, because it is still in the fleet and the run still has
	// something to say about when it goes out; what it has no weekly figure
	// for is this column. Naming its five-hour window here instead would put a
	// five-hour room and a five-hour rate in a column that is summed into the
	// weekly axis above it, and those are the two quantities this package adds
	// nowhere else.
	Window    usage.WindowName
	HasWindow bool

	// Left is 100 minus that window's utilization, read from the CURRENT
	// snapshot. The rows with a window sum to Fleet.PointsLeft, which is what
	// lets a reader check one figure against the other.
	Left float64

	// Burn is this account's own consumption over the SHARED span the axis
	// figure was divided by, so the column sums to the axis figure above it
	// rather than to something larger. A measured zero is a reading -- the
	// account was up, was polled and did not burn -- and Known false is not
	// one.
	Burn Band

	// EmptyAt is the moment the RUN first found this account out, and HasEmpty
	// is whether it ever did. It is the run's answer rather than a second
	// arithmetic: a per-account "if it carried the fleet alone" figure would
	// disagree with the run on the same page, because the run burns whichever
	// account is live at the fleet rate and spends the account with the most
	// room first.
	EmptyAt  time.Time
	HasEmpty bool

	// OutNow is whether the account is already out at now, which is a fact
	// about this minute rather than a projection.
	OutNow bool
}

// Fleet is the whole answer.
type Fleet struct {
	Basis Basis

	// FiveHour and Weekly are the two axes, each judged with only its own
	// windows burning and only they ending the run.
	FiveHour Axis
	Weekly   Axis

	// Both is the fleet answer: every window burning at once, which is the
	// question a user is asking when they ask how long the fleet lasts.
	//
	// It carries a Verdict and a moment and NOTHING ELSE. Its Burn is unknown
	// and its Replenish is zero because one percentage point of a five-hour
	// window and one of a weekly window are different quantities: a rate
	// exists only inside an axis, and a figure summing the two would have no
	// unit. A renderer must print this axis's verdict and never its cells.
	Both Axis

	// PointsLeft and PointsTotal are the weekly axis's room, counted ONE
	// window per account -- the least-room weekly one -- over the accounts
	// that were readable. The weekly axis is a set of windows that all meter
	// the same consumption, so summing them would count the same tokens three
	// to five times while the replenishment figure beside them counted one
	// window per account, and the two columns a reader is told to compare
	// would not be on one basis.
	//
	// An account that is already out still counts toward PointsTotal, and
	// contributes its zero to PointsLeft. It is a readable seat of the fleet's
	// capacity that happens to be empty right now, and dropping it would move
	// the denominator every time an account rolled over. An account that
	// reported no weekly window counts toward NEITHER, and that is a different
	// case: there is no weekly quota of it to count.
	PointsLeft  float64
	PointsTotal float64

	// AccountsUsable is how many accounts the run had to work with: the ones
	// that are eligible for the rotation and have at least one readable
	// window. An account that is out right now is one of them -- being spent
	// is a fact about this minute, and its quota comes back.
	//
	// The cross-check a reader has is Basis.Accounts less Basis.Unreadable and
	// Basis.Ineligible, and that subtraction is exact. It is NOT PointsTotal
	// divided by a hundred and NOT the denominator of either Replenish figure,
	// and those three are three different counts: PointsTotal counts only the
	// accounts that reported a WEEKLY window, the weekly Replenish is measured
	// over that same narrower set, and the five-hour Replenish over the
	// accounts carrying five_hour. An account that reported a five-hour window
	// and no weekly one is in this count and in neither of those, so inviting
	// the comparison would hand a reader a subtraction that fails on an
	// ordinary fleet.
	AccountsUsable int

	// AccountsNeeded is the smallest fleet size at which BOTH axes survive the
	// horizon, found by re-running the rotation with hypothetical accounts
	// appended -- one mechanism answering a third question, not a second
	// mechanism free to disagree with the first two on the same page.
	//
	// It counts accounts on the plan the fleet already has, so the tier notice
	// applies to it: on a fleet whose plans disagree, "one more account" is not
	// a unit.
	//
	// HasNeeded is false when there was no basis to search from, and the figure
	// is then ABSENT rather than zero: a fleet that needs no more accounts and
	// a fleet nobody measured must never read the same.
	AccountsNeeded int
	HasNeeded      bool

	// NeededBy names the axis that set the figure -- the one whose own run was
	// still failing at the count below it. It carries that axis's
	// representative window name, five_hour or seven_day, the same two the
	// replenishment figures are measured on.
	//
	// Which axis it is is MEASURED. The two axes imply counts in the ratio of
	// their measured rates against 20 and 100/168 points an hour, and nothing
	// in the design fixes which is larger. HasNeededBy is false when the count
	// below the answer failed on both axes, or on neither -- there the figure
	// came from the two together and naming one of them would invent a fact.
	NeededBy    usage.WindowName
	HasNeededBy bool

	// NeededCapped is true when the search reached its bound without finding a
	// count that holds. AccountsNeeded is then the bound itself and means "more
	// than this": a fleet that appears to need that many accounts has a
	// measurement problem rather than a purchasing one.
	NeededCapped bool

	// Rows is one line per readable account, ordered by EmptyAt ascending and
	// then by the account's index, so the account that goes first reads first.
	// PointsTotal is 100 times the rows with a weekly window, which is not
	// always every row -- see AccountRow.HasWindow.
	Rows []AccountRow

	// Credit is the paid-usage answer, which is one date or nothing.
	Credit CreditFleet

	// TierNotice is empty when every readable account reported the same plan
	// tier. Summing percentage points across accounts assumes their quotas are
	// the same size, and this says so when they may not be.
	TierNotice string
}

// Input is one account as the forecast sees it.
//
// The forecast never opens a store, a cache or a clock: everything arrives
// here. Snapshot is the CURRENT reading and is the only source of a level;
// Series is the only source of a slope. Keeping the two apart is what stops two
// commands disagreeing about one number, because the store gives each quantity
// exactly one authoritative file and history.json is not the one for levels --
// its newest sample merely duplicates the cache's.
type Input struct {
	UUID string

	// Idx is the account's display index in the store, and it is the FIRST of
	// the two things that break ties: two accounts with identical room must be
	// picked in the same order on every run, or six runs of one fleet disagree
	// about a fleet that never changed.
	//
	// It does not break them on its own, because it is numbered per provider --
	// a Claude seat and a Codex one carry the same number as a matter of course.
	// UUID above finishes the order; see simAccount.before.
	Idx int

	// Tier is rate_limit_tier, and "" is a real state -- an account added
	// while the profile endpoint was unreachable carries it and is still
	// polled and ranked. It is never counted as agreement.
	Tier string

	// Snapshot is the current reading. A nil one is an account nobody can see.
	Snapshot *usage.Snapshot

	// Series is this account's samples, oldest first, already filtered by the
	// read side.
	Series []history.Sample

	// Eligible is whether the ROTATION can hand this account work. It is the
	// caller's copy of the engine's own eligibility rule, which is stated once
	// in eligible() in internal/strategy/rank.go; this package must not import
	// a ranking, so the answer arrives as a field.
	//
	// An ineligible account's quota is quota the fleet cannot reach: a run
	// that made it live would spend capacity no switch can get to, and would
	// report a runway several times longer than the fleet has. That is the
	// same fail-open simAccount.readable refuses -- an account nobody can see
	// and an account nothing can switch to are unreachable for different
	// reasons and by the same amount.
	//
	// The zero value EXCLUDES, which is deliberate: a caller that forgets this
	// field reports an empty fleet, loudly, rather than an over-long runway
	// that reads like an answer.
	Eligible bool
}

// account is one Input with everything derived from it that more than one part
// of Of needs.
type account struct {
	in Input

	// order is the account's in-scope windows in a fixed order, so a tie
	// between two equally tight windows is broken the same way on every call.
	// Ranging a map here would let the reported window move between two runs
	// over one unchanged fleet.
	order []usage.NamedWindow

	// sim is the same set as the simulation models it.
	sim map[usage.WindowName]simWindow

	// bind is the window the account's row and its points are counted on, and
	// hasBind is whether there was one.
	bind    usage.WindowName
	hasBind bool
}

// Of measures the fleet and decides what the measurement implies.
//
// The order is fixed by what depends on what: the current snapshots give every
// level and the window set, the series give a rate per WINDOW NAME, the two
// axes and the fleet are each run twice over that band, and the rows are read
// out of the fleet run rather than computed a second way.
func Of(in []Input, now time.Time) Fleet {
	from := now.Add(-history.MeasuredSpan)

	accounts := make([]account, 0, len(in))
	reachable := make([]Input, 0, len(in))
	var unreadable, ineligible int
	for _, a := range in {
		// Eligibility is asked FIRST, and an ineligible account is counted as
		// ineligible rather than as unreadable even when it is both. It is not
		// part of the fleet being measured at all: its readings are not
		// evidence about a rotation that cannot reach it, and its quota is not
		// capacity that rotation has.
		if !a.Eligible {
			ineligible++
			continue
		}
		reachable = append(reachable, a)
		acc := accountOf(a)
		if len(acc.order) == 0 {
			unreadable++
			continue
		}
		accounts = append(accounts, acc)
	}

	rates, basisSpan, measured := measureFleet(accounts, from, now)

	f := Fleet{
		Basis: Basis{
			Window:     history.MeasuredSpan,
			Observed:   basisSpan,
			Readings:   readingsIn(reachable, from, now),
			Accounts:   len(in),
			Unmeasured: len(accounts) - measured,
			Unreadable: unreadable,
			Ineligible: ineligible,
			Known:      measured > 0,
		},
		TierNotice: tierNotice(accounts),
	}

	// The reporting figure for the weekly axis is one window per account, and
	// it is the same window the account's row is counted on, so the row and
	// the axis above it can never describe two different quotas.
	bindFilled, bindSpan, bindCount := bindBasis(accounts, from, now)
	f.Weekly.Burn = fleetBand(bindFilled, bindCount, bindSpan)
	f.FiveHour.Burn = rates.band(usage.WindowFiveHour)

	sim := make([]simAccount, 0, len(accounts))
	for _, a := range accounts {
		sim = append(sim, simAccount{uuid: a.in.UUID, idx: a.in.Idx, windows: a.sim})
	}
	f.FiveHour.Replenish = replenish(carrying(accounts, isFiveHour), usage.WindowFiveHour)
	f.Weekly.Replenish = replenish(carrying(accounts, usage.IsWeekly), usage.WindowSevenDay)

	// Scope decides what burns and what ends a run; usability stays fleet-wide
	// inside the run, because an account whose weekly window is spent cannot be
	// switched to on the five-hour axis either.
	var five, weekly, both []usage.WindowName
	for _, n := range rates.names {
		if n == usage.WindowFiveHour {
			five = append(five, n)
		}
		if usage.IsWeekly(n) {
			weekly = append(weekly, n)
		}
		both = append(both, n)
	}

	// Each axis is gated on evidence about ITS OWN windows, and deliberately
	// not on the Burn figure printed beside it. Those two are the same fact on
	// an ordinary fleet and they are not the same statement: Burn is measured
	// over one chosen window per account, so tying the verdict to it would let
	// a change in how that window is chosen decide whether an axis may speak.
	// That is exactly how a weekly row once came to say "holds" for a fleet
	// whose weekly windows had never been read.
	lowRates, highRates := rates.low(), rates.high()
	f.FiveHour = decide(f.FiveHour, sim, lowRates, highRates, five, now, anyKnown(rates, five))
	f.Weekly = decide(f.Weekly, sim, lowRates, highRates, weekly, now, anyKnown(rates, weekly))
	f.Both = decide(f.Both, sim, lowRates, highRates, both, now, f.Basis.Known)

	// How many accounts it would take is the same runs asked once more, with
	// seats the fleet does not have yet appended. It is answered here, beside
	// the verdicts, because it has to be the same mechanism: a count from
	// anywhere else would be free to disagree with the verdict printed above
	// it.
	f.AccountsUsable = len(sim)
	needed := needFor(sim, highRates, five, weekly, both, now, f.Basis.Known)
	f.AccountsNeeded, f.HasNeeded = needed.count, needed.has
	f.NeededBy, f.HasNeededBy = needed.by, needed.hasBy
	f.NeededCapped = needed.capped

	// The rows come out of the fleet run at the low end of the band. That run
	// is the one whose dry moment is reported when both ends agree, so the last
	// row's moment and the fleet's own are the same figure rather than two.
	_, _, emptyAt := simulateScoped(sim, lowRates, both, now)
	f.Rows = rowsOf(accounts, emptyAt, bindSpan, from, now)
	for _, r := range f.Rows {
		// Rows without a weekly window are counted in NEITHER figure, which is
		// why the total is accumulated here rather than taken from len(Rows):
		// an account with no weekly quota to report has none to be part of the
		// denominator either, and counting it would put 100 points into the
		// fleet's capacity that no weekly window backs.
		if !r.HasWindow {
			continue
		}
		f.PointsLeft += r.Left
		f.PointsTotal += 100
	}

	// reachable, not accounts: the slice above it has already had every account
	// with no SIMULATABLE PLAN WINDOW dropped from it, and that filter has
	// nothing to do with whether a balance can be measured. Feeding it here made
	// the credit runway structurally unreachable for a seat metered only in
	// money -- adding one irrelevant plan window to the same account was enough
	// to make the figure appear.
	f.Credit = creditFleet(creditFleetInputs(reachable, from, now))
	return f
}

// accountOf reads one account's current snapshot into the window set every
// later step works from.
//
// The set is RateLimitWindows plus the named scoped windows and nothing else.
// cinder_cove is absent because RateLimitWindows leaves it out -- its resets_at
// is an expiry rather than a rollover -- and the unnamed-scope windows are left
// out because the engine itself ranks them only on an explicit opt-in
// threshold, and no threshold reaches this package. The forecast models the
// machine that exists.
//
// A window this release knows no length for is dropped as well, so the run
// cannot hold an account out of service over a window it can never roll back.
func accountOf(in Input) account {
	a := account{in: in}
	if in.Snapshot == nil {
		return a
	}
	candidates := in.Snapshot.RateLimitWindows()
	for _, w := range in.Snapshot.ScopedWindows() {
		candidates = append(candidates, w.NamedWindow)
	}
	a.sim = make(map[usage.WindowName]simWindow, len(candidates))
	for _, w := range candidates {
		pct, ok := w.Percent()
		if !ok {
			continue
		}
		length, known := usage.WindowLength(w.Name)
		if !known {
			continue
		}
		sw := simWindow{pct: pct, length: length}
		if reset, ok := w.Reset(); ok {
			sw.reset = reset
		}
		a.order = append(a.order, w)
		a.sim[w.Name] = sw
	}
	a.bind, a.hasBind = bindingWindow(a)
	return a
}

// bindingWindow is the window an account's row and its share of the fleet's
// points are counted on: the least-room WEEKLY one.
//
// Weekly, because that is the axis the points are counted on and because the
// five-hour window comes back every five hours -- a fleet's capacity is not
// measured by a quota that refills before lunch.
//
// Room, not slack. strategy.Headroom carries both, and its Pct comes off the
// window with the least SLACK, which is threshold-derived; hover rewrites
// thresholds every tick, and a forecast that moved because a ranking moved
// would report a change in the fleet that never happened. MinPct's own doc in
// internal/strategy/headroom.go works the ordinary pair through.
//
// An account whose response carried no weekly window at all has NO binding
// window, and the second return says so. It is the account whose weekly windows
// dropped out of one response while its five-hour one came back, and falling
// back to that five-hour window is what this function used to do: it put a
// five-hour room into the fleet's weekly points and a five-hour rate into the
// weekly burn column. The two are not interchangeable at any scale -- the
// windows run over 168 hours and five, a factor of 33.6 -- so one window's
// percentage point is not the other's. The account keeps its row: rowsOf marks
// the three cells as quantities that do not exist here, so nothing disappears
// from the table except a figure that was measured on the wrong window.
func bindingWindow(a account) (usage.WindowName, bool) {
	best, found := usage.WindowName(""), false
	var bestRoom float64
	for _, w := range a.order {
		if !usage.IsWeekly(w.Name) {
			continue
		}
		room := 100 - a.sim[w.Name].pct
		if found && room >= bestRoom {
			continue
		}
		best, bestRoom, found = w.Name, room, true
	}
	return best, found
}

// fleetRates is every window name's measured band, keyed by name, plus the
// order the names were first seen in so two runs over one fleet scope their
// windows identically.
type fleetRates struct {
	names []usage.WindowName
	bands map[usage.WindowName]Band
}

func (r fleetRates) band(n usage.WindowName) Band { return r.bands[n] }

// anyKnown is whether a run over this scope has any measured rate to run at.
//
// It is what tells "the axis holds" apart from "nothing about this axis was
// ever read". A window with no rate contributes no event, so a run whose whole
// scope is unmeasured reaches the horizon without spending a thing and reports
// that the axis holds -- a promise resting on no reading. An empty scope is the
// same statement in the extreme: no window of that axis was reported by anyone.
func anyKnown(r fleetRates, scope []usage.WindowName) bool {
	for _, n := range scope {
		if r.band(n).Known {
			return true
		}
	}
	return false
}

// low and high are the two rate maps the band's ends are run at. A name with no
// measured rate is ABSENT from both rather than present as a zero: the run
// gives a window with no rate no event at all, which is the only honest thing
// to do with a window nobody could measure, and a zero entry would read the
// same while claiming to be a measurement.
func (r fleetRates) low() map[usage.WindowName]float64 { return r.pick(false) }

func (r fleetRates) high() map[usage.WindowName]float64 { return r.pick(true) }

func (r fleetRates) pick(high bool) map[usage.WindowName]float64 {
	out := make(map[usage.WindowName]float64, len(r.bands))
	for n, b := range r.bands {
		if !b.Known {
			continue
		}
		if high {
			out[n] = b.High
		} else {
			out[n] = b.Low
		}
	}
	return out
}

// measureFleet measures one band per window name and reports the span the whole
// measurement reached across, plus how many accounts cleared a gate anywhere.
//
// Consumption is summed and time is SHARED, per window name: one sum of points
// over one span. Summing per-account rates would project one account's
// forty-minute burst as though the whole fleet had sustained it for four hours.
func measureFleet(accounts []account, from, now time.Time) (fleetRates, time.Duration, int) {
	r := fleetRates{bands: map[usage.WindowName]Band{}}
	filled := map[usage.WindowName]float64{}
	count := map[usage.WindowName]int{}
	spans := map[usage.WindowName]*spanAcc{}
	var whole spanAcc
	contributed := make(map[string]bool, len(accounts))

	for _, a := range accounts {
		for _, w := range a.order {
			if _, seen := r.bands[w.Name]; !seen {
				r.names = append(r.names, w.Name)
				r.bands[w.Name] = Band{}
				spans[w.Name] = &spanAcc{}
			}
			f, _, _, ok := windowRate(a.in.Series, w.Name, from, now)
			if !ok {
				continue
			}
			first, last, ok := spanOf(a.in.Series, from, now, carriesWindow(w.Name))
			if !ok {
				continue
			}
			filled[w.Name] += f
			count[w.Name]++
			spans[w.Name].add(first, last)
			whole.add(first, last)
			contributed[a.in.UUID] = true
		}
	}
	for _, n := range r.names {
		r.bands[n] = fleetBand(filled[n], count[n], spans[n].span())
	}
	return r, whole.span(), len(contributed)
}

// bindBasis measures the weekly axis the way it is REPORTED: over each
// account's one binding window, so the per-account column sums to the axis
// figure above it and the points beside it are counted on the same windows.
func bindBasis(accounts []account, from, now time.Time) (filled float64, span time.Duration, contributors int) {
	var acc spanAcc
	for _, a := range accounts {
		if !a.hasBind {
			continue
		}
		f, _, _, ok := windowRate(a.in.Series, a.bind, from, now)
		if !ok {
			continue
		}
		first, last, ok := spanOf(a.in.Series, from, now, carriesWindow(a.bind))
		if !ok {
			continue
		}
		filled += f
		contributors++
		acc.add(first, last)
	}
	return filled, acc.span(), contributors
}

// replenish is what an axis gives back: one window's worth per account that
// carries a window of that axis, per window length.
//
// It counts accounts the run will find unusable, and that divergence is the
// point of keeping this figure out of the verdict: an account out on the other
// axis contributes here and supplies nothing to the rotation.
//
// It does NOT count an account that reported no window of this axis at all,
// which is a different case from an account that is out: a window nobody
// reported cannot roll over, and crediting the axis with a hundred points a
// week for it would explain a verdict with capacity that does not exist.
func replenish(accounts int, of usage.WindowName) float64 {
	length, known := usage.WindowLength(of)
	if !known || length <= 0 {
		return 0
	}
	return float64(accounts) * 100 / length.Hours()
}

// carrying is how many accounts reported at least one window the axis is made
// of, which is the count that axis's replenishment is measured over.
func carrying(accounts []account, inAxis func(usage.WindowName) bool) int {
	n := 0
	for _, a := range accounts {
		for _, w := range a.order {
			if inAxis(w.Name) {
				n++
				break
			}
		}
	}
	return n
}

// isFiveHour is the five-hour axis's membership test, written as a function so
// it reads beside usage.IsWeekly at the one place both are used. The axis is
// one window name; there is no set of them.
func isFiveHour(n usage.WindowName) bool { return n == usage.WindowFiveHour }

// decide runs one axis at both ends of the measured band and reads the verdict
// off the pair.
//
// "Holds" is a claim the UPPER bound had to survive, which is the whole defence
// against a thin basis: a coarse measurement produces a wide band, a wide band
// straddles the boundary, and a straddled boundary decides nothing. Deciding on
// the low end alone would promise safety the evidence does not carry.
//
// basis false is the other way to decide nothing, and it is not the same as a
// rate of zero. A window nobody could measure contributes no event to the run,
// so a run over an unmeasured axis holds trivially -- and reporting that as
// "holds" would put a promise on the screen that rests on no reading at all.
func decide(ax Axis, sim []simAccount, low, high map[usage.WindowName]float64, scope []usage.WindowName, now time.Time, basis bool) Axis {
	if !basis {
		ax.Verdict = VerdictUnknown
		return ax
	}
	lowAt, lowDry, _ := simulateScoped(sim, low, scope, now)
	_, highDry, _ := simulateScoped(sim, high, scope, now)
	switch {
	case lowDry && highDry:
		// The low run is the later of the two, and it is the one reported: the
		// moment named should be the one the measurement itself points at, not
		// the earliest the error bar allows.
		ax.Verdict, ax.DryAt, ax.HasDryAt = VerdictRunsDry, lowAt, true
	case !lowDry && !highDry:
		ax.Verdict = VerdictHolds
	default:
		ax.Verdict = VerdictUnknown
	}
	return ax
}

// rowsOf builds the per-account block, ordered by the moment the run found each
// account out and then by the account's own index.
//
// An account with no moment sorts last rather than first: it is the account the
// run never saw go out, and putting it at the top would bury the one that does.
func rowsOf(accounts []account, emptyAt map[string]time.Time, span time.Duration, from, now time.Time) []AccountRow {
	// The index rides alongside the row rather than on it, because it is a
	// sorting key and not part of the answer: a renderer already has the
	// account it is drawing and does not need this package to hand its index
	// back.
	type ranked struct {
		row AccountRow
		idx int
	}
	ordered := make([]ranked, 0, len(accounts))
	for _, a := range accounts {
		// Every readable account gets a row, including one with no weekly
		// window: it is in the fleet, the run burns it and can find it out,
		// and the EMPTY column has something true to say about it. Only the
		// three weekly cells are missing, and they are marked missing rather
		// than filled from another axis.
		row := AccountRow{UUID: a.in.UUID, HasWindow: a.hasBind}
		row.OutNow = minRoomOf(a) <= 0
		if at, ok := emptyAt[a.in.UUID]; ok {
			row.EmptyAt, row.HasEmpty = at, true
		}
		if !a.hasBind {
			ordered = append(ordered, ranked{row: row, idx: a.in.Idx})
			continue
		}
		row.Window, row.Left = a.bind, 100-a.sim[a.bind].pct
		if f, _, _, ok := windowRate(a.in.Series, a.bind, from, now); ok {
			// Divided by the SHARED span rather than by this account's own
			// cover, so the column sums to the axis figure above it. An account
			// watched for forty minutes of the four hours contributed forty
			// minutes of burn to a four-hour fleet rate, and its row has to say
			// the same thing the sum does.
			row.Burn = fleetBand(f, 1, span)
		}
		ordered = append(ordered, ranked{row: row, idx: a.in.Idx})
	}
	slices.SortStableFunc(ordered, func(x, y ranked) int {
		switch {
		case x.row.HasEmpty && !y.row.HasEmpty:
			return -1
		case !x.row.HasEmpty && y.row.HasEmpty:
			return 1
		case x.row.HasEmpty && y.row.HasEmpty && !x.row.EmptyAt.Equal(y.row.EmptyAt):
			return x.row.EmptyAt.Compare(y.row.EmptyAt)
		}
		return x.idx - y.idx
	})
	out := make([]AccountRow, 0, len(ordered))
	for _, r := range ordered {
		out = append(out, r.row)
	}
	return out
}

// minRoomOf is the least room across ALL of an account's windows, which is what
// "out" means: an account is out while any window of it is at the limit,
// whatever axis that window belongs to. It goes through the run's own rule
// rather than repeating it, so the row and the run can never disagree about
// which accounts are spent.
func minRoomOf(a account) float64 {
	s := simAccount{windows: a.sim}
	return s.minRoom()
}

// creditInputs is the fleet's paid-usage position: the balance from each
// current snapshot, the spend rate from each series, over one shared span.
//
// An account reaches the slice only when its used balance was readable AND its
// spend rate was measured, because a rate of zero inside the slice has to mean
// "polled, and spent nothing". An unmeasured account joining as a zero would be
// indistinguishable from that and would quietly hold the fleet's summed rate
// down -- which lengthens a runway, on the one axis this repository fails
// closed on.
//
// An unreadable monthly limit becomes nil, which means UNLIMITED and not a cap
// of zero. Whether that refuses the whole figure is creditFleet's decision and
// depends on whether the uncapped account is spending.
// creditInputs takes the eligible Inputs rather than the simulatable accounts,
// because the two sets differ by exactly the accounts this axis exists for.
// creditFleetInputs is creditInputs plus the arguments creditFleet takes with
// them, so the two calls cannot drift: the basis a caller passes is always the
// basis the inputs beside it were measured from.
func creditFleetInputs(in []Input, from, now time.Time) ([]creditInput, creditBasis, time.Time) {
	out, b := creditInputs(in, from, now)
	return out, b, now
}

func creditInputs(in []Input, from, now time.Time) ([]creditInput, creditBasis) {
	type measuredCredit struct {
		in       creditInput
		spent    float64
		readings int
	}
	var (
		found []measuredCredit
		acc   spanAcc
	)
	for _, a := range in {
		// A reading is what carries the balance, and an account that has never
		// had one is not a zero balance.
		if a.Snapshot == nil {
			continue
		}
		e := a.Snapshot.ExtraUsage
		used, ok := e.UsedCredits()
		if !ok {
			continue
		}
		spent, _, readings, ok := creditSpend(a.Series, from, now)
		if !ok {
			continue
		}
		first, last, ok := spanOf(a.Series, from, now, carriesCredit)
		if !ok {
			continue
		}
		c := creditInput{used: used, currency: e.CurrencyCode()}
		if limit, ok := e.MonthlyLimit(); ok {
			c.limit = &limit
		}
		found = append(found, measuredCredit{in: c, spent: spent, readings: readings})
		acc.add(first, last)
	}
	span := acc.span()
	if span <= 0 {
		return nil, creditBasis{}
	}
	out := make([]creditInput, 0, len(found))
	b := creditBasis{observed: span}
	for _, m := range found {
		m.in.rate = m.spent / span.Hours()
		out = append(out, m.in)
		// Summed over the accounts that reached the slice and no others, so the
		// basis describes the figure that was actually assembled rather than
		// the fleet it was drawn from. An account creditSpend refused
		// contributes to neither the rate nor the evidence for it.
		b.spent += m.spent
		b.readings += m.readings
	}
	return out, b
}

// tierNotice says when the fleet's percentage points may not be adding up.
//
// Summing points across accounts assumes their quotas are the same size, and
// rate_limit_tier is the only evidence of that this build has. It is tri-state:
// an account added while the profile endpoint was unreachable carries "" and is
// still polled and ranked, so an absent tier is never counted as agreement.
//
// Two known tiers that differ are reported ahead of an absent one, because that
// is the stronger statement: the mix is then a fact rather than a possibility,
// and naming the two tiers lets a reader check it.
func tierNotice(accounts []account) string {
	seen := map[string]bool{}
	missing := 0
	for _, a := range accounts {
		if a.in.Tier == "" {
			missing++
			continue
		}
		seen[a.in.Tier] = true
	}
	if len(seen) > 1 {
		tiers := make([]string, 0, len(seen))
		for t := range seen {
			tiers = append(tiers, t)
		}
		slices.Sort(tiers)
		return fmt.Sprintf("the fleet mixes plan tiers (%s), so these accounts' quotas are not the same size and their percentage points do not add up",
			strings.Join(tiers, ", "))
	}
	if missing > 0 {
		return fmt.Sprintf("%d of %d accounts report no plan tier, so the plan mix could not be determined and these percentage points may be summing quotas of different sizes",
			missing, len(accounts))
	}
	return ""
}

// readingsIn is how many samples fell inside the measured window across the
// whole fleet, including accounts that cleared no gate. It is the size of the
// evidence a reader is being asked to weigh, not the size of the part that was
// usable, and an account whose samples were all refused still took them.
//
// The fleet here is the ELIGIBLE accounts, which is the set every other figure
// on the page was measured over. Readings taken from an account the rotation
// cannot reach were never going to be weighed against anything.
func readingsIn(in []Input, from, to time.Time) int {
	n := 0
	for _, a := range in {
		for _, s := range a.Series {
			if !s.At.Before(from) && !s.At.After(to) {
				n++
			}
		}
	}
	return n
}

// carriesWindow and carriesCredit are the two "this sample has something to say
// about it" predicates, written once so spanOf pairs on exactly what the
// measuring functions pair on.
func carriesWindow(n usage.WindowName) func(history.Sample) bool {
	return func(s history.Sample) bool {
		_, ok := s.Windows[n]
		return ok
	}
}

func carriesCredit(s history.Sample) bool { return s.Credit != nil }

// spanOf is the moments an account's own readings reach between, inside
// [from, to].
//
// It exists because a fleet figure divides SUMMED consumption by ONE SHARED
// span -- the latest last reading minus the earliest first one -- and the
// measuring functions report each account's cover as a duration, which cannot
// be combined that way: two accounts each covering two hours may overlap
// entirely or not at all. What is needed is the endpoints, not the length.
func spanOf(series []history.Sample, from, to time.Time, carries func(history.Sample) bool) (first, last time.Time, ok bool) {
	for _, s := range series {
		if s.At.Before(from) || s.At.After(to) || !carries(s) {
			continue
		}
		if !ok {
			first, ok = s.At, true
		}
		last = s.At
	}
	return first, last, ok
}

// spanAcc accumulates the shared interval a set of measurements reaches across.
type spanAcc struct {
	first, last time.Time
	any         bool
}

func (a *spanAcc) add(first, last time.Time) {
	if !a.any {
		a.first, a.last, a.any = first, last, true
		return
	}
	if first.Before(a.first) {
		a.first = first
	}
	if last.After(a.last) {
		a.last = last
	}
}

// span is the interval, and it is zero when nothing was added -- which
// fleetBand reads as "no rate" rather than as a rate of zero.
func (a *spanAcc) span() time.Duration {
	if !a.any {
		return 0
	}
	return a.last.Sub(a.first)
}

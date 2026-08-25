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
	Window usage.WindowName

	// Left is 100 minus that window's utilization, read from the CURRENT
	// snapshot. The rows sum to Fleet.PointsLeft, which is what lets a reader
	// check one figure against the other.
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
	// the denominator every time an account rolled over.
	PointsLeft  float64
	PointsTotal float64

	// Rows is one line per readable account, ordered by EmptyAt ascending and
	// then by the account's index, so the account that goes first reads first.
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

	// Idx is the account's index in the store, and it is what breaks ties: two
	// accounts with identical room must be picked in the same order on every
	// run, or six runs of one fleet disagree about a fleet that never changed.
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
	var unreadable int
	for _, a := range in {
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
			Readings:   readingsIn(in, from, now),
			Accounts:   len(in),
			Unmeasured: len(accounts) - measured,
			Unreadable: unreadable,
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
	f.FiveHour.Replenish = replenish(len(accounts), usage.WindowFiveHour)
	f.Weekly.Replenish = replenish(len(accounts), usage.WindowSevenDay)

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

	lowRates, highRates := rates.low(), rates.high()
	f.FiveHour = decide(f.FiveHour, sim, lowRates, highRates, five, now, f.FiveHour.Burn.Known)
	f.Weekly = decide(f.Weekly, sim, lowRates, highRates, weekly, now, f.Weekly.Burn.Known)
	f.Both = decide(f.Both, sim, lowRates, highRates, both, now, f.Basis.Known)

	// The rows come out of the fleet run at the low end of the band. That run
	// is the one whose dry moment is reported when both ends agree, so the last
	// row's moment and the fleet's own are the same figure rather than two.
	_, _, emptyAt := simulateScoped(sim, lowRates, both, now)
	f.Rows = rowsOf(accounts, emptyAt, bindSpan, from, now)
	for _, r := range f.Rows {
		f.PointsLeft += r.Left
	}
	f.PointsTotal = 100 * float64(len(f.Rows))

	f.Credit = creditFleet(creditInputs(accounts, from, now), now)
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
// The fallback to a non-weekly window is for the account whose weekly windows
// dropped out of one response while its five-hour one came back. Reporting no
// row for it would leave a readable account out of the points total while its
// neighbours' rows still claimed to sum to it; naming the window it does have
// keeps the three figures on one set of accounts and invents no level.
func bindingWindow(a account) (usage.WindowName, bool) {
	best, found := usage.WindowName(""), false
	var bestRoom float64
	for _, w := range a.order {
		room := 100 - a.sim[w.Name].pct
		weekly := usage.IsWeekly(w.Name)
		switch {
		case !found:
		case weekly && !usage.IsWeekly(best):
		case weekly == usage.IsWeekly(best) && room < bestRoom:
		default:
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

// replenish is what an axis gives back: one window's worth per readable account
// per window length.
//
// It counts accounts the run will find unusable, and that divergence is the
// point of keeping this figure out of the verdict: an account out on the other
// axis contributes here and supplies nothing to the rotation.
func replenish(accounts int, of usage.WindowName) float64 {
	length, known := usage.WindowLength(of)
	if !known || length <= 0 {
		return 0
	}
	return float64(accounts) * 100 / length.Hours()
}

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
		if !a.hasBind {
			continue
		}
		row := AccountRow{UUID: a.in.UUID, Window: a.bind, Left: 100 - a.sim[a.bind].pct}
		row.OutNow = minRoomOf(a) <= 0
		if at, ok := emptyAt[a.in.UUID]; ok {
			row.EmptyAt, row.HasEmpty = at, true
		}
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
func creditInputs(accounts []account, from, now time.Time) []creditInput {
	type measuredCredit struct {
		in    creditInput
		spent float64
	}
	var (
		found []measuredCredit
		acc   spanAcc
	)
	for _, a := range accounts {
		e := a.in.Snapshot.ExtraUsage
		used, ok := e.UsedCredits()
		if !ok {
			continue
		}
		spent, _, _, ok := creditSpend(a.in.Series, from, now)
		if !ok {
			continue
		}
		first, last, ok := spanOf(a.in.Series, from, now, carriesCredit)
		if !ok {
			continue
		}
		c := creditInput{used: used, currency: e.CurrencyCode()}
		if limit, ok := e.MonthlyLimit(); ok {
			c.limit = &limit
		}
		found = append(found, measuredCredit{in: c, spent: spent})
		acc.add(first, last)
	}
	span := acc.span()
	if span <= 0 {
		return nil
	}
	out := make([]creditInput, 0, len(found))
	for _, m := range found {
		m.in.rate = m.spent / span.Hours()
		out = append(out, m.in)
	}
	return out
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

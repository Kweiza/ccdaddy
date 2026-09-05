package strategy

import (
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// hover derives every threshold instead of reading one.
//
// A configured threshold is a number a person typed, and it cannot know two
// things the engine knows every tick: how far through its window each account
// is, and how many accounts are left to share the remaining quota with. The
// same 80 that is generous with four accounts three days into a week is
// ruinous with one account four hours into a five-hour window, so a user who
// tunes for one of those is mis-tuned for the other every day.
//
//	threshold = elapsed share of the window + share
//	share     = max(100 / usable accounts, stranded)
//	stranded  = (100 - weekly used) - usable x (100 - weekly elapsed),
//	            floored at zero, and refused outright to an account that
//	            cannot absorb one HoverCooldown of work
//
// Read it as a pace target. An account further through its quota than through
// its window, by more than its own slice of what is left, should hand the next
// session to a peer. With four accounts and a week 43% gone that is 68; with
// one account left it is the cap, because there is nobody to hand to and
// holding quota back buys nothing.
//
// The stranded half is the same sentence about a pool that has run out of
// LATER rather than out of peers. Quota an account holds that the rotation
// cannot reach before its window resets is quota nobody is going to get back,
// so restraint about it buys nothing either -- and the share says so by
// widening to cover it. hoverStranded is where that is priced and hoverShare is
// where the two halves meet.

const (
	// A derived threshold is DELIBERATELY UNCLAMPED, and this is the note that
	// says why, because the reasoning is the opposite of what it looks like.
	//
	// A derived threshold used to be clamped to 99 on the reasoning that a
	// threshold of 100 is not crossed until the account is completely out. That
	// reasoning was sound and the clamp was still wrong, because of WHICH
	// account it lands on: the clamp fires when the elapsed share plus the pool
	// share exceeds the cap, which is to say on whichever account is furthest
	// through its own window -- exactly the account whose quota expires soonest
	// and which the pace target exists to send work to. Above that line the
	// threshold was a flat 99 and the slack collapsed to 99 - utilization, with
	// the elapsed term thrown away, so the pool was ordered on raw utilization
	// for precisely the accounts that mattered most.
	//
	// It was measured rather than argued. Three accounts with weekly resets one,
	// three and five days out, driven through Decide on thirty-minute ticks for
	// five days: the clamp was live on 240 of 240 ticks, and on every single
	// tick where the ranking failed to put the neediest account first, that
	// account was the clamped one. The same loop ordered directly on the pace
	// deficit settled to a spread of 3.5 points; hover under the clamp
	// oscillated between 17 and 41, for the same number of switches.
	//
	// What made the clamp look load-bearing was that "empty implies spent"
	// rested on it: 100 > 99 guarantees a used-up account reports negative
	// slack. That now holds structurally instead -- see Spent, which reads
	// MinPct -- so the clamp has nothing left to carry.
	//
	// Uncapped, a threshold above 100 is a real and readable statement: there is
	// nobody to hand the work to, so nothing is being held back. Nothing clamps
	// it on the way to a screen either, and that is deliberate rather than an
	// omission: a threshold the table capped would no longer reconcile with the
	// slack printed beside it, and the pair is the whole of what a reader has to
	// audit this mode with. There was a HoverDisplayCap constant here saying the
	// human table held the figure to 100; nothing ever read it, and
	// TestHoverStatusShowsWhatEachWindowUsedAndWhatItIsHeldTo has been rendering
	// `62%/130%` for as long as it has existed.

	// HoverUnknownElapsedPct is the share of itself a window is ASSUMED to have
	// elapsed when it will not say. It is an elapsed share on [0, 100] and not a
	// threshold, which is the whole of the structural move: after it there is
	// exactly ONE expression in this mode that produces a threshold, so a second
	// one cannot drift off the first one's scale.
	//
	// There was such a second one. A window with no elapsed share used to take a
	// flat 80 -- the configured default, on the reasoning that it is what the
	// engine would have used before hover existed. That is a defensible number
	// on the CONFIGURED scale and it is not a number on this one. A derived
	// threshold is ExpectedPct + share, so for a pool of n the scale runs from
	// 100/n to 100 + 100/n, and a flat 80 is the threshold of a window
	// 80 - 100/n percent elapsed -- an implied position that MOVES WITH THE POOL,
	// and backwards: 30% elapsed with two accounts, 55% with four, 67.5% with
	// eight. Hover's premise is that a larger pool holds each account to a
	// tighter pace; the flat figure assumed the unknown window was further
	// through its cycle, and so more generous, the more accounts there were. In
	// a pool of one it was not on the scale at all -- the scale there starts at
	// 100.
	//
	// The error changed sign at elapsed = 80 - share, and which side of that a
	// window sat on is exactly the quantity that is unknown. Measured on a pool
	// of four: at 25% elapsed the derived threshold is 50 and the flat 80 was 30
	// points too lax; at 95% elapsed it is 120 and the flat 80 was 40 points too
	// strict. Both directions were reproduced through Rank. The strict one
	// crosses the SPENT TIER, which lessHeadroom compares before slack, so no
	// margin could absorb it: an account holding fifteen points of its week lost
	// to one holding three, and no value of HoverHysteresisPct could have
	// changed that. TestAWindowWithNoClockDoesNotFileTheAccountAsSpent is that
	// pair.
	//
	// 50 because a window that will not say how far through it is has no
	// evidence either way, and the midpoint is the only figure that does not
	// smuggle one in. It is not a tuning number and there is nothing to tune it
	// against; what makes it safe is that it is now carried on the SAME scale as
	// every measured window, so the pool's own size still decides what it is
	// worth. TestHoverPutsAWindowWithNoClockOnTheDerivedScale sweeps the pool
	// sizes.
	//
	// It is deliberately NOT closed by probing. The window that lands here most
	// often is one that reported utilization but no reset, and neverSpent
	// requires a utilization of zero, so ColdWindow never targets it and the
	// daemon never warms it -- cold_test.go's "spent with no rollover is an
	// unreadable reset, not a stopped clock" is right, and another turn buys the
	// same unreadable field back forever. The figure has to be right because
	// nothing is going to replace it.
	HoverUnknownElapsedPct = 50.0

	// HoverCreditThreshold is the figure a primary credit seat is measured on.
	//
	// A credit seat has no window and no reset, so there is no pace to derive
	// anything from and no rollover to wait for. 95 rather than the cap
	// because credits do not come back at all: the last few points are the
	// only warning a user gets that the seat is nearly spent, where a
	// subscription window's last few points are gone in hours anyway.
	HoverCreditThreshold = 95.0

	// HoverHysteresisPct is 3 rather than the stock 10 because hover's
	// thresholds MOVE. Every window's threshold rises as the window elapses, so
	// a margin sized for a static threshold holds the engine on an account
	// whose slack the passing of time is already eroding.
	//
	// Three rather than the five it was first set to, because five turned out
	// to sit ABOVE the spread a real pool shows. Two accounts whose binding
	// windows are the same LENGTH have thresholds that rise at the same rate,
	// so the gap between their slacks does not close with time at all: only
	// burn closes it, and the burn is on the account the margin is holding the
	// engine on. The fleet of 2026-08-25 sat four points apart under a
	// five-point margin, live account at ten points of raw room and the
	// candidate at thirty -- so the margin spent the difference on the emptier
	// of the two and only released once the live account was a point nearer its
	// limit. TestTheHoverMarginIsUnderTheSpreadARealPoolShows is that pool.
	//
	// Three is still above the noise floor the stock margin was sized against
	// -- a binding window flipping between two windows a point apart, which
	// moves headroom by a point or two with no usage at all. What bounds the
	// flap RATE is HoverCooldown below, which is the mechanism for that job; a
	// margin sized to do it as well is a margin that strands quota.
	//
	// Three is unchanged now that the share carries a perishability term, and
	// the term is what makes the figure do the job it was sized for. A pool with
	// nothing at risk still separates by the same handful of points it always
	// did, so the margin still holds the engine still. A pool with quota about
	// to expire separates by tens: on the fleet of 2026-09-05 the whole pool
	// spanned 1.327 points and this margin was 2.3 times the entire spread,
	// which is how the account holding a week that expired in fifteen hours sat
	// last and stayed there; with the widened share the gap to it is 15.668. The
	// noise floor is untouched -- the wire reports utilization as a whole
	// percent and one point of it still moves slack by exactly one point.
	HoverHysteresisPct = 3.0

	// HoverHeadroomRatio is 1.0, which is "no multiplicative margin".
	//
	// The ratio runs on RAW headroom while the ranking orders on slack, and the
	// two disagree hardest exactly where hover operates: with a tight,
	// pace-derived weekly floor, an account one point from that floor still
	// shows forty points of raw headroom, and the ratio refuses the move the
	// ranking asked for. Hover's own additive margin and its cooldown are what
	// bound the flap rate instead.
	HoverHeadroomRatio = 1.0

	// HoverCooldown is 2 minutes rather than 5 for the same reason the margin
	// shrank: the thresholds are re-derived every tick, so a five-minute hold
	// is five minutes of running against numbers the clock has already moved
	// past.
	HoverCooldown = 2 * time.Minute

	// HoverRecoveryHysteresis keeps the stock 300 s. It is a margin on RESET
	// INSTANTS, which hover neither derives nor moves, so there is nothing
	// about hover that makes the stock figure wrong.
	HoverRecoveryHysteresis = 300 * time.Second

	// HoverMinPreemptLead is 60 s, which is the tightest cadence the poll
	// policy ever runs at. A lead shorter than one poll is a lead the engine
	// cannot act inside of.
	HoverMinPreemptLead = 60 * time.Second

	// HoverMaxPreemptLead is 10 minutes. Every second of lead is quota on the
	// live account deliberately left unspent, and the only cadence that
	// produces a wider gap is the post-429 ceiling of 1800 s -- where the
	// pre-emptive horizon already carries that whole blind interval, so the
	// lead does not have to carry it a second time.
	HoverMaxPreemptLead = 600 * time.Second
)

// HoverWindow is one derived threshold together with the figures it came from.
//
// It carries the inputs and not only the answer, because an automatic mode a
// user cannot audit is one they have to take on trust. `ccdad status` exposes
// the resulting threshold beside each window.
type HoverWindow struct {
	UUID   string
	Window usage.WindowName
	// ExpectedPct is the share of the window that has elapsed, and HasExpected
	// whether there was one. False means the threshold is the fallback.
	ExpectedPct float64
	HasExpected bool
	// Utilization is what the window reported. A window that reported none
	// gets no row at all: it cannot bind, so there is no threshold to derive.
	Utilization float64
	Threshold   float64
	Slack       float64
	// ProbeWanted marks a window that reported no reset. It is a rendering
	// fact, not a queue: hover forces probe_unknown on and the engine's own
	// probe path is what spends the turn that wakes the window.
	//
	// Warmup below is the one that answers "will anything actually happen", and
	// the two are kept apart deliberately. This field's name and its JSON key
	// have always meant "this window named no reset", which is a true and useful
	// thing to say; what was wrong was the renderer printing "a probe is queued"
	// from it.
	ProbeWanted bool
	// Warmup is what the warm-up loop would do about this row, and it is set on
	// exactly one row per account.
	Warmup Warmup
	// Credit marks the one row that is not a window: a primary credit seat's
	// own metering.
	Credit bool
}

// Warmup is what the warm-up loop would do about one row, as far as the ranking
// package can see it.
//
// It is filled for exactly one window per account — the one ColdWindow targets —
// and left zero everywhere else, because a warm-up is one turn aimed at one
// window and a table that marked every stopped clock would promise a turn per
// row.
//
// What is deliberately NOT here is the half the daemon knows and this package
// does not: which account is live, whether probe_unknown is on, whether there is
// a Claude Code on this PATH. The daemon's own status layers those on top. The
// split is what keeps the gate itself in one place — the parts that could drift
// are the parts nobody duplicated.
type Warmup struct {
	// Target marks the row a warm-up would aim at.
	Target bool
	// Credits marks the one refusal that is about money: a turn spent here
	// could be billed to metered credits, which unattended spending's two
	// opt-ins do not cover. WarmUpWouldSpendCredits is the predicate, and the
	// daemon refuses on the same call.
	Credits bool
	// RolledOver is whether the clock is stopped because it RAN DOWN, as against
	// never having been started. The two read differently to a user: one is the
	// ordinary end of a cycle, the other is an account nothing has ever used.
	RolledOver bool
	// Eligible is whether the gate would let the turn be spent now.
	Eligible bool
	// PollAt is when the scheduler next intends to look at this account, copied
	// from the cache. An eligible warm-up does not run at the instant the gate
	// opens; it runs on the next tick where the account is also poll-due, and a
	// table that named the gate's instant would be early by up to a poll
	// interval every time.
	PollAt time.Time
	// NextAt is when the backoff opens. Zero means no backoff is holding it —
	// either nothing is, or what is holding it is "one warm-up per rollover",
	// which has no instant to name because the next rollover has not been
	// scheduled by anything yet.
	NextAt time.Time
	// Streak is how many consecutive warm-ups of this window woke nothing.
	Streak int
	// LastAttemptAt and LastError are the last attempt's stamp and its report.
	// LastError is shown and never gated on; usage.ProbeState says why.
	LastAttemptAt time.Time
	LastError     string
}

// warmupFor is the state of one account's warm-up, for the window ColdWindow
// picked. It asks usage.ProbeState the same question the daemon asks, through
// the same method, so the table cannot promise a turn the gate would refuse.
func warmupFor(p usage.ProbeState, w usage.WindowName, rollover, pollAt, now time.Time, credits bool) Warmup {
	out := Warmup{
		Target:        true,
		Credits:       credits,
		RolledOver:    !rollover.IsZero(),
		PollAt:        pollAt,
		Streak:        p.Strikes(w),
		LastAttemptAt: p.LastAttemptAt,
		LastError:     p.LastError,
	}
	out.Eligible = p.MayProbe(now, w, rollover, out.RolledOver)
	if !out.Eligible && !(out.RolledOver && out.Streak == 0) {
		out.NextAt = p.NextAttemptAt(w)
	}
	return out
}

// HoverPlan is one hover pass over one pool.
type HoverPlan struct {
	// Usable is how many accounts the quota is divided between: eligible, with
	// a reading, and -- because Decide filters the pool before this runs -- not
	// quarantined.
	Usable int
	// Windows is every derived threshold, in pool order and then in wire
	// order, so two runs over one pool render identically.
	Windows []HoverWindow
	// CreditThreshold is what a primary credit seat is measured on.
	CreditThreshold float64
	// PreemptLead is derived from the observed poll gap; see hoverPreemptLead.
	PreemptLead time.Duration
	// BurnPerMin is the rate the licence floor was priced in, and HasBurn whether
	// one was measured. Both are REPORTING as well as input: a floor stated in
	// points of work is a floor a reader cannot reconstruct unless the rate it
	// used is on the page beside it, which is the objection hover's own note
	// raises against reading a rate at all.
	BurnPerMin float64
	HasBurn    bool
	// Accounts is the per-account half of the derivation, in pool order.
	//
	// It is new because the share stopped being one number for the whole pool.
	// Every row in Windows still satisfies Threshold = ExpectedPct + share, and
	// a reader who cannot see WHICH share cannot close that arithmetic -- which
	// is the whole of what this mode promises in exchange for overriding the
	// numbers a user typed.
	Accounts []HoverAccount

	byUUID map[string]Thresholds
}

// HoverAccount is one account's share and the figures that widened it.
//
// Window and ResetsAt are carried because Stranded is the one figure in this
// table taken off a DIFFERENT row than the one it moves: the five-hour row's
// threshold rises because of the seven-day row's numbers, so the window is
// named here rather than left to be discovered by a reader comparing rows.
//
// Stranded is the EFFECTIVE figure, after the cooldown floor, because that is
// the one that widened the share: zero for an account the floor refused,
// however much of its week is about to expire.
type HoverAccount struct {
	UUID string
	// Share is what this account's thresholds were built on: the larger of
	// PoolShare and Stranded.
	Share float64
	// PoolShare is the flat slice, 100 divided between the usable accounts.
	PoolShare float64
	// Stranded is the quota the rotation cannot reach before it expires, and
	// HasStranded whether there was a perishable window to price at all. False
	// spans every silence -- no weekly window in the narrowed set, one with no
	// readable utilization, one whose reset has already passed -- and in each of
	// them the share is the flat slice.
	HasStranded bool
	Stranded    float64
	// Window is the perishable window Stranded was priced on, and ResetsAt when
	// it goes. Both are zero when HasStranded is false.
	Window   usage.WindowName
	ResetsAt time.Time
}

// AccountFor is one account's row of the per-account table.
func (p HoverPlan) AccountFor(uuid string) (HoverAccount, bool) {
	for _, a := range p.Accounts {
		if a.UUID == uuid {
			return a, true
		}
	}
	return HoverAccount{}, false
}

// ShareFor is the share one account's thresholds were derived with.
//
// An account that was not in the pool this plan was built from gets the flat
// slice, which is the same fallback For makes for the same reason: there is no
// perishable window on record for it, so there is nothing to widen.
func (p HoverPlan) ShareFor(uuid string) float64 {
	if a, ok := p.AccountFor(uuid); ok {
		return a.Share
	}
	return hoverPoolShare(p.Usable)
}

// HoverThresholds derives the whole table for one pool.
//
// It takes the pool AFTER quarantine rather than the whole store, because a
// quarantined account cannot be switched to: counting it would divide the quota
// between accounts one of which nothing can use, tightening every other
// account's threshold for nothing.
func HoverThresholds(cands []Candidate, o Options) HoverPlan {
	p := HoverPlan{
		CreditThreshold: HoverCreditThreshold,
		byUUID:          map[string]Thresholds{},
	}

	pool := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		// An account with no reading is not one the work can go to, and it is
		// not evidence either: it is neither over nor under anything.
		if !eligible(c) || c.Usage == nil {
			continue
		}
		pool = append(pool, c)
	}
	p.Usable = len(pool)
	p.PreemptLead = hoverPreemptLead(pool)
	// The measured rate the licence floor is priced in. It is taken over the same
	// pool the shares are divided over, once, so every account's floor is stated
	// in the same units -- points of work a session actually spends, rather than
	// points of clock.
	burn, hasBurn := SessionBurnPerMin(pool)
	p.BurnPerMin, p.HasBurn = burn, hasBurn

	// The CONFIGURED table decides what may bind, and hover decides what it is
	// worth. The two are not the same question. A weekly cap filed under a scope
	// key this build cannot name binds only where a threshold names it, and that
	// entry is CONSENT -- the user saying they know what the scope caps -- not a
	// tuning number. Hover overrides every number a user typed; it may not
	// supply an opt-in on their behalf, so it reads the table here rather than
	// its own, which is empty at this point in any case.
	configured := o.Thresholds()

	for _, c := range pool {
		// The credit row FIRST, and then the seat falls through into the
		// ordinary derivation like every other account.
		//
		// It used to return here instead, on the ground that "a seat metered in
		// credits carries no plan windows and no reset". That sentence is not
		// true, and identity.Classify is where it comes apart: a seat is filed
		// KindCredit off Snapshot.HasSubscriptionWindows, which iterates
		// RateLimitWindows -- the fixed five and the two codex keys, and nothing
		// else. A weekly cap that arrived in limits[] is invisible to that test,
		// so an account with no fixed windows, extra_usage enabled and a real
		// scoped weekly classifies as a credit seat AND carries quota.
		//
		// The cost of returning early was a table with no PerWindow at all, and
		// bindingWindows reads exactly that map to decide whether an unknown
		// scope was opted into. So every opt-in the user typed was dropped for
		// this one account, and the two sides of the pre-emptive switch --
		// which judged the live account on the configured table and every
		// candidate on the derived one -- disagreed about whether the seat's own
		// cap existed. Measured: a seat whose opted-in weekly is 99.9% used with
		// an hour to run projects exhaustion in ten minutes on the configured
		// table and none at all on the derived one, and ccdad pre-empted ONTO it
		// inside a forty-minute horizon.
		// TestHoverDerivesATableForAPrimaryCreditSeatToo pins the two window
		// sets equal; TestPreemptionWillNotRunToASeatWhoseOptedInCapRunsOutFirst
		// is the switch that used to be made.
		//
		// Falling through is what fixes it, and NOT copying the configured
		// PerWindow forward. That was the obvious repair and it is wrong:
		// TestTheWeeklyResetIsReadFromTheSameTableTheHeadroomWas blesses the
		// opposite invariant for every ordinary account -- hover's table admits
		// exactly the windows hover derived a threshold for -- and consent
		// copied forward would break it.
		if pct, ok := creditRowPct(c); ok {
			p.Windows = append(p.Windows, HoverWindow{
				UUID:        c.UUID,
				Window:      creditWindow,
				Utilization: pct,
				Threshold:   HoverCreditThreshold,
				Slack:       HoverCreditThreshold - pct,
				Credit:      true,
			})
		}

		// bindingWindows, so the set hover derives thresholds for is exactly
		// the set the ranking measures against -- cinder_cove excluded, and the
		// model narrowing applied once rather than twice.
		// The row a warm-up would aim at, decided ONCE per account and by the
		// same function the daemon's probeDue calls. Asking per row would let
		// the table mark a window the loop never targets.
		cold, rollover, isCold := ColdWindow(c.Usage, o.Model, configured, o.Now)
		credits := WarmUpWouldSpendCredits(c.Usage, o.Model, configured)

		// The share, decided ONCE per account and before any row is derived.
		// Every row below reads it, which is what makes the widening a licence
		// the ACCOUNT holds rather than a property of one window -- and that is
		// the whole point: the perishable window is the one window that can
		// never bind, so a figure that stayed on it would never reach the
		// comparator. withHover says why at length.
		sw, stranded, hasStranded := hoverStranded(c.Usage, o.Model, configured, p.Usable, o.Now, burn, hasBurn)
		share := hoverShare(p.Usable, stranded)
		acct := HoverAccount{
			UUID:        c.UUID,
			Share:       share,
			PoolShare:   hoverPoolShare(p.Usable),
			HasStranded: hasStranded,
			Stranded:    stranded,
		}
		if hasStranded {
			acct.Window = sw.Name
			acct.ResetsAt, _ = sw.Reset()
		}
		p.Accounts = append(p.Accounts, acct)

		per := map[usage.WindowName]float64{}
		for _, w := range bindingWindows(c.Usage, o.Model, configured) {
			pct, ok := w.Percent()
			if !ok {
				// Nothing to compare a threshold against. This window cannot
				// bind, so deriving one would be arithmetic nobody reads.
				continue
			}
			row := HoverWindow{UUID: c.UUID, Window: w.Name, Utilization: pct}
			row.ExpectedPct, row.HasExpected = hoverElapsedPct(w, o.Now)
			// ONE expression, no branch. HasExpected still says whether the
			// share was measured or assumed -- a reader has to be able to tell
			// -- but both go through the same arithmetic, so a window hover
			// cannot read is held to the same scale as one it can.
			// Deliberately unclamped; the note on the const block says why.
			row.Threshold = row.ExpectedPct + share
			// The CONDITION is the reset and not HasExpected, and the two are
			// no longer the same set. A window that named its reset and still
			// has no measured share is a clock problem -- either a reset
			// further out than the window is long, which hoverElapsedPct now
			// reads as a window that has just started, or a length this build
			// does not know, which is reachable only for a codex window with no
			// limit_window_seconds and which the codex lane never ranks under
			// hover. Spending a turn of the user's quota fixes neither. Only the
			// window nothing has ever spent against is marked.
			if _, hasReset := w.Reset(); !hasReset {
				row.ProbeWanted = true
			}
			if isCold && w.Name == cold {
				row.Warmup = warmupFor(c.Probe, cold, rollover, c.NextPollAt, o.Now, credits)
			}
			row.Slack = row.Threshold - pct
			// The same clamp HeadroomFor applies, for the same reason and so
			// that this table cannot report a slack the engine did not rank on.
			// A window with nothing left never shows positive slack however far
			// past 100 its pace target ran.
			if pct >= 100 && row.Slack > 0 {
				row.Slack = 100 - pct
			}
			per[w.Name] = row.Threshold
			p.Windows = append(p.Windows, row)
		}
		p.byUUID[c.UUID] = Thresholds{
			Default:   hoverUnknownThreshold(share),
			PerWindow: per,
			Credit:    HoverCreditThreshold,
		}
	}
	return p
}

// creditRowPct is a primary credit seat's own meter, and nothing for any other
// account. It is a function rather than two lines inline so that the one test
// in the pool loop stays a test about WHICH ROW to add, not about what a seat is.
func creditRowPct(c Candidate) (float64, bool) {
	if !primaryCredit(c) {
		return 0, false
	}
	return c.Usage.ExtraUsage.Percent()
}

// For is the threshold table one account is measured against.
func (p HoverPlan) For(uuid string) Thresholds {
	if t, ok := p.byUUID[uuid]; ok {
		return t
	}
	// An account that was not in the pool this plan was built from -- one with
	// no reading at all, or one that arrived after the pass. It has no window to
	// pace, so anything it later reports gets the figure a window with no reset
	// gets.
	return Thresholds{Default: hoverUnknownThreshold(p.ShareFor(uuid)), Credit: HoverCreditThreshold}
}

// hoverShare is the slice of a window one account may spend before the work
// belongs with a peer: 100 points divided between the accounts that can take it.
//
// A pool of none is answered as a pool of one rather than as a division by
// zero. With nothing to hand work to, the honest threshold is the cap -- spend
// what is left -- which is what a share of 100 produces.
func hoverPoolShare(usable int) float64 {
	if usable < 1 {
		usable = 1
	}
	return 100 / float64(usable)
}

// hoverStranded is the quota an account holds that the pool's own rotation
// cannot reach before it expires, in points of the weekly window that expires
// first among those a model choice cannot dodge.
//
// It adds no assumption to hover's model; it spells out the one the divisor was
// already making. That model says the usable accounts each burn one window per
// window-length. The same sentence says what ONE account does while it is the
// one being served: it takes all of that work, so it runs at `usable` times its
// own pace. Over the window-time left -- 100 - ExpectedPct -- an account given
// every turn from here on can absorb at most usable x (100 - ExpectedPct)
// points, and whatever of 100 - Utilization sits above that cannot be spent by
// ANYBODY before the window resets.
//
// Zero is the ordinary answer. A pool keeping up with its weeks strands nothing,
// which is why the term is inert on every fixture hover was tuned on --
// TestAPoolDrainingAtPaceGetsTheFlatShare is that check, over all three of them.
//
// It is CONSERVATIVE in all three directions it can be, and that is the
// direction to have. `usable` counts accounts that are readable and eligible,
// some of which may have nothing left to take a turn with, which OVERSTATES
// what the rotation absorbs. It prices one account in isolation, when pointing
// the whole fleet at this one would strand the others. And it is refused
// outright to an account holding less than one cooldown of work on any window
// a model choice cannot dodge, which is the one case where a widening buys a
// switch and nothing to serve on the far side of it.
func hoverStranded(s *usage.Snapshot, model string, t Thresholds, usable int, now time.Time, burnPerMin float64, hasBurn bool) (usage.NamedWindow, float64, bool) {
	if usable < 1 {
		// hoverPoolShare's own clamp, for the same reason: a pool of none is a
		// pool of one, and a rotation of nobody absorbs one account's worth
		// rather than nothing at all.
		usable = 1
	}

	var best usage.NamedWindow
	var bestAt time.Time
	found, anyModel := false, false
	for _, w := range bindingWindows(s, model, t) {
		// LENGTH first, name second -- the same rule weeklyResetOf and the
		// weekly floor apply, spelled here too so a codex window whose plan
		// makes it thirty days long is judged by what it is rather than by what
		// it is called.
		if !usage.IsWeeklyOf(w.Name, w.Window) {
			continue
		}
		if _, ok := w.Percent(); !ok {
			continue
		}
		at, ok := w.Reset()
		if !ok {
			continue
		}
		// An all-model window outranks a model-scoped one OUTRIGHT, and only
		// then does the soonest reset decide. Taking the largest surplus over
		// every weekly instead would let an untouched Fable cap, on an account
		// whose all-model week is spent, buy that account the front of the
		// queue for quota the session was never going to run.
		// TestTheStrandedWindowIsTheOneAModelChoiceCannotDodge is that pair.
		wide := !capsOneModelFamily(w.Name)
		if !found || (wide && !anyModel) || (wide == anyModel && at.Before(bestAt)) {
			best, bestAt, anyModel, found = w, at, wide, true
		}
	}
	if !found {
		return usage.NamedWindow{}, 0, false
	}

	pct, _ := best.Percent()
	expected, ok := usage.ExpectedPct(best.Name, best.Window, now)
	// A reset already in the past is a window that has ALREADY rolled over and
	// whose refresh has not landed yet. ExpectedPct caps the elapsed share at
	// 100 for one -- correctly, a window cannot be more than fully elapsed --
	// and read as urgency that is the LARGEST licence this mode can express,
	// handed out at the exact instant the urgency ended, on a figure the next
	// poll deletes. Measured on the shipped shape: share 99, slack 108, a
	// 48-point lead over a healthy account, and a switch that reverses one tick
	// later. TestAWeeklyThatHasAlreadyRolledStrandsNothing.
	if !ok || !bestAt.After(now) || expected >= 100 {
		return best, 0, false
	}

	// The absorption term is a MODEL and not a measurement, and it is worth
	// knowing which. It credits the rotation with usable x (100 - expected)
	// points over the window-time left, which is a rate of
	// usable x 100 / window_length points an hour -- the same expression
	// internal/forecast calls replenish, arrived at independently in a package
	// this one may not import.
	//
	// What the model leaves out is that a real fleet does not burn at its
	// replenish rate. Measured on the fleet of 2026-09-05, a four-account pool
	// whose weekly burn band was 0.51 to 1.52 points an hour against a modelled
	// absorption of 2.38: the model credits the rotation with between 1.6 and
	// 4.7 times the absorption ever observed, and understates what strands by
	// the same factor. On that fleet one account's stranded figure was 57.4
	// where the measured band puts it between 64.1 and 72.0.
	//
	// The error is in the direction this function already chose, for the reasons
	// on its own doc: understating what strands means widening a licence less
	// often, and a widening is a claim on the ranking that every other account
	// then has to clear. Closing it would
	// mean reading a fleet-wide burn rate, which is exactly the edge
	// TestTheEngineDoesNotImportTheForecast forbids -- a threshold a user could
	// no longer follow in `ccdad status`, because it would depend on a
	// measurement taken over the last four hours of every other account.
	stranded := (100 - pct) - float64(usable)*(100-expected)
	if stranded < 0 {
		stranded = 0
	}
	// A licence is a claim on the RANKING and not on a session, and this used
	// to confuse the two. The figure was clamped by the room the account holds
	// right now, on the ground that "this is where it becomes a claim on the
	// NEXT SESSION, so this is where it has to answer could this account serve
	// that session at all" -- and ccdad never hands out a session. It moves one
	// MID SESSION: HoverCooldown is two minutes, so the engine re-reads and
	// re-ranks the fleet thirty times an hour, and "could this account carry a
	// session" is not a property the ordering has to protect.
	//
	// The two mechanisms that do hold that line read RAW ROOM and no threshold
	// at all, so no licence can weaken them. OutOfQuota files an account with
	// nothing left in headroomTier 3, which lessHeadroom compares BEFORE slack,
	// so no widening of any size lifts an empty account --
	// TestHoverRanksTheEmptyAccountLast and TestHoverSwitchesOffTheEmptyAccount
	// are that pair. And preemptTarget refuses a candidate projected to run out
	// inside its own blind interval.
	//
	// What neither covers is the SLIVER: room strictly between zero and one
	// cooldown of work. OutOfQuota is a <= 0 test, so an account with 0.05
	// points of a five-hour window -- nine seconds of work -- sits in the roomy
	// tier, and on a licence priced off its week it leads the order and Decide
	// switches onto it, buying nine seconds with a two-minute cooldown. That is
	// the one cost a widening can impose that nothing else refuses, so it is the
	// one question asked here, and it is a BOOLEAN rather than a bound: the
	// price of a switch is fixed, so what is worth asking is whether the account
	// clears it, not by how much.
	//
	// The clamp could not ask it, because it compared across axes. stranded is
	// in points of a WEEK, and on that axis the clamp was vacuous: usable is at
	// least one and expected is under 100, so the figure is already at most
	// 100 - pct, which TestTheStrandedShareNeverExceedsTheQuotaTheAccountHolds
	// pins. It could only ever bind by reaching across to a SHORTER window's
	// room, and a five-hour point and a weekly point are different quantities
	// -- internal/forecast refuses that comparison outright on its Both axis.
	// So it could only ever bind where it was wrong.
	//
	// Measured on the fleet of 2026-09-05: an account held 61 points of a week
	// six hours from its reset, and the clamp cut its licence from 38.5 to the
	// 9 points left on a five-hour window that reset 28 minutes later -- below
	// the flat 16.67, so the widening did not shrink, it vanished, and the
	// account ranked last of six. Half an hour later the same account,
	// unchanged, priced at 38.5 under either rule: a quantity a clock deletes
	// without any work being done was never a statement about what the account
	// can serve. TestAPerishableWeekOutlivesAFiveHourWindowAboutToRoll.
	//
	// The clamp, and the test that pinned it against OutOfQuota, came in
	// fe1a5fe.
	if !absorbsACooldown(s, model, t, burnPerMin, hasBurn) {
		stranded = 0
	}
	return best, stranded, true
}

// absorbsACooldown is whether every window a MODEL CHOICE CANNOT DODGE still
// holds at least one HoverCooldown of work, falling back to every readable
// window when none of those was readable.
//
// It is what is left of the room cap, and it is deliberately a floor on the
// LICENCE rather than a filter on the switch. The two are not the same guard:
// an account whose own slack already leads needs no widening to be switched
// to, and refusing it there is how the last points of every account become
// unreachable -- TestTheLastPointOfEachAccountIsReachableUnderHover is that
// line, and it is why preemptTarget's "not itself about to run out" test stays
// on the pre-emptive arm where it lives. This refuses only to MANUFACTURE a
// lead for an account that cannot pay for the switch it would win.
//
// The comparison is per window and in that window's own points, which is the
// unit the clamp got wrong. One cooldown of a window is
// 100 x HoverCooldown / length: 0.667 points of a five-hour window and 0.0198
// of a week, both two minutes, read off usage.WindowLengthOf -- the same table
// usage.ExpectedPct paces with, so a codex week a plan makes thirty days long
// is judged by its length rather than by its name. A window with no length
// this build can name says nothing rather than refusing: there is no scale to
// state a cooldown on, and zeroing a licence over it would be arithmetic on a
// number nobody reported. It still counts as SEEN, so an account whose only
// all-model window is one such is judged on it and not on a model cap beside
// it -- the same window set OutOfQuota reads, with the same all-model-first
// fallback, which is what keeps a blown Fable cap from zeroing a licence
// OutOfQuota would not zero.
//
// The pairing with the empty tier holds in the direction that matters and now
// strictly: the floor fires while room is still POSITIVE, so an account
// OutOfQuota files empty is one this has already refused to widen, and the
// band between the two -- which the old equality left open -- is covered by
// this alone. TestTheLicenceFloorFiresBeforeTheEmptyTierDoes is that pairing,
// row by row, and its two sliver rows are the cases the equality could not say.
// burnPerMin is the MEASURED rate the work is running at, and hasBurn whether
// there was one. Where there is, it replaces the clock figure below, and the
// difference is the whole reason this parameter exists: 100 x HoverCooldown /
// length is how far the WINDOW gets in two minutes -- 0.667 points of a
// five-hour one -- while a session measured on this fleet on 2026-09-05 spends
// 5.4 points a minute, which is 10.8 points in the same two minutes. Sixteen
// times. So the floor blessed an account with one point left as able to absorb a
// cooldown of work; what it could absorb was four seconds.
//
// With no measurement it is the clock figure exactly as before, which is what
// keeps a fleet ccdad has read only once behaving as it did.
func absorbsACooldown(s *usage.Snapshot, model string, t Thresholds, burnPerMin float64, hasBurn bool) bool {
	okAny, okAll := true, true
	haveAny := false
	for _, w := range bindingWindows(s, model, t) {
		pct, ok := w.Percent()
		if !ok {
			continue
		}
		short := false
		if hasBurn && burnPerMin > 0 {
			// A cooldown of WORK, in this window's own points. It does not
			// depend on the window's length: the session spends what it spends,
			// and a two-minute hold costs the same points whether the window it
			// is charged against is five hours or a week.
			short = 100-pct < burnPerMin*HoverCooldown.Minutes()
		} else if length, ok := usage.WindowLengthOf(w.Name, w.Window); ok && length > 0 {
			short = 100-pct < 100*float64(HoverCooldown)/float64(length)
		}
		okAll = okAll && !short
		if capsOneModelFamily(w.Name) {
			continue
		}
		haveAny = true
		okAny = okAny && !short
	}
	if haveAny {
		return okAny
	}
	return okAll
}

// hoverElapsedPct is the share of itself a window has run through, and whether
// that was MEASURED or assumed.
//
// usage.ExpectedPct refuses three different facts with one false -- no window
// length, no reset instant, and an elapsed share below zero -- and hover used to
// give all three the same flat number. Two of them are answerable here:
//
//   - A reset FURTHER OUT than the window is long is not an unknown window. It
//     is a window that has just started, seen through a clock a little behind the
//     endpoint's; the elapsed share is zero and saying so is measuring rather
//     than guessing. It used to take the flat 80, which on a fresh five-hour
//     window against a pool at 25 was a 55-point lead bought with one minute of
//     skew, on a row nothing marked as a guess.
//     TestAResetPastTheWindowLengthReadsAsAWindowThatHasJustStarted.
//   - Everything else genuinely has no evidence, and takes
//     HoverUnknownElapsedPct with HasExpected false so the table can say so.
//
// internal/usage is deliberately not touched. ExpectedPct's three-way refusal is
// the right contract for a package that must not guess on anybody's behalf; this
// is hover answering two of the three on its own side of that boundary, out of
// the same exported API, so TestExpectedPctRefusesRatherThanGuessing and its
// stated reason both stand.
func hoverElapsedPct(w usage.NamedWindow, now time.Time) (float64, bool) {
	if pct, ok := usage.ExpectedPct(w.Name, w.Window, now); ok {
		return pct, true
	}
	length, hasLength := usage.WindowLengthOf(w.Name, w.Window)
	at, hasReset := w.Reset()
	// now < reset - length is ExpectedPct's own elapsed < 0, re-derived from the
	// exported API rather than by reaching into it.
	if hasLength && hasReset && now.Before(at.Add(-length)) {
		return 0, true
	}
	return HoverUnknownElapsedPct, false
}

// hoverUnknownThreshold is the threshold for a window this plan derived nothing
// for: the assumed elapsed share, on the same scale and with the same share as
// every measured one.
//
// It is the Thresholds.Default door, and there are two of them -- the table an
// account in the pool carries, and the answer For gives for an account that was
// not in the pass at all. Both must be strictly positive, because
// bindingWindows reads a non-positive per-window entry as "not opted in" and a
// zero here would silently revoke a user's consent for an unknown-scope cap.
// The share is at least 100/usable with usable clamped to one, so it is.
func hoverUnknownThreshold(share float64) float64 {
	return HoverUnknownElapsedPct + share
}

// hoverShare is how far ahead of its own pace an account may run before the work
// belongs with a peer.
//
// Two reasons that is not simply the pool slice, and they are one reason said
// twice: restraint is worth nothing unless the quota it preserves is still there
// to be spent. hoverPoolShare says it once -- with one account there is nobody
// to hand to, so the share is the whole 100. This says it the other way round --
// with quota the rotation cannot reach before it expires, there is no LATER to
// hand it to, so the share is at least the part that would otherwise be thrown
// away.
//
// The LARGER of the two and not the sum, because they are two statements of one
// licence and adding them would license the same points twice. The maximum is
// also what keeps this inert in the ordinary case: on the fleet of 2026-08-25,
// three accounts one, three and five days from their resets strand 7.14 points
// against a pool share of 33.33, and an account already licensed to run 33
// points ahead needs no second licence for 7. Measured: the final-day spread
// TestHoverHoldsEveryAccountNearItsOwnPaceLine pins is unchanged to the digit.
func hoverShare(usable int, stranded float64) float64 {
	if pool := hoverPoolShare(usable); pool > stranded {
		return pool
	}
	return stranded
}

// hoverPreemptLead is how far ahead of a projected exhaustion to switch, taken
// from the gap the scheduler actually left rather than from a number in the
// file.
//
// The configured key exists because a user has to guess how long their next turn
// will be. Hover does not guess: the widest observed gap between a reading and
// the poll meant to follow it is how stale the decision will be by the time
// anything acts on it, and that is the quantity the lead is covering. It is
// self-correcting in the same direction the horizon is -- a 429 that stretches
// the cadence stretches the lead with it, and a tight cadence spends less quota
// getting out of the way early.
func hoverPreemptLead(pool []Candidate) time.Duration {
	widest := time.Duration(0)
	for _, c := range pool {
		// preemptHorizon carrying no lead of its own IS the observed blind
		// interval, and asking it rather than subtracting here inherits both of
		// its refusals. A reading with no FetchedAt has no provenance and yields
		// no gap: Sub against the zero time saturates at about 292 years, so
		// one such account would otherwise drag every OTHER account's lead
		// straight to the ceiling. And a next poll dated before the reading is a
		// clock that moved rather than a negative interval.
		gap, ok := preemptHorizon(c, 0)
		if !ok {
			continue
		}
		if gap > widest {
			widest = gap
		}
	}
	// No stamps at all lands on the floor, which is the right answer: nothing
	// has been observed, so the tightest cadence the policy runs at is the least
	// the lead can safely be.
	switch {
	case widest < HoverMinPreemptLead:
		return HoverMinPreemptLead
	case widest > HoverMaxPreemptLead:
		return HoverMaxPreemptLead
	}
	return widest
}

// HoverConfig is the anti-flap set hover imposes, and the one number it leaves
// alone. Each figure's rationale sits on the constant that declares it.
//
// preempt_lead is absent because it is not an anti-flap knob: it is a ranking
// input and travels on Options, where withHover puts the derived one.
//
// MaxAutoSpend is deliberately not touched, and that is the line this function
// exists to hold. Fully automatic must not become fully automatic SPENDING: the
// ceiling is one of the two independent opt-ins unattended overage requires, and
// an opt-in a mode supplies on the user's behalf is not an opt-in.
func HoverConfig(cfg Config) Config {
	cfg.HysteresisPct = HoverHysteresisPct
	cfg.HeadroomRatio = HoverHeadroomRatio
	cfg.Cooldown = HoverCooldown
	cfg.RecoveryHysteresis = HoverRecoveryHysteresis
	return cfg
}

// withHover installs a hover pass over cands when hover is on and one has not
// been installed already.
//
// Decide installs it once for the whole evaluation, because the anti-flap set it
// derives has to come from the same pool the ranking ran on. This is the guard
// for the callers that reach Rank directly: without it, Options.Hover would be a
// field that reads as "on" and silently changes nothing.
func (o Options) withHover(cands []Candidate) Options {
	if !o.Hover || o.hover != nil {
		return o
	}
	p := HoverThresholds(cands, o)
	o.hover = &p
	o.CreditThreshold = p.CreditThreshold
	o.PreemptLead = p.PreemptLead
	// The strategy is one of the keys hover overrides, and this is where that
	// happens. Consume-first spends the quota that expires soonest, and hover
	// carries that answer on the SHARE rather than in a mode of its own:
	// hoverShare widens an account's licence by exactly the quota its own
	// rotation cannot reach in time, so the perishable account leads the slack
	// order that the ranking, headroomGate and lessRecovery all already read.
	//
	// The wording here used to say the elapsed share already expressed this, and
	// it was wrong in the one direction that mattered. A window near its reset
	// does carry a high threshold -- and a high threshold is high SLACK, which
	// is what HeadroomFor's minimum throws away, so the perishable window was
	// the one window guaranteed never to bind. Writing the derivation out,
	// slack_w = share + (expected_w - util_w) with the share constant per
	// account, so argmin slack is argmax (util - expected): the binding window
	// is the one furthest AHEAD of pace, while a window with quota about to
	// expire is by definition the one furthest behind it. The subsumption was
	// inert in exactly the case it was claimed for.
	//
	// Measured on the fleet of 2026-09-05: four accounts separated by 1.327
	// points of five-hour pace, the account holding 99 points of a week that
	// expired in fifteen hours ranked LAST of four, and the account with ninety
	// hours left to spend its own ranked ahead of it. The licence has to travel
	// on the ACCOUNT, not on the window, or it never reaches the comparator at
	// all. TestHoverRaisesTheShareOfAnAccountItsRotationCannotDrain is that
	// pool, and TestHoverSwitchesToTheAccountWhoseWeekIsAboutToBeStranded is the
	// same pool through Decide -- which is the half a comparator-only fix leaves
	// red, because the additive margin measures the same slack the ranking does.
	//
	// The key is still overridden, and now for a reason that survives reading.
	// consume-first ranks on the reset INSTANT alone: it discards the binding
	// window, so it would hand the session to an account whose five-hour window
	// is empty because its week expires first, it ranks a week already spent
	// ahead of one that is not, and it sorts an account with no weekly reset
	// last -- which demotes a primary credit seat for having no week.
	o.Strategy = StrategyHeadroom
	return o
}

// thresholdsFor is the threshold table one candidate is measured against: the
// configured bundle, or the one hover derived for this account.
func (o Options) thresholdsFor(c Candidate) Thresholds {
	if o.hover != nil {
		return o.hover.For(c.UUID)
	}
	return o.Thresholds()
}

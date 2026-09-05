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
//	            floored at zero and capped by the room the account has
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
	// HoverDisplayCap is the highest threshold a HUMAN table prints. Nothing
	// clamps the derived figure itself any more; see the note below.
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
	// nobody to hand the work to, so nothing is being held back. The human table
	// still prints 100 as its ceiling, because a percentage above 100 reads as a
	// bug to everyone who has not read this comment; `--json` carries the true
	// figure, because a machine consumer checks slack against it.
	HoverDisplayCap = 100.0

	// HoverFallbackThreshold is what a window with no elapsed share gets: one
	// that reported no reset, and one whose reset is further out than the
	// window is long.
	//
	// It is the same 80 the configured default has always been, which is the
	// honest choice for a window hover knows nothing about -- it is what the
	// engine would have used before hover existed. A window that lands here
	// because it named no reset is also the window probe_unknown wakes, and
	// hover forces that key on, which is what removes the fallback.
	HoverFallbackThreshold = 80.0

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
// Stranded is the EFFECTIVE figure, after the cap, because that is the one that
// widened the share. Room is carried beside it so a reader can see whether the
// cap bound at all.
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
	// Room is the quota the account can actually serve, which is what caps
	// Stranded. See hoverRoom.
	Room float64
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

	// The CONFIGURED table decides what may bind, and hover decides what it is
	// worth. The two are not the same question. A weekly cap filed under a scope
	// key this build cannot name binds only where a threshold names it, and that
	// entry is CONSENT -- the user saying they know what the scope caps -- not a
	// tuning number. Hover overrides every number a user typed; it may not
	// supply an opt-in on their behalf, so it reads the table here rather than
	// its own, which is empty at this point in any case.
	configured := o.Thresholds()

	for _, c := range pool {
		if primaryCredit(c) {
			// A seat metered in credits carries no plan windows and no reset,
			// so there is nothing to pace and the plan windows it might still
			// report are not the meter it runs on -- the same reassignment
			// measure makes when it ranks such a seat.
			p.byUUID[c.UUID] = Thresholds{Default: HoverFallbackThreshold, Credit: HoverCreditThreshold}
			if pct, ok := c.Usage.ExtraUsage.Percent(); ok {
				p.Windows = append(p.Windows, HoverWindow{
					UUID:        c.UUID,
					Window:      creditWindow,
					Utilization: pct,
					Threshold:   HoverCreditThreshold,
					Slack:       HoverCreditThreshold - pct,
					Credit:      true,
				})
			}
			continue
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
		sw, stranded, hasStranded := hoverStranded(c.Usage, o.Model, configured, p.Usable, o.Now)
		share := hoverShare(p.Usable, stranded)
		acct := HoverAccount{
			UUID:        c.UUID,
			Share:       share,
			PoolShare:   hoverPoolShare(p.Usable),
			HasStranded: hasStranded,
			Stranded:    stranded,
			Room:        hoverRoom(c.Usage, o.Model, configured),
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
			row.ExpectedPct, row.HasExpected = usage.ExpectedPct(w.Name, w.Window, o.Now)
			row.Threshold = HoverFallbackThreshold
			if row.HasExpected {
				// Deliberately unclamped; HoverDisplayCap says why, and the
				// renderer is what holds the printed figure to 100.
				row.Threshold = row.ExpectedPct + share
			}
			// A window that named its reset and still has no share elapsed is a
			// clock problem, and spending a turn of the user's quota would not
			// fix it. Only the window nothing has ever spent against is marked.
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
			Default:   HoverFallbackThreshold,
			PerWindow: per,
			Credit:    HoverCreditThreshold,
		}
	}
	return p
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
	return Thresholds{Default: HoverFallbackThreshold, Credit: HoverCreditThreshold}
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
// the whole fleet at this one would strand the others. And it is capped by the
// room this account actually has.
func hoverStranded(s *usage.Snapshot, model string, t Thresholds, usable int, now time.Time) (usage.NamedWindow, float64, bool) {
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

	stranded := (100 - pct) - float64(usable)*(100-expected)
	if stranded < 0 {
		stranded = 0
	}
	// Capped by the room the account actually has. Hover prices the figure on
	// the perishable window's own terms; this is where it becomes a claim on the
	// NEXT SESSION, so this is where it has to answer "could this account serve
	// that session at all". An account with ten points left on the window no
	// model choice can dodge can absorb ten points of work, so ten points is the
	// most its licence may be widened by, however much of its week is about to
	// expire.
	//
	// Without it, measured: an account with 10 points of five-hour room and a
	// week 91% elapsed at 1% used scored a licence of 81, a five-hour pace
	// target of 171, and OUTRANKED an account holding 90 points -- and Decide
	// switched onto it. TestANearlyEmptyAccountCannotBuyTheFrontOfTheQueue.
	//
	// At zero room the figure is zero, which is the same instant OutOfQuota
	// files the account empty, so the two guards agree rather than merely
	// coexist. TestTheStrandedCapAndTheEmptyTierReadTheSameRoom pins that.
	if room := hoverRoom(s, model, t); stranded > room {
		stranded = room
	}
	if stranded < 0 {
		stranded = 0
	}
	return best, stranded, true
}

// hoverRoom is the least raw room among the windows a MODEL CHOICE CANNOT
// DODGE, falling back to the least room anywhere when none of those was
// readable, and zero for an account nothing could be read from.
//
// It is OutOfQuota's rule stated as a number rather than as a verdict, over the
// same window set, so the figure that caps a licence and the figure that files
// an account empty go to zero at the same instant. It exists as a second
// spelling of a rule HeadroomFor already applies, and a second spelling that
// drifts is how a blown Fable cap would come to zero a licence that OutOfQuota
// would not zero -- so the two are pinned together by a test rather than by
// this comment.
func hoverRoom(s *usage.Snapshot, model string, t Thresholds) float64 {
	minAny, minAll := 0.0, 0.0
	haveAny, haveAll := false, false
	for _, w := range bindingWindows(s, model, t) {
		pct, ok := w.Percent()
		if !ok {
			continue
		}
		room := 100 - pct
		if !haveAll || room < minAll {
			minAll, haveAll = room, true
		}
		if capsOneModelFamily(w.Name) {
			continue
		}
		if !haveAny || room < minAny {
			minAny, haveAny = room, true
		}
	}
	switch {
	case haveAny:
		return minAny
	case haveAll:
		return minAll
	}
	return 0
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

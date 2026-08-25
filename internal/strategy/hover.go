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
//	threshold = min(99, elapsed share of the window + 100 / usable accounts)
//
// Read it as a pace target. An account further through its quota than through
// its window, by more than its own slice of what is left, should hand the next
// session to a peer. With four accounts and a week 43% gone that is 68; with
// one account left it is the cap, because there is nobody to hand to and
// holding quota back buys nothing.

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
// user cannot audit is one they have to take on trust. `ccdad hover status`
// prints these verbatim.
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
// a Claude Code on this PATH. `ccdad hover status` layers those on top. The
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

	byUUID map[string]Thresholds
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
	share := hoverShare(p.Usable)

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
func hoverShare(usable int) float64 {
	if usable < 1 {
		usable = 1
	}
	return 100 / float64(usable)
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
	// happens. Consume-first spends the quota that expires soonest; hover has
	// already expressed that on the slack axis, because a window close to its
	// reset has a high elapsed share and therefore a high threshold. Ordering by
	// reset instant on top of that would discard every threshold hover just
	// derived and rank on a quantity none of them came from.
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

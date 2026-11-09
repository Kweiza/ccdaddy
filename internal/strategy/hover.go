package strategy

import (
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// hover is an ORDER, not a derivation.
//
// The owner's rules, in the order the ranking applies them:
//
//  1. Accounts with a five-hour window come first. Among them the one whose
//     week resets soonest is entered first, and the session then stays on it
//     until it reaches a limit: a five-hour account is never left for another
//     that merely holds more room.
//  2. Weekly-only accounts come next -- a verified Ultra seat reads five_hour
//     null -- and among them the session always goes to the one whose week
//     resets soonest.
//  3. The session returns to a five-hour account as soon as it has room again.
//  4. Every hand-off happens BEFORE a limit: one point before any window a
//     model choice cannot dodge, or earlier when the next reading would land
//     past it.
//
// tier.go is the order, departure.go is the one fact it has to remember, and
// the anti-flap gate's ModeHover case is where "stay until a limit" and the
// reset margin between weekly-only seats are held.
//
// It replaced a pace rotation that derived a threshold per account and window
// from how far through its window each account was and how many accounts
// shared the pool. That held four accounts level with their own clocks by
// moving the session two hundred times a week on the owner's fleet of
// 2026-11-09, and every move rewrites a running session's prompt cache.
// TestTodaysFleetOverAWeek carries both sets of figures. Its cost is stated
// there too and is the real one: a weekly-only seat is spent only once every
// five-hour account is at a limit, and then only the one whose week ends
// first, so on a fleet with several Ultra seats most of their weeks now expire
// unspent.

const (
	// HoverHandoffThreshold is the threshold hover holds every window to: the
	// last point the endpoint reports before the reading that says the account
	// has stopped.
	//
	// Hover has no pace line, so the only line room is measured against is the
	// one it hands off at, and that is what `ccdad status --json` prints beside
	// each window. It also fixes what the shared machinery means under hover:
	// ModeRecovery is every account Spent -- past this line on a window no
	// model choice can dodge, because hover's table does not bind on the others
	// (ModelCapsDodged) -- which is when soonest-recovery is the right question,
	// and a blown cap on one model family does not get it there.
	//
	// And it is positive, which admittedWindows relies on when it reads the
	// opt-in for a scope this build cannot name: a non-positive per-window entry
	// reads as "not opted in", and hover's table carries the user's opt-ins at
	// this line, so a non-positive one would silently revoke that consent.
	HoverHandoffThreshold = 100 - handoffRoomPct

	// HoverCreditThreshold is the figure a primary credit seat is measured on.
	//
	// A credit seat has no window and no reset, so there is no rollover to wait
	// for. 95 rather than the hand-off line because credits do not come back at
	// all: the last few points are the only warning a user gets that the seat
	// is nearly spent, where a subscription window's last few points are gone
	// in hours anyway.
	HoverCreditThreshold = 95.0

	// HoverCooldown is 2 minutes rather than the stock 5.
	//
	// It is the only hold on the pre-emptive move -- Decide answers pre-emption
	// before every margin but not before the cooldown -- so it is sized for that
	// move: two polls at the 60 s urgent cadence, so that a session handed on
	// can be handed on again before a second limit lands. It no longer has to
	// bound a rotation, which is what lets it be this short. The order does
	// that: a return onto an account left at a limit waits for that account's
	// window to roll, a move between two weekly-only seats needs one week to end
	// recovery_hysteresis sooner than the other, and a five-hour account is left
	// only at a limit. The out-and-back two minutes and one second apart that
	// this cooldown once allowed needs none of the three.
	HoverCooldown = 2 * time.Minute

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

// HoverPlan is one hover pass over one pool.
type HoverPlan struct {
	// CreditThreshold is what a primary credit seat is measured on.
	CreditThreshold float64
	// PreemptLead is derived from the observed poll gap; see hoverPreemptLead.
	PreemptLead time.Duration

	byUUID map[string]Thresholds
}

// HoverThresholds builds the table for one pool: every window an account can
// be stopped by, held to the hand-off line.
//
// It takes the pool AFTER quarantine rather than the whole store, because the
// pre-emptive lead is read off the poll gaps of the accounts the ranking will
// actually consider, and a quarantined account is not one of them.
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
	p.PreemptLead = hoverPreemptLead(pool)

	// The CONFIGURED table decides what may bind, and hover decides what it is
	// held to. The two are not the same question. A weekly cap filed under a
	// scope key this build cannot name binds only where a threshold names it,
	// and that entry is CONSENT -- the user saying they know what the scope
	// caps -- not a tuning number. Hover overrides every number a user typed;
	// it may not supply an opt-in on their behalf, so it reads the table here
	// rather than its own.
	configured := o.Thresholds()

	for _, c := range pool {
		// A primary credit seat gets its table like every other account. A seat
		// is filed KindCredit off Snapshot.HasSubscriptionWindows, which iterates
		// RateLimitWindows -- the fixed five and the two codex keys, and nothing
		// else -- so an account with no fixed windows, extra_usage enabled and a
		// real scoped weekly classifies as a credit seat AND carries quota.
		// Skipping seats here once left such a seat with no PerWindow at all, and
		// admittedWindows reads exactly that map to decide whether an unknown
		// scope was opted into, so every opt-in the user typed was dropped for
		// that one account. TestHoverDerivesATableForAPrimaryCreditSeatToo pins
		// the two window sets equal.
		//
		// The windows the configured table admits, so the windows hover holds
		// to a line are the ones the user's table admits -- cinder_cove
		// excluded, the opt-ins kept, and the model narrowing applied once
		// rather than twice. Which of them BIND is the hover table's own rule
		// (hoverTable).
		per := map[usage.WindowName]float64{}
		for _, w := range admittedWindows(c.Usage, o.Model, configured) {
			if _, ok := w.Percent(); !ok {
				// Nothing to compare a line against. This window cannot bind,
				// and a window hover holds to nothing is not one the ranking
				// measures: TestTheWeeklyResetIsReadFromTheSameTableTheHeadroomWas.
				continue
			}
			per[w.Name] = HoverHandoffThreshold
		}
		p.byUUID[c.UUID] = hoverTable(per)
	}
	return p
}

// For is the threshold table one account is measured against.
func (p HoverPlan) For(uuid string) Thresholds {
	if t, ok := p.byUUID[uuid]; ok {
		return t
	}
	// An account that was not in the pool this plan was built from -- one with
	// no reading at all, or one that arrived after the pass. Anything it later
	// reports is held to the same line as everything else.
	return hoverTable(nil)
}

// hoverTable is the table hover measures an account on: every window held to the
// hand-off line, a primary credit seat's meter to the credit threshold, and the
// windows a model choice can dodge left out of what binds (ModelCapsDodged, D5).
// per carries the user's opt-ins, at hover's line. It is one function so that the
// table an account was ranked on and the one an account outside the pass is
// answered with cannot differ in anything but those opt-ins.
func hoverTable(per map[usage.WindowName]float64) Thresholds {
	return Thresholds{
		Default:   HoverHandoffThreshold,
		PerWindow: per,
		Credit:    HoverCreditThreshold,
		ModelCaps: ModelCapsDodged,
	}
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

// HoverConfig is the anti-flap set hover imposes: the cooldown, and nothing
// else.
//
// hysteresis_pct, headroom_ratio and recovery_hysteresis are the user's again,
// because none of them is a pace knob any longer. The first two are reached only
// in ModeRecovery's far tier, where both accounts are past their last point and
// the stock figures mean what they mean outside hover. recovery_hysteresis is
// the margin on the reset axis, which is what hover moves weekly-only seats on,
// so a user who wants two Ultra seats to trade places only when one week ends
// an hour sooner can say so.
//
// preempt_lead is absent because it is not an anti-flap knob: it is a ranking
// input and travels on Options, where withHover puts the derived one.
//
// MaxAutoSpend is deliberately not touched, and that is the line this function
// exists to hold. Fully automatic must not become fully automatic SPENDING: the
// ceiling is one of the two independent opt-ins unattended overage requires, and
// an opt-in a mode supplies on the user's behalf is not an opt-in.
func HoverConfig(cfg Config) Config {
	cfg.Cooldown = HoverCooldown
	return cfg
}

// withHover installs a hover pass over cands when hover is on and one has not
// been installed already.
//
// Decide installs it once for the whole evaluation, because the pre-emptive lead
// it derives has to come from the same pool the ranking ran on. This is the guard
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
	// happens: consume-first's own case comes first in Rank, so left on it would
	// sort the whole pool on the reset instant and the tier order would never
	// run. Its answer is not lost -- lessHover compares the soonest reset inside
	// every rung.
	o.Strategy = StrategyHeadroom
	return o
}

// thresholdsFor is the threshold table one candidate is measured against: the
// configured bundle, or the one hover built for this account.
func (o Options) thresholdsFor(c Candidate) Thresholds {
	if o.hover != nil {
		return o.hover.For(c.UUID)
	}
	return o.Thresholds()
}

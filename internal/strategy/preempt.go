package strategy

import (
	"time"

	"github.com/Kweiza/ccdaddy/internal/pollpolicy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// The pre-emptive switch: the move that has to happen before the next reading,
// because there is not going to be one in time.
//
// Every other margin in this package compares two accounts AS THEY ARE. That
// comparison is worth nothing when the account in use will be at its hard limit
// before the next poll lands: hysteresis and the headroom ratio hold the engine
// on an account that has already stopped working, and the evidence arrives one
// poll interval late. The user's session is cut off in the middle of a turn.
//
// So the projection is evaluated FIRST, and it overrides both of those margins.
// It does not override the cooldown, which is what bounds a switch storm, and it
// cannot reach the credit pool at all, which is what keeps unattended spending
// behind its own two opt-ins. Neither of those is a freshness problem.

// preemptHorizon is how far ahead this decision has to see: the interval the
// engine is blind for, plus the lead.
//
// The interval comes from the two stamps the SCHEDULER wrote — when the reading
// was taken and when it means to take the next one — and not from the clock. So
// the horizon is the engine's real blind interval rather than a guess, and it is
// what makes this rule self-correcting: at the 60 s urgent cadence the horizon
// is 3 minutes and the switch happens late and close to the limit, wasting
// almost no quota; at the 1800 s ceiling a 429 imposes it is 32 minutes and the
// switch happens early, because polling is blocked and the session is not.
//
// A reading with no FetchedAt has no provenance and yields no horizon. The zero
// time is the year 1, and the arithmetic on it is not merely large, it is
// meaningless: Sub saturates at the largest Duration there is, about 292 years,
// and adding the lead to a saturated maximum wraps it negative — so the horizon
// lands centuries in the PAST and the rule silently switches itself off for the
// one account it was asked about. Answering "no" here says that out loud and
// hands the decision back to the ordinary margins.
//
// A zero NextPollAt is NOT the same condition — that is the scheduler saying
// "poll now", which is how the daemon's own due() reads it, so the blind
// interval is nothing and the lead is the whole horizon.
func preemptHorizon(c Candidate, lead time.Duration) (time.Duration, bool) {
	if c.FetchedAt.IsZero() {
		return 0, false
	}
	blind := time.Duration(0)
	if !c.NextPollAt.IsZero() {
		if blind = c.NextPollAt.Sub(c.FetchedAt); blind < 0 {
			// A next poll dated before the reading is a clock that moved, not a
			// negative interval.
			blind = 0
		}
	}
	return blind + lead, true
}

// runsOutBy reports whether a window projected to reach 100% at `at` does so by
// the deadline AND before it resets.
//
// A window that resets first is not about to stop the session, however steep the
// projection: the reset hands back a whole window before the session reaches its
// limit, and leaving it costs two switches -- out now, back at the rollover --
// for a stop that was never going to happen. Every projection the closing and
// carry tests make goes through this one rule, so the three of them cannot
// disagree about a window on the edge of its reset. A reset already in the past
// is a window that has rolled and whose refresh has not landed, which is lasting
// by the same rule. TestAWindowThatResetsBeforeItRunsOutIsNotClosing. The rule
// itself is usage.LastsToReset, the one PaceOf's projection reports too.
func runsOutBy(w usage.Window, at, deadline time.Time) bool {
	return !at.After(deadline) && stopsBeforeReset(w, at)
}

// stopsBeforeReset is whether a window spent at `at` is spent before it resets:
// runsOutBy without the deadline, for a projection that has no horizon of its
// own and wants only the instant.
func stopsBeforeReset(w usage.Window, at time.Time) bool {
	reset, hasReset := w.Reset()
	return !hasReset || !usage.LastsToReset(at, reset)
}

// atRate is when a window holding `room` points is spent at perMin, and whether
// that is anywhere at all: a rate of nothing spends nothing.
func atRate(now time.Time, room, perMin float64) (time.Time, bool) {
	minutes, ok := usage.MinutesAtRate(room, perMin)
	if !ok {
		return time.Time{}, false
	}
	return now.Add(time.Duration(minutes * float64(time.Minute))), true
}

// projectedExhaustion names the window of ws that reaches 100% soonest within
// horizon of now, before it resets, or "" when none does.
//
// It reads the window that RUNS OUT FIRST, which is deliberately not the window
// the ranking orders on. Ordering is on slack — the distance to a window's own
// configured threshold — while a session is ended by whichever window reaches
// 100% soonest, and the two are the same window only when the burn rates match.
// They never match: a five-hour window is 33.6 times shorter than a weekly one,
// so the same amount of work climbs it that much faster. Reading only the
// least-slack window leaves the engine sitting on an account whose five-hour cap
// lands in ten minutes because its weekly cap is nearer its threshold.
//
// ws is bindingWindows: the set the ranking measures, and on hover's table only
// the part of it no model choice can dodge. That is what keeps the widening
// honest -- a per-model weekly cap belonging to another family cannot pre-empt a
// session that will never touch it.
//
// It asks usage.PaceOf for each extrapolation rather than dividing here, so the
// engine and `ccdad status --json` read ONE projection and cannot come to two
// different answers about the same account. That includes the suppression: a
// window inside its first seventh has numbers too noisy to extrapolate from, and
// this reads that as "say nothing about this window", never as "not yet" and
// never as a reason to stop asking about the others.
func projectedExhaustion(ws []usage.NamedWindow, now time.Time, horizon time.Duration) usage.WindowName {
	deadline := now.Add(horizon)
	var first usage.WindowName
	var firstAt time.Time
	for _, w := range ws {
		proj, ok := usage.PaceOf(w.Name, w.Window, now).Projection()
		if !ok || !runsOutBy(w.Window, proj.ExhaustionAt, deadline) {
			continue
		}
		if first == "" || proj.ExhaustionAt.Before(firstAt) {
			first, firstAt = w.Name, proj.ExhaustionAt
		}
	}
	return first
}

// measuredExhaustion is projectedExhaustion's question asked at the rate the work
// was last MEASURED at on this account, on the window it was measured on.
//
// PaceOf's rate is the average since the window opened. On a five-hour window a
// session is most of that average; on a week it is diluted by days of nothing.
// Measured on the fleet of 2026-11-02, a week at 99% that a session was spending
// at 7.6 points an hour projected as an hour and twenty-three minutes from its
// limit, when it was eight. A verified Ultra seat has no shorter window to see the
// wall coming on, so the average is not good enough for this rule to read alone.
//
// It names the window the rate was measured on when it runs out, and "" when it
// does not -- including when that window is not one of ws, so a rate measured on
// a model-scoped cap projects nothing under hover.
func measuredExhaustion(c Candidate, ws []usage.NamedWindow, now time.Time, horizon time.Duration) usage.WindowName {
	if !c.HasBurn {
		return ""
	}
	for _, w := range ws {
		if w.Name != c.BurnWindow {
			continue
		}
		pct, ok := w.Percent()
		if !ok {
			return ""
		}
		if at, ok := atRate(now, 100-pct, c.BurnPerMin); ok && runsOutBy(w.Window, at, now.Add(horizon)) {
			return w.Name
		}
		return ""
	}
	return ""
}

// runsOutWithin names the window this account is projected to run out of inside
// the horizon on its OWN rates -- the window average and the burn last measured
// on it -- or "" when it is not. The average is asked first because it reads
// every window; the measured rate reads only the one it was taken on.
func (o Options) runsOutWithin(c Candidate, horizon time.Duration) usage.WindowName {
	ws := bindingWindows(c.Usage, o.Model, o.thresholdsFor(c))
	if w := projectedExhaustion(ws, o.Now, horizon); w != "" {
		return w
	}
	return measuredExhaustion(c, ws, o.Now, horizon)
}

// closingWindow is the configured strategies' projection: the window this
// account is projected to run out of inside its OWN horizon -- its blind interval
// plus preempt_lead -- or "" when none is. Hover asks a different question of a
// different horizon (runwayOf, liveLimitOf) and never this one.
//
// The live account is judged on its own rates, the window average and the burn
// last measured on it. Every other account is judged on its own rates AND on
// whether the work would run it out inside its horizon at the pool's maximum
// measured rate, which is the rule these strategies have always applied to a
// pre-emptive target: only one account is being spent at a time, so the maximum
// is the live account's rate, and the stale maximum still on the account just
// left is what keeps the walk off that account. Under them this field is read
// only as the walk's refusal. TestNonHoverPreemptionKeepsThePoolMaximum.
//
// A lead of zero switches pre-emption OFF, and that is an answer rather than an
// omission: config supplies the default, so a zero here means a user set
// preempt_lead to nothing on purpose. It is the same direction MaxAutoSpend
// takes, and for the same reason -- this is an opt-out somebody may
// legitimately want, not an anti-flap mechanism that must never be switched
// off.
//
// A reading with no provenance yields no horizon, and that is read as "cannot
// say" rather than "will run out": refusing on it would narrow the rule for the
// accounts it knows least about.
func (o Options) closingWindow(c Candidate, h Headroom) usage.WindowName {
	if o.PreemptLead <= 0 {
		return ""
	}
	horizon, ok := preemptHorizon(c, o.PreemptLead)
	if !ok {
		return ""
	}
	if w := o.runsOutWithin(c, horizon); w != "" || isActive(c.UUID, o.live) {
		return w
	}
	return o.work.carryRate().runsOut(c.Usage, h, o.Now, horizon)
}

// preemptTarget is where a pre-emptive move goes under the configured
// strategies: the best-ranked account that is not the live one and is somewhere
// the session can actually keep running. Hover makes its moves in hoverMove.
//
// It used to require POSITIVE SLACK, on the reasoning that an account already
// past one of its own window thresholds is not somewhere to run to. That holds
// for a threshold a person typed. It did not hold for the PACE target hover used
// to derive from how far through its window each account was, under which an
// ordinary pool was negative across the board. The measured consequence: on the
// pool of 2026-08-24 all six accounts were negative, so this rule, the one that
// exists to move a session before its account hits a hard limit, could not fire
// AT ALL while five accounts still held quota.
//
// Two tests replace it, each the narrowest statement of one thing that would
// waste the move:
//
//   - Not AT A LIMIT: Ranked.AtLimit, the one answer the order and the gate read
//     too. It covers an empty account, which is what positive slack was reaching
//     for; an account on its last point; and an account the work running now
//     would run out inside its own horizon -- moving from an account that
//     exhausts in five minutes to one that exhausts in six trades one cut-off
//     session for another and spends the cooldown doing it.
//   - Not one whose poller is throttled, PREFERABLY. A 429 on the usage endpoint
//     says the reading cannot be refreshed, never that the account is spent, so
//     it is a preference and not a filter: a throttled candidate is remembered
//     and returned when nothing cleaner turns up. A filter would hand this rule
//     a fresh way to fire never, which is the bug being fixed.
//
// An account nobody could read is skipped as well: nothing can be projected for
// it, so this rule, which acts on a projection, has nothing to send a session
// there on. The ordinary move still reaches one.
//
// It walks Result.Order and never Result.Credit, and that is how the credit gate
// stays out of this: a pre-emptive move cannot reach a credit account at all, so
// there is no gate here to bypass and none re-implemented. An engine that runs
// out of subscription room still opens the credit pool the ordinary way, through
// the gate, once the account it is on is actually spent.
func preemptTarget(byUUID map[string]Candidate, res Result, activeUUID string, o Options) (Ranked, bool) {
	var throttled Ranked
	hasThrottled := false
	for _, r := range res.Order {
		// The live account, reached in ranking order, ENDS the walk rather than
		// being skipped over.
		//
		// This rule exists to move somewhere BETTER before the session is cut
		// off. Everything after the live account in Result.Order is, by the
		// order's own definition, worse than it -- and moving to a worse account
		// does not buy the session anything: it is cut off there sooner, and the
		// engine has spent a switch to arrange that.
		//
		// Skipping instead of stopping is what made this rule flap. Nothing else
		// in the pass damps it: pre-emption is answered BEFORE the cooldown and
		// before every margin, on purpose, because an account about to run out
		// must not be held on by a hold. So with the live account already first,
		// this walk took second place, the ordinary better-target rule moved
		// back on the next tick, and the two alternated for as long as the
		// projection held. Measured on a live fleet: a switch every two minutes
		// -- the poll cadence -- for fifty-four minutes, sixty-five switches,
		// between the same two accounts.
		//
		// Staying is the right answer when nothing is better. The projection is
		// still true and the account will still run out; what is false is the
		// premise that somewhere else is preferable.
		if isActive(r.UUID, activeUUID) {
			break
		}
		if !r.Headroom.Known || r.AtLimit() != "" {
			continue
		}
		c, ok := byUUID[r.UUID]
		if !ok {
			// Ranked with no candidate behind it. Nothing can be projected for
			// it, so it is not somewhere this rule may send a session.
			continue
		}
		if recentlyRateLimited(c, o.Now) {
			if !hasThrottled {
				throttled, hasThrottled = r, true
			}
			continue
		}
		return r, true
	}
	if hasThrottled {
		return throttled, true
	}
	return Ranked{}, false
}

// recentlyRateLimited is whether this account's USAGE POLLER took a 429 inside
// the endpoint's own saturation horizon.
//
// pollpolicy owns that horizon and it is imported rather than restated: the span
// is a property of the endpoint the poller measured, and two copies would drift
// the moment one is retuned. The zero time is NEVER, which Sub would otherwise
// read as about two thousand years ago -- true, but only by accident, and the
// explicit test says which answer is meant.
func recentlyRateLimited(c Candidate, now time.Time) bool {
	if c.LastRateLimited.IsZero() {
		return false
	}
	return now.Sub(c.LastRateLimited) < pollpolicy.Recent429Window
}

// preempt is the whole rule, or the report that it does not fire: the live
// account's own Ranked.Closing, and the walk.
//
// find answers the question a candidate map cannot: whether the live account is
// in the pass at all, rather than held out of it by a quarantine. An empty
// activeUUID -- a live login nobody could attribute -- is never found, which is
// right: there is no baseline for any margin in that state and no burn to
// project either. And an account nobody could read is never closing, because
// both projections need a utilization to extend.
func preempt(byUUID map[string]Candidate, res Result, activeUUID string, o Options) (Ranked, bool) {
	if live, ok := find(res, activeUUID); !ok || live.Closing == "" {
		return Ranked{}, false
	}
	return preemptTarget(byUUID, res, activeUUID, o)
}

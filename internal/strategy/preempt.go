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

// projectedExhaustion reports whether any window that binds reaches 100% within
// horizon of now.
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
// The set is bindingWindows and not every window the response carried, and that
// is what keeps the widening honest: it is exactly the set the slack minimum is
// taken over, so a per-model weekly cap belonging to another family cannot
// pre-empt a session that will never touch it. That is why the thresholds come
// in rather than being defaulted here — the set depends on them, and a bundle
// built for convenience at this one call site would project over windows the
// ranking never ordered on, or miss one it did.
//
// It asks usage.PaceOf for each extrapolation rather than dividing here, so the
// engine and `ccdad status --json` read ONE projection and cannot come to two
// different answers about the same account. That includes the suppression: a
// window inside its first seventh has numbers too noisy to extrapolate from, and
// this reads that as "say nothing about this window", never as "not yet" and
// never as a reason to stop asking about the others.
func projectedExhaustion(s *usage.Snapshot, model string, now time.Time, horizon time.Duration, t Thresholds) bool {
	deadline := now.Add(horizon)
	for _, w := range bindingWindows(s, model, t) {
		proj, ok := usage.PaceOf(w.Name, w.Window, now).Projection()
		if !ok {
			continue
		}
		if !proj.ExhaustionAt.After(deadline) {
			return true
		}
	}
	return false
}

// preemptTarget is where a pre-emptive move goes: the best-ranked account that
// is not the live one and is somewhere the session can actually keep running.
//
// It used to require POSITIVE SLACK, on the reasoning that an account already
// past one of its own window thresholds is not somewhere to run to. That holds
// for a threshold a person typed. It does not hold for hover, which derives a
// threshold from how far through its window each account is -- a PACE target --
// and under which an ordinary pool is negative across the board. The measured
// consequence: on the pool of 2026-08-24 all six accounts were negative, so this
// rule, the one that exists to move a session before its account hits a hard
// limit, could not fire AT ALL while five accounts still held quota.
//
// Three tests replace it, each the narrowest statement of one thing that would
// waste the move:
//
//   - Not EMPTY. This is what positive slack was reaching for and is now sayable
//     directly. It is strictly weaker: any account with positive slack sorts
//     into the roomy tier ahead of every spent one, so wherever such a candidate
//     exists this still reaches it first. The change only adds candidates where
//     there were none.
//   - Not itself about to run out, asked with the SAME projection the live
//     account was judged by, over the candidate own blind interval. Moving from
//     an account that exhausts in five minutes to one that exhausts in six
//     trades one cut-off session for another and spends the cooldown doing it.
//   - Not one whose poller is throttled, PREFERABLY. A 429 on the usage endpoint
//     says the reading cannot be refreshed, never that the account is spent, so
//     it is a preference and not a filter: a throttled candidate is remembered
//     and returned when nothing cleaner turns up. A filter would hand this rule
//     a fresh way to fire never, which is the bug being fixed.
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
		if !r.Headroom.Known {
			continue
		}
		if empty, known := OutOfQuota(r.Headroom); known && empty {
			continue
		}
		c, ok := byUUID[r.UUID]
		if !ok {
			// Ranked with no candidate behind it. Nothing can be projected for
			// it, so it is not somewhere this rule may send a session.
			continue
		}
		// A candidate with no provenance yields no horizon, and that is read as
		// "cannot say" rather than "will run out": the emptiness test above has
		// already done the work that matters, and refusing here would narrow the
		// rule for the accounts it knows least about.
		if horizon, ok := preemptHorizon(c, o.PreemptLead); ok &&
			projectedExhaustion(c.Usage, o.Model, o.Now, horizon, o.thresholdsFor(c)) {
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

// preempt is the whole rule, or the report that it does not fire.
//
// o.PreemptLead of zero switches pre-emption OFF, and that is an answer rather
// than an omission: config supplies the 2 minute default, so a zero here means a
// user set preempt_lead to nothing on purpose. It is the same direction
// MaxAutoSpend takes, and for the same reason — this is an opt-out somebody may
// legitimately want, not an anti-flap mechanism that must never be switched off.
func preempt(byUUID map[string]Candidate, res Result, activeUUID string, o Options) (Ranked, bool) {
	if o.PreemptLead <= 0 {
		return Ranked{}, false
	}
	if activeUUID == "" {
		// The live login could not be attributed to a managed account. There is
		// no baseline for any margin in that state, and no burn to project
		// either.
		return Ranked{}, false
	}
	active, ok := byUUID[activeUUID]
	if !ok {
		return Ranked{}, false
	}
	ranked, ok := find(res, activeUUID)
	if !ok || !ranked.Headroom.Known {
		// find answers the question byUUID cannot: whether the live account is
		// in the pass at all, rather than held out of it by a quarantine. And an
		// account nobody could read has no burn to extend — that is already the
		// case the ordinary margins hand to a readable candidate, and inventing
		// a projection for it would be arithmetic on a number that does not
		// exist.
		return Ranked{}, false
	}
	horizon, ok := preemptHorizon(active, o.PreemptLead)
	if !ok {
		return Ranked{}, false
	}
	if !projectedExhaustion(active.Usage, o.Model, o.Now, horizon, o.Thresholds()) {
		return Ranked{}, false
	}
	return preemptTarget(byUUID, res, activeUUID, o)
}

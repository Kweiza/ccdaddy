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

// projectedExhaustion names the window that binds and reaches 100% soonest
// within horizon of now, or "" when none does.
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
func projectedExhaustion(s *usage.Snapshot, model string, now time.Time, horizon time.Duration, t Thresholds) usage.WindowName {
	deadline := now.Add(horizon)
	var first usage.WindowName
	var firstAt time.Time
	for _, w := range bindingWindows(s, model, t) {
		proj, ok := usage.PaceOf(w.Name, w.Window, now).Projection()
		if !ok || proj.ExhaustionAt.After(deadline) {
			continue
		}
		if first == "" || proj.ExhaustionAt.Before(firstAt) {
			first, firstAt = w.Name, proj.ExhaustionAt
		}
	}
	return first
}

// measuredExhaustion is projectedExhaustion's question asked at the rate the work
// was last MEASURED at, on the window it was measured on.
//
// PaceOf's rate is the average since the window opened. On a five-hour window a
// session is most of that average; on a week it is diluted by days of nothing.
// Measured on the fleet of 2026-11-02, a week at 99% that a session was spending
// at 7.6 points an hour projected as an hour and twenty-three minutes from its
// limit, when it was eight. A verified Ultra seat has no shorter window to see the
// wall coming on, so the average is not good enough for this rule to read alone.
//
// It reads the candidate's own rate rather than the session's, which is the
// question this rule asks: not how long the work would last there, which is
// CarriesFor, but whether this account is being run out right now. It names the
// window the rate was measured on when it is, and "" when it is not.
func measuredExhaustion(c Candidate, model string, horizon time.Duration, t Thresholds) usage.WindowName {
	if !c.HasBurn {
		return ""
	}
	for _, w := range bindingWindows(c.Usage, model, t) {
		if w.Name != c.BurnWindow {
			continue
		}
		pct, ok := w.Percent()
		if !ok {
			return ""
		}
		if minutes, ok := usage.MinutesAtRate(100-pct, c.BurnPerMin); ok && time.Duration(minutes*float64(time.Minute)) <= horizon {
			return w.Name
		}
		return ""
	}
	return ""
}

// runsOutWithin names the window this account is projected to run out of inside
// the horizon, on either rate, or "" when it is not projected to run out. The
// average is asked first because it reads every window; the measured rate reads
// only the one it was taken on.
func runsOutWithin(c Candidate, o Options, horizon time.Duration) usage.WindowName {
	t := o.thresholdsFor(c)
	if w := projectedExhaustion(c.Usage, o.Model, o.Now, horizon, t); w != "" {
		return w
	}
	return measuredExhaustion(c, o.Model, horizon, t)
}

// closingWindow is runsOutWithin over this account's OWN horizon: its blind
// interval plus the lead. measure asks it of every account, so the rule that
// leaves the live account and the walk that refuses a candidate read one
// answer, computed once, on one threshold table.
//
// A lead of zero switches pre-emption OFF, and that is an answer rather than an
// omission: config supplies the default, so a zero here means a user set
// preempt_lead to nothing on purpose. It is the same direction MaxAutoSpend
// takes, and for the same reason -- this is an opt-out somebody may
// legitimately want, not an anti-flap mechanism that must never be switched
// off. With it off nothing is closing, so nothing pre-empts and hover's order
// files no account at a limit on a projection.
//
// A reading with no provenance yields no horizon, and that is read as "cannot
// say" rather than "will run out": refusing on it would narrow the rule for the
// accounts it knows least about.
func closingWindow(c Candidate, o Options) usage.WindowName {
	if o.PreemptLead <= 0 {
		return ""
	}
	horizon, ok := preemptHorizon(c, o.PreemptLead)
	if !ok {
		return ""
	}
	return runsOutWithin(c, o, horizon)
}

// preemptTarget is where a pre-emptive move goes: the best-ranked account that
// is not the live one and is somewhere the session can actually keep running.
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
// Three tests replace it, each the narrowest statement of one thing that would
// waste the move:
//
//   - Not EMPTY. This is what positive slack was reaching for and is now sayable
//     directly. It is strictly weaker: any account with positive slack sorts
//     into the roomy tier ahead of every spent one, so wherever such a candidate
//     exists this still reaches it first. The change only adds candidates where
//     there were none.
//   - Not itself about to run out: Ranked.Closing, which is the SAME question
//     the live account was judged by, asked over the candidate's own blind
//     interval on the same table. Moving from an account that exhausts in five
//     minutes to one that exhausts in six trades one cut-off session for
//     another and spends the cooldown doing it.
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
	// The rate the work is running at, measured once for the walk. It is the
	// pool's maximum because only one account is being spent at a time -- see
	// SessionBurnPerMin.
	burn := measuredSessionBurn(poolOf(byUUID))
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
		//
		// Under hover the premise holds because of the order and not in spite
		// of it. A live five-hour account with room is FIRST in hover's order
		// and would end this walk before it began; but the projection that
		// brought the walk here is Ranked.Closing, and a closing account is on
		// the limit rung. So the accounts ahead of it are exactly the ones not
		// at a limit, and the walk needs no hover branch of its own.
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
		if r.Closing != "" {
			continue
		}
		// And the same question asked the other way round, because the one above
		// cannot answer it. projectedExhaustion reads THIS candidate's own pace,
		// and a candidate nobody is spending has none -- so an account a session
		// would empty in nine minutes passes that test on every fleet there is.
		//
		// This asks how long the account would carry the work running NOW, at the
		// rate that work was measured at, and refuses a target that cannot
		// survive the same horizon the live account is being pre-empted over.
		// Moving to an account that runs out inside the horizon spends a switch
		// to arrange the identical outcome a few minutes later.
		//
		// With no measured rate it never fires, so a fleet ccdad has read only
		// once behaves exactly as it did before the rate existed.
		if horizon, ok := preemptHorizon(c, o.PreemptLead); ok {
			perMin, applies := burn.onWindow(r.Headroom.MinAnyModelWindow)
			if carries, known := CarriesFor(r.Headroom, perMin, applies, horizon); known && !carries {
				continue
			}
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

// poolOf is the candidate map as a slice, for the readers that measure across
// the pool rather than looking one account up. Order is irrelevant to every one
// of them -- SessionBurnPerMin takes a maximum -- so the map's own is fine.
func poolOf(byUUID map[string]Candidate) []Candidate {
	out := make([]Candidate, 0, len(byUUID))
	for _, c := range byUUID {
		out = append(out, c)
	}
	return out
}

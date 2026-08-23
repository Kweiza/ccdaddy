package strategy

import (
	"time"

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

// projectedExhaustion reports whether one window's current burn reaches 100%
// within horizon of now.
//
// It asks usage.PaceOf for the extrapolation rather than dividing here, so the
// engine and `ccdad status --json` read ONE projection and cannot come to two
// different answers about the same account. That includes the suppression: a
// window inside its first seventh has numbers too noisy to extrapolate from, and
// this reads that as "no", never as "not yet".
func projectedExhaustion(s *usage.Snapshot, binding usage.WindowName, now time.Time, horizon time.Duration) bool {
	for _, w := range s.AllWindows() {
		if w.Name != binding {
			continue
		}
		proj, ok := usage.PaceOf(w.Name, w.Window, now).Projection()
		if !ok {
			return false
		}
		return !proj.ExhaustionAt.After(now.Add(horizon))
	}
	return false
}

// preemptTarget is where a pre-emptive move goes: the best-ranked account that
// is not the live one and still has slack under its own thresholds.
//
// Slack rather than headroom, because an account already past one of its own
// window thresholds is not somewhere to run to — that move trades one limit for
// another and spends the cooldown doing it.
//
// It walks Result.Order and never Result.Credit, and that is how the credit gate
// stays out of this: a pre-emptive move cannot reach a credit account at all, so
// there is no gate here to bypass and none re-implemented. An engine that runs
// out of subscription room still opens the credit pool the ordinary way, through
// the gate, once the account it is on is actually spent.
func preemptTarget(res Result, activeUUID string) (Ranked, bool) {
	for _, r := range res.Order {
		if isActive(r.UUID, activeUUID) {
			continue
		}
		if r.Headroom.Known && r.Headroom.Slack > 0 {
			return r, true
		}
	}
	return Ranked{}, false
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
		// An account nobody could read has no binding window and no burn. That
		// is already the case the ordinary margins hand to a readable candidate,
		// and inventing a projection for it would be arithmetic on a number that
		// does not exist.
		return Ranked{}, false
	}
	horizon, ok := preemptHorizon(active, o.PreemptLead)
	if !ok {
		return Ranked{}, false
	}
	if !projectedExhaustion(active.Usage, ranked.Headroom.Binding, o.Now, horizon) {
		return Ranked{}, false
	}
	return preemptTarget(res, activeUUID)
}

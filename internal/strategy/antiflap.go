package strategy

import (
	"sort"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// Spec §7.2, anti-flap, and the gate order §7.3 puts around it.
//
// The table is the easy half. The paragraph under it is the specification:
//
//	These bound the flap RATE; they do not make a reverse move impossible.
//	Headroom changes, so a target that burns down must be able to lose its
//	position.
//
// The naive implementation latches the chosen target until it is exhausted.
// That passes every anti-flap test anyone would write and fails the product,
// because an account burning four times faster than the one it displaced has to
// be able to lose its place. Nothing here latches: every mechanism is a MARGIN
// that a changing headroom can cross back over.

const (
	// DefaultHysteresisPct is §7.2's hysteresis margin, in percentage points of
	// headroom.
	//
	// The spec fixes the mechanism and names the config key but never fixes the
	// number, so it is chosen here. Ten points is the smallest margin that
	// actually does the job it is named for: the binding window can change
	// between two readings — an account whose five_hour and seven_day are a
	// point apart flips between them — and that alone moves headroom by a few
	// points with no usage at all. A margin under that noise floor lets the
	// engine ping-pong across the threshold on jitter, which is the one thing
	// this row exists to prevent.
	DefaultHysteresisPct = 10.0

	// DefaultHeadroomRatio is §7.2's 2.0. A candidate must have twice the
	// active account's headroom, which is what makes a REVERSE move need a 4x
	// relative burn: the forward move already required 2x in the other
	// direction, so the ratio between the two has to swing by 2x2. That
	// squaring is the whole point, and it falls out of applying the same 2.0 to
	// every move rather than out of remembering which account was left.
	DefaultHeadroomRatio = 2.0

	// DefaultCooldown is §7.2's 5 min.
	DefaultCooldown = 5 * time.Minute

	// DefaultRecoveryHysteresis is §7.2's 300 s. It is the margin on the
	// RECOVERY axis, for the all-above-threshold mode where §7.1 ranks by
	// soonest recovery: comparing headroom there would gate on a number the
	// order did not use.
	DefaultRecoveryHysteresis = 300 * time.Second

	// DefaultQuarantine is how long a dead refresh token holds an account out.
	//
	// The spec fixes no expiry, so it is chosen here. An hour matches
	// DefaultRecoveryHorizon and is short enough that a MISCLASSIFIED failure
	// costs an hour rather than a day; a genuinely dead token costs one wasted
	// refresh an hour, which is nothing. Only re-authentication actually fixes
	// a dead token, so this expiry is a safety valve and not a healing time —
	// see Quarantine.
	DefaultQuarantine = time.Hour
)

// Config is §7.2's table, as knobs.
//
// Every field defaults when it is not positive, so the ZERO VALUE of this
// struct is the full default set. That direction is deliberate: an
// under-populated Config must never read as "every anti-flap mechanism off",
// and §7.2 lists no way to switch one off at all. `ccdad config` (task 47)
// supplies the real values; until it does these are them.
type Config struct {
	// HysteresisPct is how many points of headroom a candidate must beat the
	// active account by.
	HysteresisPct float64
	// HeadroomRatio is the multiplicative margin on the same axis.
	HeadroomRatio float64
	// Cooldown is the minimum gap between two switches.
	Cooldown time.Duration
	// RecoveryHysteresis is how much sooner a candidate must recover.
	RecoveryHysteresis time.Duration
	// QuarantineFor is how long a dead refresh token holds an account out.
	QuarantineFor time.Duration
	// MaxAutoSpend is §7.3's ceiling, in the currency's major unit. Its default
	// is 0 — the explicit opt-in — so unlike every other field here a zero is
	// honoured rather than replaced.
	MaxAutoSpend float64
}

func (c Config) withDefaults() Config {
	if c.HysteresisPct <= 0 {
		c.HysteresisPct = DefaultHysteresisPct
	}
	if c.HeadroomRatio <= 0 {
		c.HeadroomRatio = DefaultHeadroomRatio
	}
	if c.Cooldown <= 0 {
		c.Cooldown = DefaultCooldown
	}
	if c.RecoveryHysteresis <= 0 {
		c.RecoveryHysteresis = DefaultRecoveryHysteresis
	}
	if c.QuarantineFor <= 0 {
		c.QuarantineFor = DefaultQuarantine
	}
	return c
}

// Action is what the engine should do.
type Action uint8

const (
	// ActionStay is "the world is already how it should be", exit 3.
	ActionStay Action = iota
	// ActionSwitch is a move to Plan.Target.
	ActionSwitch
	// ActionBlocked is "a move was wanted and there is no viable target",
	// exit 4. §9.3's 3-versus-4 line is the actionability line, and this is the
	// side the user has to do something about.
	ActionBlocked
)

func (a Action) String() string {
	switch a {
	case ActionStay:
		return "stay"
	case ActionSwitch:
		return "switch"
	case ActionBlocked:
		return "blocked"
	}
	return "unknown"
}

// Reason is why the engine answered as it did. Every value has a name because
// every one of them reaches a user through `ccdad auto --json` or a
// notification, and "no switch" with no reason is the cswap behaviour §9.3
// exists to fix.
type Reason uint8

const (
	// ReasonBetterTarget: a candidate cleared every margin.
	ReasonBetterTarget Reason = iota
	// ReasonAlreadyBest: the live account already tops the ranking.
	ReasonAlreadyBest
	// ReasonCooldown: a switch happened too recently.
	ReasonCooldown
	// ReasonHysteresis: the candidate is ahead, but not by the margin.
	ReasonHysteresis
	// ReasonHeadroomRatio: the candidate beat the margin but not the ratio.
	ReasonHeadroomRatio
	// ReasonRecoveryHysteresis: the candidate recovers sooner, but not by 300 s.
	ReasonRecoveryHysteresis
	// ReasonWeeklyResetMargin: consume-first's equivalent on the reset axis.
	ReasonWeeklyResetMargin
	// ReasonNoCandidates: nothing is eligible to rank at all.
	ReasonNoCandidates
	// ReasonAllQuarantined: everything eligible is held out by a quarantine.
	ReasonAllQuarantined
	// ReasonAllExhausted: every account is spent and there is no credit pool.
	ReasonAllExhausted
	// ReasonCreditGate: the credit pool was reached and §7.3 refused it. The
	// gate's own answer is in Plan.Credit.
	ReasonCreditGate
	// ReasonNoSubscriptionRoom: the engine is on a credit account and no
	// subscription account has room to go back to. Staying is right; moving to
	// a spent subscription account would break the session for nothing.
	ReasonNoSubscriptionRoom
)

func (r Reason) String() string {
	switch r {
	case ReasonBetterTarget:
		return "a better target cleared every margin"
	case ReasonAlreadyBest:
		return "already on the best account"
	case ReasonCooldown:
		return "a switch happened too recently"
	case ReasonHysteresis:
		return "the candidate is ahead by less than the hysteresis margin"
	case ReasonHeadroomRatio:
		return "the candidate does not have the required multiple of the headroom"
	case ReasonRecoveryHysteresis:
		return "the candidate does not recover enough sooner"
	case ReasonWeeklyResetMargin:
		return "the candidate's weekly quota does not expire enough sooner"
	case ReasonNoCandidates:
		return "no account is eligible for auto-rotation"
	case ReasonAllQuarantined:
		return "every eligible account is quarantined"
	case ReasonAllExhausted:
		return "every account is spent"
	case ReasonCreditGate:
		return "the credit gate refused"
	case ReasonNoSubscriptionRoom:
		return "no subscription account has room"
	}
	return "unknown"
}

// Plan is one engine evaluation.
type Plan struct {
	// Action is what to do.
	Action Action
	// Reason is why.
	Reason Reason
	// Target is where to move, set only when Action is ActionSwitch.
	Target Ranked
	// Result is the ranking this plan was made from. It covers the pool AFTER
	// quarantined accounts were removed, so the mode it reports is the mode the
	// decision was actually made in.
	Result Result
	// Quarantined lists the eligible accounts held out of the pass, in uuid
	// order.
	Quarantined []string
	// SubscriptionExhausted is §7.3 step 2's answer, reported so a caller can
	// say why the credit pool was or was not consulted.
	SubscriptionExhausted bool
	// Credit is the gate's answer, set only when the credit pool was reached.
	Credit Decision
	// CreditConsulted records that it was reached at all, because a refusing
	// Decision and a never-consulted one are both zero-ish and mean opposite
	// things.
	CreditConsulted bool
	// RetryAt is when the blocking condition lifts, when that is knowable — the
	// end of a cooldown, or the soonest quarantine expiry. It is advice for a
	// scheduler, never a promise that the answer changes then.
	RetryAt time.Time
	// HasRetryAt is whether RetryAt was set. A zero time is a real instant.
	HasRetryAt bool
}

// Decide is §7.2 and §7.3's gate order over one ranking pass.
//
// activeUUID must be the account in the LIVE CREDENTIALS FILE, derived by
// attributing that file against the stored credentials. It must NOT be
// store.ActiveUUID(): store.go documents that field as a display HINT, and it
// goes stale the moment the user runs /login inside Claude Code. Hysteresis
// measured against a stale baseline compares the candidate to an account that
// is not there, which is how an engine talks itself into a switch it already
// made. "" means the live login could not be attributed to a managed account,
// which is a real state and not an error: there is then no baseline to hold a
// margin against, and the margins that need one are skipped.
//
// st carries the persisted cooldown and quarantines. It is never written here —
// deciding is not switching, and a plan that is not executed must not leave a
// cooldown behind.
func Decide(cands []Candidate, o Options, cfg Config, st *State, activeUUID string) Plan {
	cfg = cfg.withDefaults()
	// The ceiling the pool is ORDERED on has to be the ceiling the gate DECIDES
	// on. Config is the half that comes from config.toml, so it is copied into
	// the pass rather than read from both places: ranking on one ceiling while
	// gating on another walks to a first choice the gate then refuses, with a
	// usable account waiting behind it.
	o.MaxAutoSpend = cfg.MaxAutoSpend
	if st == nil {
		st = NewState()
	}

	// Quarantine filters the pool BEFORE the ranking rather than rejecting a
	// winner afterwards. Rejecting afterwards would let a quarantined account
	// hold first place and stop the second-best from ever being considered —
	// and it would leave Result.Mode and SubscriptionExhausted answering about
	// a pool that includes an account nothing can use.
	pool := make([]Candidate, 0, len(cands))
	held := make([]string, 0)
	byUUID := make(map[string]Candidate, len(cands))
	for _, c := range cands {
		if !eligible(c) {
			continue
		}
		byUUID[c.UUID] = c
		if _, q := st.Quarantined(c.UUID, o.Now); q {
			held = append(held, c.UUID)
			continue
		}
		pool = append(pool, c)
	}
	sort.Strings(held)

	res := Rank(pool, o)
	subExhausted := SubscriptionExhausted(pool, o)
	plan := Plan{
		Result:                res,
		Quarantined:           held,
		SubscriptionExhausted: subExhausted,
	}

	if len(res.Order) == 0 && len(res.Credit) == 0 {
		plan.Action = ActionBlocked
		if len(held) > 0 {
			plan.Reason = ReasonAllQuarantined
			if at, ok := soonestQuarantineEnd(st, held); ok {
				plan.RetryAt, plan.HasRetryAt = at, true
			}
			return plan
		}
		plan.Reason = ReasonNoCandidates
		return plan
	}

	activeIsCredit := false
	for _, r := range res.Credit {
		if isActive(r.UUID, activeUUID) {
			activeIsCredit = true
			break
		}
	}

	// §7.3 step 1: the subscription pool first. The one situation where it is
	// skipped is an engine already on a credit account with no subscription
	// room to return to — moving there would end the session on a spent
	// account and buy nothing.
	if len(res.Order) > 0 && !(activeIsCredit && subExhausted) {
		best := res.Order[0]
		if !isActive(best.UUID, activeUUID) {
			if reason, blocked, retry := gate(res, best, activeUUID, cfg, st, o.Now); blocked {
				plan.Action = ActionStay
				plan.Reason = reason
				if !retry.IsZero() {
					plan.RetryAt, plan.HasRetryAt = retry, true
				}
				return plan
			}
			plan.Action = ActionSwitch
			plan.Reason = ReasonBetterTarget
			plan.Target = best
			return plan
		}
		// Already on the best subscription account. That is the end of it
		// unless the whole pool is spent, which is the only thing that opens
		// §7.3 step 2.
		if !subExhausted {
			plan.Action = ActionStay
			plan.Reason = ReasonAlreadyBest
			return plan
		}
	}

	if activeIsCredit {
		// Staying is not a switch, so the gate is not re-run against the
		// account already in use: §7.3 decides what may be SWITCHED TO.
		plan.Action = ActionStay
		plan.Reason = ReasonNoSubscriptionRoom
		return plan
	}

	// §7.3 steps 2-4: the credit pool, and only now.
	if len(res.Credit) == 0 {
		plan.Action = ActionBlocked
		plan.Reason = ReasonAllExhausted
		return plan
	}

	// Result.Credit is ordered by most armed room, uuid last, and this walks it
	// in that order — so the first account it reaches is the one with the most
	// money armed under §7.3's cap.
	var firstRefusal Decision
	for _, r := range res.Credit {
		c := byUUID[r.UUID]
		d := CreditGate(extraUsageOf(c), cfg.MaxAutoSpend, true)
		if !d.Allow {
			if !plan.CreditConsulted {
				firstRefusal = d
				plan.CreditConsulted = true
			}
			continue
		}
		plan.CreditConsulted = true
		plan.Credit = d
		// The cooldown, and ONLY the cooldown. §7.2's other margins are
		// comparisons between two accounts metered the same way, and a credit
		// account carries no plan windows at all: its headroom is permanently
		// unknown and it never recovers, so a headroom or recovery margin
		// against it would be arithmetic on a number that does not exist.
		// What makes this move safe is §7.3's gate, which has already run.
		if reason, blocked, retry := cooldownGate(res, activeUUID, cfg, st, o.Now); blocked {
			plan.Action = ActionStay
			plan.Reason = reason
			if !retry.IsZero() {
				plan.RetryAt, plan.HasRetryAt = retry, true
			}
			return plan
		}
		plan.Action = ActionSwitch
		plan.Reason = ReasonBetterTarget
		plan.Target = r
		return plan
	}

	plan.Action = ActionBlocked
	plan.Reason = ReasonCreditGate
	plan.Credit = firstRefusal
	return plan
}

// extraUsageOf is a candidate's credit axis. A candidate with no reading has an
// absent one, which the gate reads as unpriceable and refuses — the fail-closed
// answer on money.
func extraUsageOf(c Candidate) usage.ExtraUsage {
	if c.Usage == nil {
		return usage.ExtraUsage{}
	}
	return c.Usage.ExtraUsage
}

// soonestQuarantineEnd is when the first of the held accounts comes back.
func soonestQuarantineEnd(st *State, held []string) (time.Time, bool) {
	var out time.Time
	for _, uuid := range held {
		q, ok := st.data.Quarantine[uuid]
		if !ok || q.Until.IsZero() {
			continue
		}
		if out.IsZero() || q.Until.Before(out) {
			out = q.Until
		}
	}
	return out, !out.IsZero()
}

// gate applies the cooldown and then the margin for whichever axis the ranking
// actually ordered on. It reports the reason a move is refused, whether it is
// refused, and when the refusal lifts if that is knowable.
func gate(res Result, target Ranked, activeUUID string, cfg Config, st *State, now time.Time) (Reason, bool, time.Time) {
	if reason, blocked, retry := cooldownGate(res, activeUUID, cfg, st, now); blocked {
		return reason, blocked, retry
	}
	active, hasBaseline := find(res, activeUUID)
	if !hasBaseline {
		// Nothing to hold a margin against. cooldownGate has already said this
		// is not a state to sit in.
		return ReasonBetterTarget, false, time.Time{}
	}

	// Every caller reaches here with target = res.Order[0], so the ranking has
	// already put the target AHEAD of the active account. The tier rules below
	// lean on that: a tier difference is read as the target being in the better
	// tier, which is only true because the better tier sorts first.
	switch res.Mode {
	case ModeRecovery:
		// §7.1 tiers this mode: an account returning inside the horizon beats
		// one that does not, whatever its headroom. So a target that returns
		// inside the horizon got in front on the RECOVERY instant — whether it
		// outranked another near-tier account or a far-tier one — and that is
		// the axis the margin belongs on.
		//
		// The tier difference is deliberately NOT treated as categorical. Two
		// accounts a second apart either side of the horizon are a tier apart
		// and two seconds better, which is precisely the ping-pong the 300 s
		// margin exists to stop.
		if target.ReturnsInsideHorizon {
			if !active.HasRecovery {
				// The active account never said when it comes back. There is
				// no instant to hold a margin against, and a candidate that
				// named one is a better answer than silence.
				return ReasonBetterTarget, false, time.Time{}
			}
			if target.RecoversAt.After(active.RecoversAt.Add(-cfg.RecoveryHysteresis)) {
				return ReasonRecoveryHysteresis, true, time.Time{}
			}
			return ReasonBetterTarget, false, time.Time{}
		}
		// A target OUTSIDE the horizon can only have outranked an active
		// account that is also outside it, because the near tier sorts first.
		// §7.1 orders that tier on headroom, so the headroom margins apply.
		return headroomGate(active, target, cfg)

	case ModeConsumeFirst:
		// Ranked by soonest weekly reset, so the margin goes on that axis. The
		// spec fixes no number for it — consume-first is not in §7.2's table —
		// and RecoveryHysteresis is borrowed rather than a second knob
		// invented: it is the same question, "is this timestamp meaningfully
		// sooner", on a quantity that moves even more slowly.
		if target.HasWeeklyReset != active.HasWeeklyReset {
			return ReasonBetterTarget, false, time.Time{}
		}
		if target.HasWeeklyReset {
			if target.WeeklyResetsAt.After(active.WeeklyResetsAt.Add(-cfg.RecoveryHysteresis)) {
				return ReasonWeeklyResetMargin, true, time.Time{}
			}
		}
		return ReasonBetterTarget, false, time.Time{}

	default:
		return headroomGate(active, target, cfg)
	}
}

// cooldownGate is §7.2's 5-minute cooldown.
//
// It bounds churn BETWEEN TWO USABLE ACCOUNTS. With nothing usable underneath —
// the live login is not a managed account at all, or it is disabled, or it is
// quarantined — waiting is not caution, it is downtime, and it cannot storm:
// every move lands on an account that is eligible and not quarantined, so the
// next evaluation has a baseline again and the cooldown is back in force.
func cooldownGate(res Result, activeUUID string, cfg Config, st *State, now time.Time) (Reason, bool, time.Time) {
	if _, hasBaseline := find(res, activeUUID); !hasBaseline {
		return ReasonBetterTarget, false, time.Time{}
	}
	if left, cooling := st.CooldownRemaining(now, cfg.Cooldown); cooling {
		return ReasonCooldown, true, now.Add(left)
	}
	return ReasonBetterTarget, false, time.Time{}
}

// headroomGate is the pair of margins on the headroom axis: an additive one in
// points and a multiplicative one.
//
// The additive margin is what stops jitter near the threshold, where the
// binding window can flip between two windows a point apart and move headroom
// with no usage at all. The ratio is what makes a REVERSE move need a 4x
// relative burn: it applies to every move, so a round trip has to clear it
// twice in opposite directions.
//
// Neither margin latches. Both compare CURRENT headroom, so an account that
// burns down loses its position the moment the numbers say so — which is the
// paragraph under §7.2's table, and the thing a latch-until-exhausted
// implementation gets wrong while passing every test anyone would write for it.
func headroomGate(active, target Ranked, cfg Config) (Reason, bool, time.Time) {
	// No baseline figure to hold a margin against. Moving to an account we can
	// measure beats staying on one we cannot, and refusing here is how a pool
	// of unreadable accounts becomes a dead end (§7.2).
	if !active.Headroom.Known {
		return ReasonBetterTarget, false, time.Time{}
	}
	// The candidate is the unmeasurable one. It is ahead of the active account
	// only because §7.1 files "we have no idea" ahead of "we know it is spent",
	// and that is a maybe worth trying rather than a figure to compare.
	if !target.Headroom.Known {
		return ReasonBetterTarget, false, time.Time{}
	}
	if target.Headroom.Pct < active.Headroom.Pct+cfg.HysteresisPct {
		return ReasonHysteresis, true, time.Time{}
	}
	// With an active account at or past its limit the ratio is trivially
	// satisfied, which is correct: there is nothing left to be twice as much
	// as, and the additive margin above has already done the work.
	if target.Headroom.Pct < cfg.HeadroomRatio*active.Headroom.Pct {
		return ReasonHeadroomRatio, true, time.Time{}
	}
	return ReasonBetterTarget, false, time.Time{}
}

// isActive is the ONLY way to ask "is this the live account".
//
// "" is Decide's word for a live login that could not be attributed to a
// managed account, and it must never match anything — including an account
// whose own uuid is empty. A bare == would read such a candidate as the live
// one, which conjures a hysteresis baseline out of nothing and reports "already
// on the best account" for a machine that is on no managed account at all.
func isActive(uuid, activeUUID string) bool {
	return activeUUID != "" && uuid == activeUUID
}

// find locates an account in a ranking pass, subscription pool or credit pool.
func find(res Result, uuid string) (Ranked, bool) {
	for _, r := range res.Order {
		if isActive(r.UUID, uuid) {
			return r, true
		}
	}
	for _, r := range res.Credit {
		if isActive(r.UUID, uuid) {
			return r, true
		}
	}
	return Ranked{}, false
}

package strategy

import (
	"sort"
	"time"

	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// The ranking axis. This is the decision the whole product exists to make, and
// lessRecovery writes out the exact key shape because the obvious
// implementation is wrong.

const (
	// DefaultThreshold is the utilization percent above which an account counts
	// as spent. `ccdad config` supplies the real value; this is what stands in
	// when it has not.
	DefaultThreshold = 80.0

	// DefaultCreditThreshold is the utilization percent above which a
	// credit-metered account counts as spent.
	//
	// It matches DefaultThreshold because it answers the same question on a
	// different meter — a share of a credit balance rather than of a plan
	// window — and it is its own constant so that the two can be moved apart
	// without the credit axis silently inheriting a change made for
	// subscriptions.
	DefaultCreditThreshold = 80.0

	// DefaultRecoveryHorizon is how soon a spent account has to come back before
	// "it comes back soon" outranks "it is less blown".
	//
	// The 300 s recovery hysteresis has a fixed default; nothing fixes this
	// one, so it is chosen here. An hour matches the window the endpoint's own
	// request budget slides over (pollpolicy.Recent429Window), and it is about
	// the longest wait a user would sit through rather than switching by hand.
	// Past it, "when does it come back" stops being the useful question and how
	// badly an account is blown is the better discriminator — which is exactly
	// what the far tier ranks on.
	DefaultRecoveryHorizon = time.Hour
)

// timeValue is an optional time. A zero time is a real instant, so absence needs
// its own flag rather than being spelled with one.
type timeValue struct {
	at time.Time
	ok bool
}

// Strategy is which question the ranking asks.
type Strategy uint8

const (
	// StrategyHeadroom is the default: keep the engine on whichever account has
	// the most left.
	StrategyHeadroom Strategy = iota
	// StrategyConsumeFirst spends perishable weekly quota before it expires.
	StrategyConsumeFirst
)

func (s Strategy) String() string {
	switch s {
	case StrategyHeadroom:
		return "headroom"
	case StrategyConsumeFirst:
		return "consume-first"
	}
	return "unknown"
}

// StrategyNames lists every strategy a caller may name, in this type's own
// order. A CLI builds its help text and its error message from this, so a
// strategy added here cannot be forgotten in one of them.
func StrategyNames() []string {
	return []string{StrategyHeadroom.String(), StrategyConsumeFirst.String()}
}

// ParseStrategy is String's inverse. Unlike identity.ParseKind it does NOT fall
// back to a default: a name that reaches here came from a user typing it, and
// silently running the wrong strategy for a typo is the cswap behaviour the
// exit contract exists to fix.
func ParseStrategy(name string) (Strategy, bool) {
	for _, s := range []Strategy{StrategyHeadroom, StrategyConsumeFirst} {
		if s.String() == name {
			return s, true
		}
	}
	return StrategyHeadroom, false
}

// Mode is the situation the ranking actually found itself in — one of the three
// below. It is reported so `ccdad status` can say WHY an order looks the way it
// does.
type Mode uint8

const (
	// ModeHeadroom: at least one account has room, or some account could not be
	// read and might. Rank by most headroom.
	ModeHeadroom Mode = iota
	// ModeRecovery: every account is known to be over threshold. Rank by
	// soonest recovery, because sitting still burns the active account to a
	// hard limit while a peer that frees up in eight minutes is never tried.
	ModeRecovery
	// ModeConsumeFirst: the user asked for perishable quota to be spent first.
	ModeConsumeFirst
)

func (m Mode) String() string {
	switch m {
	case ModeHeadroom:
		return "headroom"
	case ModeRecovery:
		return "recovery"
	case ModeConsumeFirst:
		return "consume-first"
	}
	return "unknown"
}

// Candidate is one account the engine may rank. It is deliberately not a
// store.Account: ranking needs a handful of facts, and taking only those keeps
// this package independent of how accounts are persisted.
type Candidate struct {
	UUID     string
	Kind     identity.Kind
	Disabled bool
	// Primary marks a credit-metered account that is the seat's ordinary
	// metering rather than an overage: it ranks with the subscriptions instead
	// of waiting in the last-resort pool. Nothing sets it here; the flag is an
	// account fact, read off the store.
	Primary bool
	// Usage is the account's most recent reading, or nil when there has never
	// been one. Nil is UNKNOWN, never empty.
	Usage *usage.Snapshot
	// FetchedAt is when this reading was taken and NextPollAt when the next one
	// is due. Both are copied from usage.Entry, and the gap between them is the
	// interval during which the engine is blind: a projection that has to guess
	// it would be guessing the one number the cache already knows.
	FetchedAt  time.Time
	NextPollAt time.Time
}

// Options configures one ranking pass.
type Options struct {
	Now time.Time
	// Threshold is the utilization percent above which an account is spent. It
	// is the fallback for any window the table below does not name.
	Threshold float64
	// WindowThreshold is the threshold for a named window, for the windows that
	// have one of their own. A window missing from it — and nil, which is every
	// window missing — falls back to Threshold.
	//
	// It is keyed by the window name the wire uses, scoped names included, so
	// the map a user's configuration builds needs no translation table between
	// what was typed and what the snapshot reports.
	WindowThreshold map[usage.WindowName]float64
	// CreditThreshold is the utilization percent above which a credit-metered
	// account counts as spent.
	CreditThreshold float64
	// PreemptLead is how far ahead of a projected exhaustion the engine moves.
	//
	// A zero switches pre-emption OFF, and that is a real answer rather than an
	// omission: this is an opt-out a user may legitimately want, the same
	// direction MaxAutoSpend already takes, and unlike an anti-flap margin it is
	// not a mechanism that must never be switched off. The default lives in the
	// configuration, which is the only thing that knows a user did not say.
	PreemptLead time.Duration
	// Horizon is how soon a recovery has to be to win its tier.
	Horizon  time.Duration
	Strategy Strategy
	// Model is the model the session about to run will use, as the user typed
	// it. Empty is the unqualified pass — every window binds — and is what the
	// daemon and every reporting caller use. A named model narrows the ranking
	// to the windows that bind for it; see bindingWindows for the rule.
	//
	// It is a free string rather than a parsed family because the refusal for a
	// name ccdad cannot place belongs at the CLI, where there is a user to tell.
	// Reaching the engine, an unplaceable name simply narrows nothing.
	Model string
	// MaxAutoSpend is the credit gate's ceiling, and it is here for ONE reason:
	// the credit pool is ordered by armed room, and room cannot be computed
	// without it.
	//
	// It is the same number Config.MaxAutoSpend carries, and Decide copies it
	// from there rather than trusting a caller to set both — a pass that ranked
	// on one ceiling while the gate decided on another would walk to a first
	// choice the gate then refuses. 0 is the default and arms nothing, which
	// leaves the pool in the uuid order it had before this field existed.
	MaxAutoSpend float64
}

func (o Options) horizon() time.Duration {
	if o.Horizon <= 0 {
		return DefaultRecoveryHorizon
	}
	return o.Horizon
}

// Thresholds is this pass's thresholds as one value, which is the shape every
// consumer outside this package takes.
//
// The defaulting lives in Thresholds rather than here, so there is one place a
// zero can be turned into DefaultThreshold and no way for a caller to reach a
// comparison with a bare zero — which would make an account at one percent read
// as spent and open the credit gate.
func (o Options) Thresholds() Thresholds {
	return Thresholds{Default: o.Threshold, PerWindow: o.WindowThreshold, Credit: o.CreditThreshold}
}

// Ranked is a candidate with the figures the ranking used, so a caller can
// explain the order rather than only obey it.
type Ranked struct {
	UUID     string
	Kind     identity.Kind
	Headroom Headroom
	// RecoversAt is when the binding window rolls over.
	RecoversAt  time.Time
	HasRecovery bool
	// WeeklyResetsAt is the soonest weekly reset, which consume-first ranks on.
	WeeklyResetsAt time.Time
	HasWeeklyReset bool
	// ReturnsInsideHorizon is the tier bit of lessRecovery's tiered key.
	ReturnsInsideHorizon bool
	// CreditRoom is the armed spend the credit gate would allow, for a credit
	// account under the configured ceiling. It is the credit pool's ordering
	// key and is reported so `ccdad status` can explain that order rather than
	// only obey it.
	CreditRoom float64
	// HasCreditRoom is whether there is any. False covers every refusal the
	// gate would make — no ceiling configured, overage switched off, spend
	// unreadable, the armed cap spent — so it is not a claim that the account
	// has none, only that none can be armed right now.
	HasCreditRoom bool
}

// Result is one ranking pass.
type Result struct {
	// Order is the SUBSCRIPTION pool, best first.
	Order []Ranked
	// Credit is the credit accounts, in uuid order. They are not ranked here
	// and never appear in Order: a credit account is metered in money and
	// carries no plan windows, so its headroom is permanently unknown — which
	// would file it in the "we have no idea" tier, ahead of every account known
	// to be spent, and make the engine's best candidate the one that costs
	// money. A credit account is a last resort, and the gate, not this
	// comparator, is what decides whether one may be used.
	Credit []Ranked
	// AllOverThreshold is true only when every SUBSCRIPTION candidate is KNOWN
	// to be over threshold. An account that could not be read makes it false:
	// it is neither over nor under, and letting one expired token decide this
	// is how cswap's engine came to park itself permanently. Credit accounts
	// are excluded, or a registered credit account's permanently unknown
	// headroom would pin this to false and make ModeRecovery unreachable.
	AllOverThreshold bool
	Mode             Mode
}

// eligible drops the accounts that cannot be ranked at all, BEFORE any ordering
// happens. KindAPIKey has no quota concept, so there is nothing to compare it
// on; a disabled account is held out of auto-rotation by the user.
//
// KindCredit IS eligible — it is a real switch target — but it is ranked on the
// credit axis rather than on headroom; see Result.Credit.
func eligible(c Candidate) bool {
	return !c.Disabled && c.Kind != identity.KindAPIKey
}

// Spent is whether an account is past a threshold on any window it carries.
//
// One test covers them all: a negative slack means the binding window is over
// the threshold that window was given, and the binding window is the one with
// the least slack, so every other window has slack to spare.
//
// It is three-valued on purpose. An account that could not be read is neither
// spent nor unspent, and folding that into a boolean is the bug that left cswap
// parked on the account that reset last.
func Spent(h Headroom) (spent, known bool) {
	if !h.Known {
		return false, false
	}
	return h.Slack < 0, true
}

// weeklyResetOf is the soonest reset among the weekly windows, which is what
// consume-first spends against.
//
// It reads the same narrowed set the headroom does, so --model means one thing
// in both strategies: a session that named Sonnet does not go chasing an Opus
// cap's expiry, because that is not quota it is going to spend.
func weeklyResetOf(s *usage.Snapshot, model string) timeValue {
	var out timeValue
	for _, w := range bindingWindows(s, model) {
		if !usage.IsWeekly(w.Name) {
			continue
		}
		at, ok := w.Reset()
		if !ok {
			continue
		}
		if !out.ok || at.Before(out.at) {
			out = timeValue{at: at, ok: true}
		}
	}
	return out
}

func measure(c Candidate, o Options) Ranked {
	h := HeadroomFor(c.Usage, o.Model, o.Thresholds())
	// Recovery is the moment the account stops being spent, and that is not
	// always the binding window's rollover. A blown WEEKLY window still holds the
	// account back after the five-hour window has come back, so when there is one
	// it is that floor which has to clear.
	clears := h.Binding
	if h.HasFloor {
		clears = h.Floor
	}
	rec := recoveryOf(c.Usage, clears)
	weekly := weeklyResetOf(c.Usage, o.Model)

	r := Ranked{
		UUID:           c.UUID,
		Kind:           c.Kind,
		Headroom:       h,
		RecoversAt:     rec.at,
		HasRecovery:    rec.ok,
		WeeklyResetsAt: weekly.at,
		HasWeeklyReset: weekly.ok,
	}
	// A binding window with no reset cannot be said to return inside the
	// horizon. Treating "no answer" as "back immediately" would put the least
	// knowable account at the front of the queue.
	r.ReturnsInsideHorizon = rec.ok && !rec.at.After(o.Now.Add(o.horizon()))
	return r
}

// Rank orders the eligible accounts, best first. It does not reorder its input.
func Rank(cands []Candidate, o Options) Result {
	measured := make([]Ranked, 0, len(cands))
	credit := make([]Ranked, 0)
	// True until some SUBSCRIPTION candidate turns out not to be known-and-over.
	// An account that could not be read counts against it: it is neither over
	// nor under, and letting one expired token answer this is how cswap's engine
	// came to park itself permanently.
	allOver := true
	for _, c := range cands {
		if !eligible(c) {
			continue
		}
		r := measure(c, o)
		if c.Kind == identity.KindCredit {
			r.CreditRoom, r.HasCreditRoom = armedRoom(c, o.MaxAutoSpend)
			credit = append(credit, r)
			continue
		}
		measured = append(measured, r)

		if over, known := Spent(r.Headroom); !known || !over {
			allOver = false
		}
	}
	sort.SliceStable(credit, func(i, j int) bool { return lessCredit(credit[i], credit[j]) })

	res := Result{Order: measured, Credit: credit, AllOverThreshold: allOver}
	switch {
	case o.Strategy == StrategyConsumeFirst:
		res.Mode = ModeConsumeFirst
		sort.SliceStable(measured, func(i, j int) bool { return lessConsumeFirst(measured[i], measured[j]) })
	case allOver && len(measured) > 0:
		res.Mode = ModeRecovery
		sort.SliceStable(measured, func(i, j int) bool { return lessRecovery(measured[i], measured[j]) })
	default:
		res.Mode = ModeHeadroom
		sort.SliceStable(measured, func(i, j int) bool { return lessHeadroom(measured[i], measured[j]) })
	}
	return res
}

// headroomTier separates three kinds of candidate, because "we know it has room"
// and "we have no idea" and "we know it is spent" are three answers, not two.
// The unknown sits between: a maybe beats a no, and trying it is how a pool of
// unreadable accounts stops being a dead end.
func headroomTier(r Ranked) int {
	over, known := Spent(r.Headroom)
	switch {
	case known && !over:
		return 0
	case !known:
		return 1
	default:
		return 2
	}
}

func lessHeadroom(a, b Ranked) bool {
	if ta, tb := headroomTier(a), headroomTier(b); ta != tb {
		return ta < tb
	}
	// Within a tier, more SLACK first — distance from each account's own
	// threshold rather than from a shared 100. An unknown tier has no slack to
	// compare, so both sides are zero and the uuid decides.
	if a.Headroom.Known && b.Headroom.Known && a.Headroom.Slack != b.Headroom.Slack {
		return a.Headroom.Slack > b.Headroom.Slack
	}
	return a.UUID < b.UUID
}

// lessRecovery is the tiered recovery key:
//
//	{0, recoveryTS, -slack}  // returns inside the horizon
//	{1, -slack, recoveryTS}  // does not
//
// The tier is what makes it work. A flat key compares a raw slack against an
// epoch second — 0-100 against ~1.7e9 — so magnitude decides and the engine
// parks on whatever resets last.
//
// The quantity is SLACK rather than Pct so that both modes order on one axis.
// Ordering on the display axis while the mode switch and the margins run on the
// decision axis is the seam this whole change removes; at a single threshold the
// two differ by a constant and the choice is invisible, and the moment a weekly
// window carries a threshold of its own it is slack that says which account is
// closer to its own floor.
func lessRecovery(a, b Ranked) bool {
	if a.ReturnsInsideHorizon != b.ReturnsInsideHorizon {
		return a.ReturnsInsideHorizon
	}
	if a.ReturnsInsideHorizon {
		if !a.RecoversAt.Equal(b.RecoversAt) {
			return a.RecoversAt.Before(b.RecoversAt)
		}
		if a.Headroom.Slack != b.Headroom.Slack {
			return a.Headroom.Slack > b.Headroom.Slack
		}
		return a.UUID < b.UUID
	}
	if a.Headroom.Slack != b.Headroom.Slack {
		return a.Headroom.Slack > b.Headroom.Slack
	}
	// An account with no recovery at all sorts after one that has a distant
	// recovery: "sometime" is still more than "never said".
	if a.HasRecovery != b.HasRecovery {
		return a.HasRecovery
	}
	if a.HasRecovery && !a.RecoversAt.Equal(b.RecoversAt) {
		return a.RecoversAt.Before(b.RecoversAt)
	}
	return a.UUID < b.UUID
}

// lessCredit orders the credit pool by MOST ARMED ROOM, which is the money
// analogue of most headroom, with the uuid as the final key.
//
// The uuid tie-break is not decoration: two accounts with identical room must
// not reorder between two ticks, or the engine switches, finds a new "best" on
// the next pass and switches back — and the cooldown never converges. An
// account with no armed room sorts behind every account that has some, whatever
// its uuid, because Decide walks this order and the first account it can
// actually use should be the first one it reaches.
func lessCredit(a, b Ranked) bool {
	if a.HasCreditRoom != b.HasCreditRoom {
		return a.HasCreditRoom
	}
	if a.HasCreditRoom && a.CreditRoom != b.CreditRoom {
		return a.CreditRoom > b.CreditRoom
	}
	return a.UUID < b.UUID
}

// armedRoom is what the credit gate would arm for this account under ceiling.
//
// It goes through CreditGate rather than through CreditRoom directly, so the
// order agrees with the decision by construction: every refusal the gate makes
// — the account's own overage switch, the organization's block, an unreadable
// spend, an invalid ceiling — lands here as "no room" without this file
// re-stating any of them. subscriptionExhausted is true because this asks what
// the gate WOULD say when it is the credit pool's turn; whether it is that
// pool's turn is Decide's question, not the comparator's.
func armedRoom(c Candidate, ceiling float64) (float64, bool) {
	var e usage.ExtraUsage
	if c.Usage != nil {
		e = c.Usage.ExtraUsage
	}
	d := CreditGate(e, ceiling, true)
	if !d.Allow {
		return 0, false
	}
	return d.Room, true
}

// lessConsumeFirst ranks by SOONEST weekly reset. Weekly quota is perishable —
// use it or lose it — and that is the opposite direction from headroom, so it
// cannot be folded into the same comparator by flipping a sign.
func lessConsumeFirst(a, b Ranked) bool {
	if a.HasWeeklyReset != b.HasWeeklyReset {
		return a.HasWeeklyReset
	}
	if a.HasWeeklyReset && !a.WeeklyResetsAt.Equal(b.WeeklyResetsAt) {
		return a.WeeklyResetsAt.Before(b.WeeklyResetsAt)
	}
	return a.UUID < b.UUID
}

// SubscriptionExhausted answers step 2 of the credit gate: may the credit pool
// be considered at all?
//
// Only subscription accounts count. An account that could not be read holds the
// pool OPEN — it is not known to be spent, and the fail-closed direction on
// money is to keep using quota that is already paid for rather than to start
// spending because one poll failed. A pool with no subscription accounts in it
// is exhausted trivially.
func SubscriptionExhausted(cands []Candidate, o Options) bool {
	subs := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if eligible(c) && c.Kind == identity.KindSubscription {
			subs = append(subs, c)
		}
	}
	if len(subs) == 0 {
		return true
	}
	for _, c := range subs {
		over, known := Spent(HeadroomFor(c.Usage, o.Model, o.Thresholds()))
		if !known || !over {
			return false
		}
	}
	return true
}

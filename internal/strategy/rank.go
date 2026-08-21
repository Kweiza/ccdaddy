package strategy

import (
	"sort"
	"time"

	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// Spec §7.1, the ranking axis. This is the decision the whole product exists to
// make, and the spec writes out the exact key shape because the obvious
// implementation is wrong.

const (
	// DefaultThreshold is the utilization percent above which an account counts
	// as spent. Task 47's `ccdad config` supplies the real value; this is the
	// value used until it does.
	DefaultThreshold = 80.0

	// DefaultRecoveryHorizon is how soon a spent account has to come back before
	// "it comes back soon" outranks "it is less blown".
	//
	// The spec fixes the 300 s recovery hysteresis but never fixes this, so it
	// is chosen here. An hour matches the window the endpoint's own request
	// budget slides over (§7.4's recent429Window), and it is about the longest
	// wait a user would sit through rather than switching by hand. Past it,
	// "when does it come back" stops being the useful question and how badly an
	// account is blown is the better discriminator — which is exactly what the
	// far tier ranks on.
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

// Mode is the situation the ranking actually found itself in (§7.1's three
// rows). It is reported so `ccdad status` can say WHY an order looks the way it
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
// store.Account: ranking needs four facts, and taking only those keeps this
// package independent of how accounts are persisted.
type Candidate struct {
	UUID     string
	Kind     identity.Kind
	Disabled bool
	// Usage is the account's most recent reading, or nil when there has never
	// been one. Nil is UNKNOWN, never empty.
	Usage *usage.Snapshot
}

// Options configures one ranking pass.
type Options struct {
	Now time.Time
	// Threshold is the utilization percent above which an account is spent.
	Threshold float64
	// Horizon is how soon a recovery has to be to win its tier.
	Horizon  time.Duration
	Strategy Strategy
}

func (o Options) horizon() time.Duration {
	if o.Horizon <= 0 {
		return DefaultRecoveryHorizon
	}
	return o.Horizon
}

// threshold defaults the same way Horizon does, and it matters more.
//
// A zero Threshold read literally means "over threshold if utilization > 0", so
// an account with a single percent used counts as spent — which flows straight
// into SubscriptionExhausted, the input that opens the credit gate. The zero
// value of this struct would therefore fail OPEN on money, against everything
// §7.3 stands for. Defaulting it makes the omission harmless instead.
func (o Options) threshold() float64 {
	if o.Threshold <= 0 {
		return DefaultThreshold
	}
	return o.Threshold
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
	// ReturnsInsideHorizon is the tier bit of §7.1's key.
	ReturnsInsideHorizon bool
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
	// money. §7.3 calls credit accounts a last resort, and the gate, not this
	// comparator, is what decides whether one may be used.
	Credit []Ranked
	// AllOverThreshold is true only when every SUBSCRIPTION candidate is KNOWN
	// to be over threshold. An account that could not be read makes it false:
	// it is neither over nor under, and letting one expired token decide this
	// is how cswap's engine came to park itself permanently. Credit accounts
	// are excluded, or a registered credit account's permanently unknown
	// headroom would pin this to false and make §7.1's recovery situation
	// unreachable.
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

// overThreshold is three-valued on purpose. An account that could not be read is
// neither over nor under, and folding that into a boolean is §7.2's named bug.
func overThreshold(h Headroom, threshold float64) (over, known bool) {
	if !h.Known {
		return false, false
	}
	return 100-h.Pct > threshold, true
}

// weeklyResetOf is the soonest reset among the seven-day windows, which is what
// consume-first spends against.
func weeklyResetOf(s *usage.Snapshot) timeValue {
	var out timeValue
	if s == nil {
		return out
	}
	for _, w := range s.RateLimitWindows() {
		switch w.Name {
		case usage.WindowSevenDay, usage.WindowSevenDayOAuthApps,
			usage.WindowSevenDayOpus, usage.WindowSevenDaySonnet:
		default:
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
	h := HeadroomOf(c.Usage)
	rec := recoveryOf(c.Usage, h.Binding)
	weekly := weeklyResetOf(c.Usage)

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
			credit = append(credit, r)
			continue
		}
		measured = append(measured, r)

		if over, known := overThreshold(r.Headroom, o.threshold()); !known || !over {
			allOver = false
		}
	}
	sort.SliceStable(credit, func(i, j int) bool { return credit[i].UUID < credit[j].UUID })

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
		sort.SliceStable(measured, func(i, j int) bool { return lessHeadroom(measured[i], measured[j], o.threshold()) })
	}
	return res
}

// headroomTier separates three kinds of candidate, because "we know it has room"
// and "we have no idea" and "we know it is spent" are three answers, not two.
// The unknown sits between: a maybe beats a no, and trying it is how a pool of
// unreadable accounts stops being a dead end.
func headroomTier(r Ranked, threshold float64) int {
	over, known := overThreshold(r.Headroom, threshold)
	switch {
	case known && !over:
		return 0
	case !known:
		return 1
	default:
		return 2
	}
}

func lessHeadroom(a, b Ranked, threshold float64) bool {
	if ta, tb := headroomTier(a, threshold), headroomTier(b, threshold); ta != tb {
		return ta < tb
	}
	// Within a tier, more headroom first. An unknown tier has no headroom to
	// compare, so both sides are zero and the uuid decides.
	if a.Headroom.Known && b.Headroom.Known && a.Headroom.Pct != b.Headroom.Pct {
		return a.Headroom.Pct > b.Headroom.Pct
	}
	return a.UUID < b.UUID
}

// lessRecovery is §7.1's tiered key:
//
//	{0, recoveryTS, -headroom}  // returns inside the horizon
//	{1, -headroom, recoveryTS}  // does not
//
// The tier is what makes it work. A flat key compares a raw headroom against an
// epoch second — 0-100 against ~1.7e9 — so magnitude decides and the engine
// parks on whatever resets last.
func lessRecovery(a, b Ranked) bool {
	if a.ReturnsInsideHorizon != b.ReturnsInsideHorizon {
		return a.ReturnsInsideHorizon
	}
	if a.ReturnsInsideHorizon {
		if !a.RecoversAt.Equal(b.RecoversAt) {
			return a.RecoversAt.Before(b.RecoversAt)
		}
		if a.Headroom.Pct != b.Headroom.Pct {
			return a.Headroom.Pct > b.Headroom.Pct
		}
		return a.UUID < b.UUID
	}
	if a.Headroom.Pct != b.Headroom.Pct {
		return a.Headroom.Pct > b.Headroom.Pct
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

// SubscriptionExhausted answers §7.3 step 2: may the credit pool be considered
// at all?
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
		over, known := overThreshold(HeadroomOf(c.Usage), o.threshold())
		if !known || !over {
			return false
		}
	}
	return true
}

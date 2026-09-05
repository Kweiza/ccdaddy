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
	// Elsewhere marks an account another machine's ccdad owns. It is ranked out
	// exactly like Disabled, and it is a separate field for the same reason it
	// is a separate flag on the account: the two answer different questions, and
	// a listing that folds them prints the wrong reason beside the row.
	Elsewhere bool
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
	// Probe is what the last warm-up of this account did, copied from
	// usage.Entry for the same reason the two stamps above are: `ccdad hover
	// status` renders what the warm-up loop would do about each row, and a table
	// that derived that from anything but the daemon's own inputs would be a
	// second implementation of the gate, free to drift from the one that spends
	// the quota.
	Probe usage.ProbeState
	// LastRateLimited is when the USAGE POLLER last took a 429 for this account,
	// zero for never.
	//
	// It is not evidence that the account is out of quota, and nothing here may
	// treat it as such. The throttle is on /api/oauth/usage, and the sharper of
	// the two experiments on record found a Retry-After of 0 scoped to the
	// ACCESS TOKEN rather than to the account -- so a 429 here can be an
	// artefact of the token ccdad happens to be polling with. Filing a throttled
	// account as spent is the cswap failure this package warns about in three
	// other places: unreadable is not empty.
	//
	// What it does mean is that the reading will not be refreshed soon: the poll
	// policy floors the cadence at Post429MinInterval and lets it climb to
	// Post429MaxInterval. So it is read in exactly one place -- the pre-emptive
	// switch, which is the rule that acts on a PROJECTION rather than on a
	// figure, and which therefore has something to lose by running to an account
	// whose numbers it cannot check.
	LastRateLimited time.Time
	// BurnPerMin is the rate measured across this account's last two readings,
	// in points of its binding window a minute, and HasBurn whether one could be
	// taken at all. Both are copied from usage.PollState.
	//
	// It is a fact about the SESSION rather than about the account, and that is
	// the whole of how it must be read. An account nobody is spending measures
	// zero; the moment a session moves onto it, it burns at whatever rate that
	// session burns. So the figure that answers "how long would this candidate
	// last" is the LIVE account's rate applied to the candidate's room -- never
	// the candidate's own, which is zero for every account the engine might move
	// to and would report that each of them lasts forever.
	//
	// HasBurn false is "cannot say" and never "nothing is being spent". A
	// measured idle account carries zero with HasBurn true, and the two send the
	// engine down different arms: no measurement leaves every rule exactly as it
	// was before this field existed.
	BurnPerMin float64
	HasBurn    bool
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
	// Hover replaces every threshold in this struct with one derived per account
	// and per window. It is a mode rather than a knob, so it is a plain bool with
	// no default to fall back to.
	Hover bool
	// hover is the pass Hover asked for, installed by withHover. It is
	// unexported because it can only be built from the POOL -- a caller cannot
	// set it, and one that could would be setting a table for accounts the
	// ranking never saw.
	hover *HoverPlan
	// MaxAutoSpend is the credit gate's ceiling, and it is here for ONE reason:
	// the credit pool is ordered by armed room, and room cannot be computed
	// without it.
	//
	// It is the same number Config.MaxAutoSpend carries, and Decide copies it
	// from there rather than trusting a caller to set both — a pass that ranked
	// on one ceiling while the gate decided on another would walk to a first
	// choice the gate then refuses. 0 is the default and arms nothing, which
	// leaves the pool in the uuid order it had before this field existed.
	//
	// It does not bound a primary credit account; see Candidate.Primary.
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
	// HoverShare is the share this account's thresholds were derived with, and
	// Stranded the quota its rotation cannot reach before StrandedWindow
	// resets. All three are zero when hover is off.
	//
	// REPORTING ONLY. No comparator reads them, and none may: the ordering
	// already carries the figure, because Headroom.Threshold was derived WITH
	// HoverShare and Headroom.Slack is measured against it. Reading them here
	// as well would count the same licence twice.
	//
	// They are carried because the consumers that print the ranking never see
	// the pass it came from -- `ccdad auto --json` renders a []Ranked and holds
	// no HoverPlan -- and a table that printed a threshold whose second term is
	// no longer 100/usable, without saying what that term was, would be
	// arithmetic a reader cannot close.
	HoverShare     float64
	Stranded       float64
	StrandedWindow usage.WindowName
	// HasCreditRoom is whether there is any. False covers every refusal the
	// gate would make — no ceiling configured, overage switched off, spend
	// unreadable, the armed cap spent — so it is not a claim that the account
	// has none, only that none can be armed right now. It is also false for a
	// PRIMARY credit seat, where no room was priced at all: Rank routes such a
	// seat into Result.Order and the ceiling never applied to it.
	HasCreditRoom bool
}

// Result is one ranking pass.
type Result struct {
	// Order is the MAIN POOL, best first: the subscription accounts and the
	// credit seats marked primary, ranked together.
	Order []Ranked
	// Credit is the last-resort credit accounts, ordered by most armed room;
	// see lessCredit. They are not ranked on headroom and never appear in
	// Order: such an account is metered in money and carries no plan windows,
	// so its headroom is permanently unknown — which would file it in the "we
	// have no idea" tier, ahead of every account known to be spent, and make
	// the engine's best candidate the one that costs money. A credit account is
	// a last resort, and the gate, not this comparator, is what decides whether
	// one may be used. A credit account the user marked primary is NOT here: it
	// is in Order, ranked on the credit utilization that is its ordinary meter.
	Credit []Ranked
	// AllOverThreshold is true only when every MAIN-POOL candidate is KNOWN
	// to be over threshold. An account that could not be read makes it false:
	// it is neither over nor under, and letting one expired token decide this
	// is how cswap's engine came to park itself permanently. Last-resort credit
	// accounts are excluded, or a registered credit account's permanently
	// unknown headroom would pin this to false and make ModeRecovery
	// unreachable.
	AllOverThreshold bool
	Mode             Mode
}

// eligible drops the accounts that cannot be ranked at all, BEFORE any ordering
// happens. KindAPIKey has no quota concept, so there is nothing to compare it
// on; a disabled account is held out of auto-rotation by the user.
//
// KindCredit IS eligible — it is a real switch target — but it is ranked on
// money in its own pool unless the user marked it primary, in which case it is
// ranked on its credit utilization in the main pool. eligible itself does not
// change: primary decides WHICH axis, never WHETHER.
func eligible(c Candidate) bool {
	return !c.Disabled && !c.Elsewhere && c.Kind != identity.KindAPIKey
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
// The MinPct clause states "empty implies spent" STRUCTURALLY rather than
// leaving it to arithmetic on a threshold constant. It used to be free: hover
// clamped every threshold below 100, so a used-up window could only ever report
// negative slack. With the clamp gone a window at 100% late in its own cycle is
// measured against a pace target above 100 and reports POSITIVE slack -- which
// would file an account with nothing in it in the roomy tier, and, because
// allOver is built from this predicate, would make ModeRecovery unreachable for
// the whole pool on the strength of one such account.
func Spent(h Headroom) (spent, known bool) {
	if !h.Known {
		return false, false
	}
	return h.Slack < 0 || h.MinPct <= 0, true
}

// OutOfQuota is whether some window this account carries has nothing left in it
// at all -- not "past the number it was given", which is Spent, but empty.
//
// The two questions were one question for as long as every window shared one
// threshold: at a flat 80, slack is raw room shifted by twenty, so "past its
// threshold" and "nearly empty" ordered accounts identically and one predicate
// answered both. Per-window thresholds broke that, and hover derives a fresh
// per-window table on every tick, so under hover they are never the same
// question again.
//
// What makes them come apart at the top is that hover's threshold is a PACE
// TARGET and nothing clamps it. A window far enough through its own cycle is
// measured against a figure above 100, so an account with one point left reports
// POSITIVE slack and lands in the roomy tier ahead of an account holding sixty.
// Ranked on slack alone the near-empty account wins, and the engine hands the
// session to the one account that cannot serve it.
//
// It reads a MINIMUM rather than Pct for the reason those minima exist: Pct
// describes whichever window is tightest on the SLACK axis, and the window that
// is empty is not always that one.
//
// Which minimum is the question this predicate turns on, and it is
// MinAnyModelPct rather than MinPct. "Nothing left in it at all" has to mean
// nothing left that any model could spend, because that is what every caller
// does with the answer: the empty tier files the account behind everything
// else, the anti-flap gate waves every margin through to get off it, and
// MainPoolExhausted starts buying credits once they all say yes. A weekly cap
// scoped to ONE model family satisfies none of that — an account whose Fable
// week is gone and whose all-model weekly holds a fifth can serve every prompt
// that is not Fable, and reading it as empty throws that fifth away and buys
// credits to replace quota already paid for.
//
// MinPct keeps its own job: Spent still reads it, so a blown sub-cap still
// makes the account spent and still moves the engine off it in good time. The
// difference between the two predicates is now exactly the difference between
// "past a line" and "cannot serve anything", which is what they were always
// named for.
//
// Three-valued for the same reason Spent is. An account nobody could read is
// not an empty one.
//
// An unnamed MinAnyModelWindow falls back to MinPct rather than answering
// unknown, and that is the conservative direction on two counts. An account
// whose only readable window is model-scoped has nothing else to go on, and a
// blown sub-cap is then the only thing known about it. And every Headroom built
// by hand — the ranking's own fixtures, the TUI's colour cases, a consumer
// outside this package — leaves the pair unset and keeps exactly the answer it
// has today.
func OutOfQuota(h Headroom) (out, known bool) {
	if !h.Known {
		return false, false
	}
	if h.MinAnyModelWindow == "" {
		return h.MinPct <= 0, true
	}
	return h.MinAnyModelPct <= 0, true
}

// weeklyResetOf is the soonest reset among the weekly windows, which is what
// consume-first spends against.
//
// It reads the same narrowed set the headroom does, so --model means one thing
// in both strategies: a session that named Sonnet does not go chasing an Opus
// cap's expiry, because that is not quota it is going to spend.
func weeklyResetOf(s *usage.Snapshot, model string, t Thresholds) timeValue {
	var out timeValue
	for _, w := range bindingWindows(s, model, t) {
		// Length first, name second -- the same rule the weekly floor applies,
		// spelled at both sites because a pass that disagreed with the floor
		// about which windows are perishable would spend against a reset the
		// floor is not waiting on.
		if !usage.IsWeeklyOf(w.Name, w.Window) {
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

// primaryCredit reports whether this candidate is a credit-metered seat the
// user has marked primary.
//
// The Kind test is half the predicate on purpose. Primary is stored per account
// and survives a reclassification, so it can be set on an account that is not,
// or not yet, metered in credits. There it means nothing: the ceiling it
// removes only ever applied to a credit account, and the meter it selects is
// not the one such an account runs on.
func primaryCredit(c Candidate) bool {
	return c.Primary && c.Kind == identity.KindCredit
}

// creditWindow is the binding-window name a primary seat reports. It is
// extra_usage's own key on the wire, and it is deliberately NOT one of the six
// window names: a caller looking it up in Snapshot.AllWindows finds nothing,
// which is the true answer -- there is no window behind it and no reset, so
// recoveryOf answers "no recovery" without needing to be told about this axis.
const CreditWindow usage.WindowName = "extra_usage"

// creditWindow is CreditWindow under its old in-package spelling, so exporting
// the name moved no line inside this package. Nothing outside it may use this
// one; nothing inside it needs the exported spelling.
const creditWindow = CreditWindow

// creditHeadroom is the axis a PRIMARY credit seat is ranked on.
//
// It reads extra_usage.utilization and nothing else. That figure is already a
// percent of 0-100 on the wire and is never converted, so the minor-unit
// conversion monthly_limit and used_credits go through cannot reach this path
// -- and those two are not read here at all, because a seat billed only in
// credits has no overage to price.
//
// The result is comparable with a subscription account's slack because it is
// the same quantity: how many points are left before the meter that binds this
// account reaches its configured stop. That commensurability is the whole claim
// the primary flag makes.
//
// Threshold is filled in for the same reason HeadroomFor fills it: it is the
// number Slack was measured against, `ccdad status --json` prints the two side
// by side as windowThreshold and slack, and a zero there would print a pair
// that is not arithmetic on each other.
//
// A utilization that could not be read leaves Known false, which files the seat
// in the middle tier exactly as an unreadable subscription account -- not
// spent, and not empty. The cost is one switch onto a seat that may turn out to
// have nothing left, which is the cost an unreadable subscription account
// already carries, and the alternative is a pool of unreadable accounts that is
// a dead end.
func creditHeadroom(c Candidate, o Options) Headroom {
	var e usage.ExtraUsage
	if c.Usage != nil {
		e = c.Usage.ExtraUsage
	}
	return creditHeadroomOf(e, o.Thresholds())
}

// creditHeadroomOf is creditHeadroom's arithmetic, with the Candidate and the
// Options taken off it.
//
// It is separated for one reason: HeadroomOrCredit needs the same axis from a
// snapshot alone, and the ranking and the rendering answering the same reading
// with two different numbers is the exact defect this pair exists to close.
// Spelled twice, the two would drift the first time either changed, and the
// symptom would be a dashboard disagreeing with the engine about the account it
// is describing.
func creditHeadroomOf(e usage.ExtraUsage, t Thresholds) Headroom {
	pct, ok := e.Percent()
	if !ok {
		return Headroom{}
	}
	thr := t.CreditThreshold()
	// The all-model minimum is the same figure here, and named rather than left
	// unset: a credit balance is money, and no choice of model spends less of
	// it. Left unset it would fall back to MinPct, which happens to be the same
	// number today — naming the window states the fact rather than relying on
	// the two staying equal.
	return Headroom{
		Pct:               100 - pct,
		MinPct:            100 - pct,
		MinWindow:         creditWindow,
		MinAnyModelPct:    100 - pct,
		MinAnyModelWindow: creditWindow,
		Slack:             thr - pct,
		Threshold:         thr,
		Known:             true,
		Binding:           creditWindow,
	}
}

func measure(c Candidate, o Options) Ranked {
	// thresholdsFor rather than Thresholds: under hover the table is derived
	// per account, and it is the same call below so the two window sets this
	// function reads cannot be built from different tables.
	h := HeadroomFor(c.Usage, o.Model, o.thresholdsFor(c))
	// A primary seat is metered in credits rather than in plan windows, so it is
	// ranked on the credit allowance instead. This REPLACES the plan headroom
	// above rather than being compared with it, and that is a real choice with a
	// real cost, not the no-op the comment here used to claim.
	//
	// What it used to claim was that such a seat carries no plan windows, so the
	// line above found nothing for it. That is false. identity.Classify files an
	// account KindCredit off Snapshot.HasSubscriptionWindows, which iterates
	// RateLimitWindows -- the fixed five and the two codex keys, and nothing
	// else -- so a weekly cap that arrived in limits[] is invisible to it. A
	// seat with no fixed windows, extra_usage enabled and a real scoped weekly
	// is both a credit seat AND an account carrying quota, and HeadroomFor
	// answers Known for it.
	//
	// So this line can discard a measured plan headroom. Measured: a seat whose
	// opted-in weekly is 99.9% used reports plan slack near -50 and is ranked
	// here on a credit allowance of 85, which puts it FIRST. The pre-emptive
	// switch no longer walks onto such a seat -- it reads the derived table now,
	// and the seat derives one -- but the ORDINARY better-target rule still can.
	//
	// Taking the minimum of the two instead is the obvious repair and it is not
	// made here, because it would move Spent for a credit seat, and Spent feeds
	// MainPoolExhausted, which is the one decision in ccdad that spends the
	// user's money. That needs its own investigation and its own release rather
	// than a line in this one.
	if primaryCredit(c) {
		h = creditHeadroom(c, o)
	}
	// Recovery is the moment the account stops being spent, and that is not
	// always the binding window's rollover. A blown WEEKLY window still holds the
	// account back after the five-hour window has come back, so when there is one
	// it is that floor which has to clear.
	clears := h.Binding
	if h.HasFloor {
		clears = h.Floor
	}
	rec := recoveryOf(c.Usage, clears)
	weekly := weeklyResetOf(c.Usage, o.Model, o.thresholdsFor(c))

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
	// The derivation's own figures, copied for the renderers. Nothing below
	// reads them; see the fields' own doc.
	if o.hover != nil {
		if a, ok := o.hover.AccountFor(c.UUID); ok {
			r.HoverShare, r.Stranded, r.StrandedWindow = a.Share, a.Stranded, a.Window
		}
	}
	return r
}

// Rank orders the eligible accounts, best first. It does not reorder its input.
func Rank(cands []Candidate, o Options) Result {
	o = o.withHover(cands)
	measured := make([]Ranked, 0, len(cands))
	credit := make([]Ranked, 0)
	// True until some MAIN-POOL candidate turns out not to be known-and-over.
	// An account that could not be read counts against it: it is neither over
	// nor under, and letting one expired token answer this is how cswap's engine
	// came to park itself permanently.
	allOver := true
	for _, c := range cands {
		if !eligible(c) {
			continue
		}
		r := measure(c, o)
		// A credit account that is NOT primary is the last resort: metered in
		// money, ordered by armed room, behind the credit gate. armedRoom is
		// not computed for a primary seat and the gate never sees one, because
		// the ceiling it prices bounds an OVERAGE -- spending past quota that
		// is already paid for -- and a seat billed only in credits has no such
		// quota for its credits to be an overage of.
		if c.Kind == identity.KindCredit && !c.Primary {
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

// headroomTier separates four kinds of candidate, because "we know it has room"
// and "we have no idea" and "we know it is past its pace" and "we know it is
// empty" are four answers, not two. The unknown sits second: a maybe beats a no,
// and trying it is how a pool of unreadable accounts stops being a dead end.
//
// The last tier is categorical on purpose, and it is the one place in the
// ranking where raw room outranks pace outright. Every other comparison here is
// a judgement about which account SHOULD spend next; this one is about which
// account CAN, and no amount of being nicely on pace makes a window with nothing
// left in it able to serve a prompt. Keeping it as a tier rather than folding it
// into the slack comparison is what stops the cap at HoverCap from letting an
// empty account outrank a roomy one by a point of arithmetic.
func headroomTier(r Ranked) int {
	if empty, known := OutOfQuota(r.Headroom); known && empty {
		return 3
	}
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

// emptyTier is headroomTier's last distinction on its own, for the recovery
// order -- which tiers on the recovery instant instead and so cannot reach
// headroomTier for the rest.
//
// It is separate rather than shared because the two orders disagree about
// everything else: recovery deliberately files a spent account that comes back
// inside the hour ahead of one that does not, which headroomTier would rank the
// other way round. The only thing they must agree on is that an empty account
// goes last.
func emptyTier(r Ranked) int {
	if empty, known := OutOfQuota(r.Headroom); known && empty {
		return 1
	}
	return 0
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
	// Ahead of the recovery instant, because recovery order otherwise ranks an
	// empty account FIRST exactly when it is most wrong to: the soonest reset
	// belongs to the window that has been running longest, which is the window
	// most likely to be the one that ran out. An account that comes back in an
	// hour is a good target; an account that comes back in an hour AND has
	// nothing left until then is not somewhere to send the next prompt.
	if ta, tb := emptyTier(a), emptyTier(b); ta != tb {
		return ta < tb
	}
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
// re-stating any of them. mainPoolExhausted is true because this asks what the
// gate WOULD say when it is the credit pool's turn; whether it is that pool's
// turn is Decide's question, not the comparator's.
//
// It is never reached for a primary credit account: Rank routes one into the
// main pool above, so the ceiling this prices is not applied to the one kind of
// account for which that ceiling has no meaning.
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

// MainPoolExhausted answers step 2 of the credit gate: may the LAST-RESORT
// credit pool be considered at all?
//
// The main pool is what Rank puts in Result.Order: the subscription accounts,
// and the credit seats marked primary. A primary seat with room means the pool
// is not exhausted, because its credits are quota the user is already paying
// for -- spending overage while it sits unused is the exact inversion the gate
// order exists to prevent.
//
// An account that could not be read holds the pool OPEN. It is not known to be
// spent, and the fail-closed direction on money is to keep using what is
// already bought rather than to start spending because one poll failed. A pool
// with nothing in it is exhausted trivially.
//
// The test is OutOfQuota and NOT Spent, and on this path the difference is
// money. Spent asks whether an account is past the number it was given, and
// under hover that number is a PACE target -- so a pool of six accounts every
// one of which is merely running ahead of schedule answers "exhausted" while
// carrying, between them, a fifth of a week of quota already paid for. Opening
// the last-resort pool there starts buying credits to avoid spending
// subscription quota that nobody is going to get back. The gate may only open
// once the free quota is actually gone.
func MainPoolExhausted(cands []Candidate, o Options) bool {
	main := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if !eligible(c) {
			continue
		}
		if c.Kind == identity.KindSubscription || primaryCredit(c) {
			main = append(main, c)
		}
	}
	if len(main) == 0 {
		return true
	}
	for _, c := range main {
		// measure is the ONE place that chooses which axis a candidate is
		// ranked on -- plan windows, or the credit allowance for a primary
		// seat -- so this asks it rather than choosing again here. "Is this
		// account spent" already has more than one implementation in this
		// tree; a second copy of "on which axis" would be the next one, and it
		// would be the copy that silently drops the --model narrowing.
		spent, known := Spent(measure(c, o).Headroom)
		if !known || !spent {
			return false
		}
	}
	return true
}

// SubscriptionExhausted is MainPoolExhausted under the name it had while the
// main pool was subscriptions and nothing else.
//
// Deprecated: use MainPoolExhausted. It is kept for one release so a caller
// that has not been updated -- internal/strategy/antiflap.go among them --
// keeps compiling and keeps getting the same answer, rather than a name that
// quietly means less than it used to.
func SubscriptionExhausted(cands []Candidate, o Options) bool {
	return MainPoolExhausted(cands, o)
}

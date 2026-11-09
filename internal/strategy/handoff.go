package strategy

import (
	"time"

	"github.com/Kweiza/ccdaddy/internal/pollpolicy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// Hover's hand-off: when the session may leave the live account at a limit, and
// for which account.
//
// Two questions, kept apart. The ORDER files every account the session is not on
// by its levels and its mark alone -- empty, its last point, held out -- because
// a rung that read a rate would move whenever the rate's source moved: the order
// re-filed accounts on every switch. Whether an account can take the work
// running now is a separate verdict, computed once per account in measure
// (runwayOf), and read only by the decision (hoverMove) and by the reporters.
//
// The verdict is tiered, absolute first and relative second:
//
//   - a CARRIER would not be at a limit at its first live reading -- the same
//     answer whoever is live, so every move the order prefers lands where the
//     next look does not demand another;
//   - a BRIDGE outlasts the live account by a cooldown, and is taken only when
//     the live account would stop before the engine next reads it;
//   - a LAST RESORT is anything with work left in it, taken only when the live
//     account has nothing left, by level or by projection.
//
// An empty account is never a target: that is the eighteen-switch guard,
// TestAFleetWithNothingLeftStaysPutInsteadOfSwappingEmptyForEmpty.

// EvaluationInterval is how often the engine is asked: the daemon's tick, τ in
// the hand-off rule. It lives here so the daemon's loop and the weekly
// simulation read one constant, and so "before the engine next looks" is stated
// in the unit the engine actually looks in.
const EvaluationInterval = time.Second

// The hand-off rule's horizons, each a constant of the poll policy rather than a
// number of this file's own. TestTheHorizonTermsAreThePollPolicysOwn.
const (
	// hoverReserve is λ: the live account is closing when the work would stop it
	// within λ after its next reading. It is the poll policy's normal floor, the
	// shortest gap it leaves between two ordinary readings, so a live account
	// that would stop inside it may well stop before the engine reads it again.
	//
	// A longer reserve tolerates a sharper rise in the work between two
	// readings and throws away more of every window the session leaves.
	// CandidateMaxInterval (600 s) was measured against this over a simulated
	// week and lost: it left more than twice the five-hour points behind,
	// stopped today's fleet for 3427 s under production load where this stops
	// it for none, and stopped with room on another account in five variants
	// where this does in two. It was better in one, a burst on two Max
	// accounts alone, by 23 s of 1177 -- the burst gap no reserve closes
	// (TestEveryFleetHandsOffBeforeTheLimit).
	hoverReserve = pollpolicy.MinInterval
	// hoverBlind is B, the longest an unthrottled live account goes unread. It
	// is also the cadence the first live poll after a move gets, because that
	// poll measures no movement against the idle anchor before it.
	hoverBlind = pollpolicy.ActiveMaxInterval
)

// workRate is the work's rate on windows of one length: the most recent rate
// measured across two readings both taken while the account measured was live.
type workRate struct {
	perMin float64
	at     time.Time
}

// workRates is the work's rate by window length.
//
// Per LENGTH, and never converted between lengths: a rate is in points of one
// window, and five points a minute of a five-hour window are not five of a
// week. Within a length it is applied across names and across plans -- the
// five-hour window of one Max seat and of another, a week and a surface's
// weekly cap -- and that is a known risk, stated rather than hidden: a point on
// one plan is not a point on another, and the wire carries no plan size. The
// rejected alternative was to confirm the unit per capacity class, which would
// read "cannot say" on almost every account for want of a size nobody reports.
// The error is bounded instead: the prompt poll a switch schedules gives the new
// live account a live pair of its own within the serve floor and one live
// cadence (pollpolicy.ServeTTL + hoverBlind), after which its own rate stands,
// and the last-point level hand-off catches whatever the projection misses.
type workRates map[time.Duration]workRate

// workRatesOf is the table for one pass.
//
// The latest measurement for each length, over every account the pass can see:
// the work moves between accounts and its rate goes with it, so right after a
// switch it is the rate measured on the account just left, until the new live
// account has a pair of its own. A pair whose readings straddle the last switch
// onto that same account spans an absence -- the account was left and taken
// back between its two readings -- and is diluted by it, so it is dropped; a
// pair taken before the account was left is the work as it was then, and stays. An idle pair never enters at all: usage.LiveBurn is set only from
// a live one. Of the windows of one length one pair measured -- a week, a
// surface's weekly cap, a model's -- the highest is the work's: a cap that counts
// part of the work moves no faster than the window that counts all of it, so the
// highest is the whole, and an error that way only hands off sooner.
func workRatesOf(cands []Candidate, switchedAt time.Time, switchedTo string) workRates {
	out := workRates{}
	for _, c := range cands {
		for _, b := range c.LiveBurns {
			if !b.Measured() {
				continue
			}
			if isActive(c.UUID, switchedTo) && b.From.Before(switchedAt) && b.At.After(switchedAt) {
				continue
			}
			length, ok := usage.WindowLength(b.Window)
			if !ok {
				continue
			}
			if cur, has := out[length]; has && (b.At.Before(cur.at) || b.At.Equal(cur.at) && b.PerMin <= cur.perMin) {
				continue
			}
			out[length] = workRate{perMin: b.PerMin, at: b.At}
		}
	}
	return out
}

// work is what one pass knows about the work running now.
type work struct {
	// rates is hover's: the work's rate by window length.
	rates workRates
	// poolMax is the configured strategies' carry rate: the pool's maximum
	// measured rate, as it has always been there. See closingWindow.
	poolMax sessionBurn
	// switchedAt and switchedTo are the last switch, which says when the work
	// arrived on the account it was moved to.
	switchedAt time.Time
	switchedTo string
	// left is every departure mark, the live account's included: when each
	// account was last left at a limit, which says how long the work ran on it
	// after its reading.
	left departures
}

// workOf is the work for a pass over cands, with the last switch from st when
// there is a state to read it from.
func workOf(cands []Candidate, st *State) work {
	var w work
	if st != nil {
		w.switchedAt, w.switchedTo = st.LastSwitch()
		w.left = st.departuresFor("")
	}
	w.rates = workRatesOf(cands, w.switchedAt, w.switchedTo)
	w.poolMax = measuredSessionBurn(cands)
	return w
}

// runway is when the work running now would stop an account if it ran there.
type runway struct {
	// known is whether any of the account's windows had a rate to project on.
	known bool
	// at is the earliest stop, and on the window it is on. A known runway with
	// a zero at is one nothing stops: every rate it read was zero, or every
	// window resets before it is spent.
	at time.Time
	on usage.WindowName
}

// stopsBy is whether the work stops this account no later than t.
func (r runway) stopsBy(t time.Time) bool {
	return r.known && !r.at.IsZero() && !r.at.After(t)
}

// runwayOf is THE projection: when the work running now would stop this account.
//
// It reads every window no model choice can dodge (bindingWindows on hover's
// table), not only the one with the least room, because a rate measured on
// windows of one length applies to every window of that length an account
// carries: an account whose five-hour window has five points left and whose week
// has four is stopped by the five-hour window at a five-hour rate long before the
// week. Each window counts only if it would be spent before it resets.
//
// The live account and every other one differ only in when the clock starts.
// On the live account the reading's room is being spent from when the work
// reached it -- the reading, or the switch onto it if that came later; on any
// other account, from now. On both it is less what the work spent there
// between the reading and the account's departure, when its mark says it was
// left after it was read: without that, an account just left reads up to one
// live cadence roomier than it is, and one the work has just come back to reads
// as though it had never been spent.
//
// The measured rate wins wherever one exists for a window's length. Only on the
// live account, and only for a length nobody has measured yet -- the cold start,
// before any account has two live readings -- does the account's own evidence
// stand in: its window average (usage.PaceOf) and the burn its own last two
// readings measured. On any other account a missing rate is no runway at all,
// and its verdict is its level: the cold-start risk, named rather than hidden,
// which lasts until the work has been read live twice.
func (o Options) runwayOf(c Candidate) runway {
	live := isActive(c.UUID, o.live)
	from := o.Now
	if live {
		// A reading with no FetchedAt has no provenance: nothing says when its
		// room was read, so nothing can say when the work spends it (the same
		// refusal preemptHorizon makes). Its level still speaks for it.
		if c.FetchedAt.IsZero() {
			return runway{}
		}
		from = o.work.spendingFrom(c)
	}
	var out runway
	for _, w := range bindingWindows(c.Usage, o.Model, o.thresholdsFor(c)) {
		pct, ok := w.Percent()
		if !ok {
			continue
		}
		at, known := o.stopOf(c, w, 100-pct, from, live)
		if !known {
			continue
		}
		out.known = true
		if at.IsZero() || !stopsBeforeReset(w.Window, at) {
			continue
		}
		if out.at.IsZero() || at.Before(out.at) {
			out.at, out.on = at, w.Name
		}
	}
	return out
}

// stopOf is when the work spends room on window w of c, starting from from, and
// whether anything can say. A zero instant with known true is a rate of nothing:
// nothing is being spent, so nothing runs out.
func (o Options) stopOf(c Candidate, w usage.NamedWindow, room float64, from time.Time, live bool) (time.Time, bool) {
	length, hasLength := usage.WindowLengthOf(w.Name, w.Window)
	if rate, ok := o.work.rateOn(length); hasLength && ok {
		room -= rate.perMin * o.work.workedAfterReading(c).Minutes()
		at, spends := atRate(from, room, rate.perMin)
		if !spends {
			return time.Time{}, true
		}
		return at, true
	}
	if !live {
		return time.Time{}, false
	}
	var earliest time.Time
	known := false
	if proj, ok := usage.PaceOf(w.Name, w.Window, o.Now).Projection(); ok {
		earliest, known = proj.ExhaustionAt, true
	}
	if c.HasBurn && c.BurnWindow == w.Name {
		if at, spends := atRate(from, room, c.BurnPerMin); spends && (!known || at.Before(earliest)) {
			earliest = at
		}
		known = true
	}
	return earliest, known
}

// The work is read through these, and each answers for a pass nothing installed
// a work on -- a caller that measured one account on its own -- as a pass with
// no rate and no switch.

// rateOn is the work's rate on windows of one length.
func (w *work) rateOn(length time.Duration) (workRate, bool) {
	if w == nil {
		return workRate{}, false
	}
	r, ok := w.rates[length]
	return r, ok
}

// carryRate is the configured strategies' carry rate, the pool's maximum.
func (w *work) carryRate() sessionBurn {
	if w == nil {
		return sessionBurn{}
	}
	return w.poolMax
}

// spendingFrom is when the work began spending the live account's reading: the
// reading itself, or the switch onto the account if the work arrived after it
// was read.
func (w *work) spendingFrom(c Candidate) time.Time {
	if w != nil && isActive(c.UUID, w.switchedTo) && w.switchedAt.After(c.FetchedAt) {
		return w.switchedAt
	}
	return c.FetchedAt
}

// workedAfterReading is how long the work ran on an account after its last
// reading was taken, by its departure mark: the span between the reading and
// the departure, when the account was left after it was read. The live account
// is asked too -- one the work has come back to is still short what it spent
// before it was left, until its next reading says so.
func (w *work) workedAfterReading(c Candidate) time.Duration {
	if w == nil {
		return 0
	}
	m, ok := w.left[c.UUID]
	if !ok || !m.At.After(c.FetchedAt) {
		return 0
	}
	return m.At.Sub(c.FetchedAt)
}

// liveLimit is the live account's state against the work's runway: closing when
// the work would stop it before the reading after next, stopping when before the
// next one, and out when before the engine looks again. Each is a narrower
// version of the one before, and the window named is the one that stops it.
type liveLimit int

const (
	liveRoom liveLimit = iota
	liveClosing
	liveStopping
	liveOut
)

// The leads, before the end of a runway, that the hand-off measures from a
// look: out and stopping are a tick before it, closing a reserve before it,
// and a carrier must outlast a live cadence and a reserve after its first live
// reading. Each is stated here once, for liveLimitOf, carries and lastResort
// and for clockTurns, which says when the clock reaches them.
const (
	outLead     = EvaluationInterval
	closingLead = hoverReserve
	carrierLead = hoverBlind + hoverReserve
)

// liveLimitOf files the live account against its runway.
//
// The next reading is the one the scheduler has set, or now when it is
// overdue: a look already due is the next thing that happens.
func (o Options) liveLimitOf(c Candidate, rw runway) liveLimit {
	next := c.NextPollAt
	if next.Before(o.Now) {
		next = o.Now
	}
	switch {
	case rw.stopsBy(o.Now.Add(outLead)):
		return liveOut
	case rw.stopsBy(next.Add(outLead)):
		return liveStopping
	case rw.stopsBy(next.Add(closingLead)):
		return liveClosing
	}
	return liveRoom
}

// clockTurns is every instant at which the clock alone can change what the
// hand-off makes of this account's runway, with no reading landing and no
// switch: each lead above, counted back from the runway's end. A live
// account's next look can be overdue -- no poll is made sooner than ServeTTL
// after the last -- and a target's prompt poll is now once its reading is that
// old, so from then on each lead runs from the clock. Between two of these
// instants a pass over
// the same readings and the same state gives the same answer, which is what
// lets the weekly simulation skip the seconds in between.
func (r Ranked) clockTurns() []time.Time {
	if !r.HasRunway || r.RunsOutAt.IsZero() {
		return nil
	}
	return []time.Time{
		r.RunsOutAt.Add(-outLead),
		r.RunsOutAt.Add(-closingLead),
		r.RunsOutAt.Add(-carrierLead),
	}
}

// carries is whether an account the session is not on is a CARRIER: it would
// not be at a limit at its first live reading.
//
// After a move the engine first reads the account at its prompt poll
// (pollpolicy.PromptPollAt, the instant the switch writes), and that reading's
// next poll is at most one live cadence later, hoverBlind; at that reading the
// account is closing if the work stops it within the reserve. So outlasting the
// prompt poll, a live cadence and the reserve is exactly "the move is not
// followed by a second move on the same rate" (TestACarrierIsNotClosingAtItsFirstLiveReading).
// An account with no runway is a carrier by its level, which is the cold start;
// an empty one, or one on its last point, never is.
func (o Options) carries(c Candidate, r Ranked, rw runway) bool {
	if !r.roomyByLevel() {
		return false
	}
	if !rw.known || rw.at.IsZero() {
		return true
	}
	first := pollpolicy.PromptPollAt(c.FetchedAt, c.NextPollAt, c.LastRateLimited, o.Now)
	return rw.at.After(first.Add(carrierLead))
}

// roomyByLevel is whether an account has room by its levels alone: not empty,
// and not on its last point.
func (r Ranked) roomyByLevel() bool {
	if empty, known := OutOfQuota(r.Headroom); known && empty {
		return false
	}
	return r.LastPoint == ""
}

// hoverMove is every move hover makes, and the hold when it makes none: the
// pre-emptive move, the ordinary move (a return to a five-hour account, a sooner
// week), the last-point hand-off and the empty live account all go through it,
// in ModeRecovery as in ModeHover -- that mode only changes the order it walks.
//
// It picks the TARGET first and applies the cooldown second. "At a limit with
// nowhere to go" is then blocked whether or not a cooldown is running and
// whichever account sorts first, and a hold the cooldown imposes names the move
// it held (Plan.Held), which is the one `ccdad switch --force` may take.
// TestTheTargetIsChosenBeforeTheCooldown.
func (p *Plan) hoverMove(activeUUID string, cfg Config, st *State, o Options, byUUID map[string]Candidate) {
	res := p.Result
	live, inPass := find(res, activeUUID)
	if !inPass {
		// No baseline: the live login is not an account in the pass. Anything
		// beats staying on it, and the cooldown agrees (cooldownGate).
		target, ok := firstCarrier(res, activeUUID, byUUID, o.Now)
		if !ok {
			target = res.Order[0]
		}
		p.moveOrHold(target, ReasonBetterTarget, activeUUID, cfg, st, o.Now)
		return
	}

	if live.AtLimit() == "" {
		p.hoverMoveWithRoom(live, activeUUID, cfg, st, o.Now)
		return
	}

	// At a limit: the first carrier, then a bridge if the live account would
	// stop unseen, then a last resort if it has nothing left, then money.
	if target, ok := firstCarrier(res, activeUUID, byUUID, o.Now); ok {
		p.moveOrHold(target, leaveReason(live, target), activeUUID, cfg, st, o.Now)
		return
	}
	if live.limit >= liveStopping {
		if target, ok := bridge(res, live); ok {
			p.moveOrHold(target, leaveReason(live, target), activeUUID, cfg, st, o.Now)
			return
		}
	}
	if live.out(o.Now) {
		if target, ok := lastResort(res, live, o.Now); ok {
			p.moveOrHold(target, leaveReason(live, target), activeUUID, cfg, st, o.Now)
			return
		}
		// Money comes after every free point of subscription quota, and only
		// on the user's own thresholds. Why at this point and not at the first
		// limit: until the live account is out, it still holds free quota of
		// its own, and a bridge or a last resort may yet appear.
		if p.SubscriptionExhausted && len(res.Credit) > 0 {
			p.creditWalk(byUUID, activeUUID, cfg, st, o.Now)
			return
		}
	}
	p.hold(ReasonNothingCanServe, soonestRollover(res))
}

// hoverMoveWithRoom is the move from a live account with room: to the first
// account ahead of it in the order that can carry the work, on the order's own
// same-rung rules (hoverGate). An account ahead of it that cannot carry the work
// is passed over, and if nothing else moves the session the hold says so.
func (p *Plan) hoverMoveWithRoom(live Ranked, activeUUID string, cfg Config, st *State, now time.Time) {
	passedOver := false
	for _, target := range p.Result.Order {
		if isActive(target.UUID, activeUUID) {
			break
		}
		if !target.Carries {
			passedOver = true
			continue
		}
		reason, refused, retry := hoverGate(live, target, cfg)
		if refused {
			p.hold(reason, retry)
			p.Held = target
			return
		}
		p.moveOrHold(target, reason, activeUUID, cfg, st, now)
		return
	}
	if passedOver {
		p.hold(ReasonCannotCarry, time.Time{})
		return
	}
	p.hold(ReasonAlreadyBest, time.Time{})
}

// moveOrHold moves to target unless the cooldown holds the engine, in which case
// the hold names the move it held.
func (p *Plan) moveOrHold(target Ranked, reason Reason, activeUUID string, cfg Config, st *State, now time.Time) {
	if why, held, retry := cooldownGate(p.Result, activeUUID, cfg, st, now); held {
		p.hold(why, retry)
		p.Held = target
		return
	}
	p.switchTo(target, reason, activeUUID)
}

// out is whether the live account has nothing left that one more evaluation
// would see: empty by level, or projected out before the engine looks again.
func (r Ranked) out(now time.Time) bool {
	if empty, known := OutOfQuota(r.Headroom); known && empty {
		return true
	}
	return r.limit == liveOut
}

// firstCarrier is the first account in the order, the live one excluded, that
// can carry the work -- one whose poller is not throttled if there is one: a
// throttled reading cannot be refreshed after the move, and that is a
// preference rather than a filter (TestPreemptionPrefersACandidateItCanStillSee).
func firstCarrier(res Result, activeUUID string, byUUID map[string]Candidate, now time.Time) (Ranked, bool) {
	var throttled Ranked
	hasThrottled := false
	for _, r := range res.Order {
		if isActive(r.UUID, activeUUID) || !r.Carries {
			continue
		}
		if recentlyRateLimited(byUUID[r.UUID], now) {
			if !hasThrottled {
				throttled, hasThrottled = r, true
			}
			continue
		}
		return r, true
	}
	return throttled, hasThrottled
}

// bridge is the account that outlasts the stopping live account by the most,
// among those that outlast it by at least a cooldown and have room by their
// levels.
//
// A cooldown, because after the move the engine is held for one: the bridge has
// to survive it, and a move back would need the same margin the other way, so
// every leg is at least two cooldowns and the endgame between two short-window
// accounts is bounded. Only a known runway can be compared, so an account with
// none is a carrier or nothing here.
func bridge(res Result, live Ranked) (Ranked, bool) {
	var best Ranked
	found := false
	beyond := live.RunsOutAt.Add(HoverCooldown)
	for _, r := range res.Order {
		if r.UUID == live.UUID || !r.roomyByLevel() || !r.HasRunway || r.RunsOutAt.IsZero() {
			continue
		}
		if !r.RunsOutAt.After(beyond) {
			continue
		}
		if !found || r.RunsOutAt.After(best.RunsOutAt) {
			best, found = r, true
		}
	}
	return best, found
}

// lastResort is where a live account with nothing left goes: anything with work
// left in it, the longest runway first and an account with no runway last. Its
// last point is allowed here and nowhere else -- one point is better than a stop
// -- and an empty account never is.
func lastResort(res Result, live Ranked, now time.Time) (Ranked, bool) {
	var best Ranked
	found := false
	for _, r := range res.Order {
		if r.UUID == live.UUID {
			continue
		}
		if empty, known := OutOfQuota(r.Headroom); known && empty {
			continue
		}
		if r.HasRunway && !r.RunsOutAt.IsZero() && !r.RunsOutAt.After(now.Add(outLead)) {
			continue
		}
		if !found || outlastsForLastResort(r, best) {
			best, found = r, true
		}
	}
	return best, found
}

// outlastsForLastResort is lastResort's preference: a runway nothing stops, then
// the latest stop, then no runway at all.
func outlastsForLastResort(a, b Ranked) bool {
	rank := func(r Ranked) int {
		switch {
		case r.HasRunway && r.RunsOutAt.IsZero():
			return 0
		case r.HasRunway:
			return 1
		}
		return 2
	}
	if ra, rb := rank(a), rank(b); ra != rb {
		return ra < rb
	}
	return a.HasRunway && a.RunsOutAt.After(b.RunsOutAt)
}

package strategy

import (
	"time"

	"github.com/Kweiza/ccdaddy/internal/identity"
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
	// CandidateMaxInterval (600 s) was measured against this over the
	// simulated week -- whole-percent readings, the daemon's warm-ups, the
	// pause load -- and lost on the whole: 4298 s stopped with room on
	// another account across every fleet and load, against 3562 s here, and
	// more left behind, today's five-hour account reaching its weekly reset
	// 3.98 points unspent against 1.98. It was better in five variants:
	// today's fleet and the big Ultras under burst load, 29 s to none each;
	// two Max under heavy load, 1641 to 1272; two Max, three Ultras under
	// heavy load, 239 to 233, and under production load, 346 to 234. It was
	// worse in the other five: today's fleet under production load, none to
	// 422; two Max under burst load, 958 to 1370, and under production load,
	// 136 to 430; two Max, three Ultras under the pause load, 5 to 91; two
	// Ultras under production load, 37 to 104
	// (TestEveryFleetHandsOffBeforeTheLimit).
	hoverReserve = pollpolicy.MinInterval
	// hoverBlind is B, the longest an unthrottled live account goes unread. It
	// is also the cadence the first live poll after a move gets, because that
	// poll measures no movement against the idle anchor before it.
	hoverBlind = pollpolicy.ActiveMaxInterval
)

// workRate is the work's rate on windows of one length: the most recent rate
// measured across two readings both taken while the account measured was live,
// and whether that pair saw the work running (usage.LiveBurn.Running).
type workRate struct {
	perMin  float64
	at      time.Time
	running bool
}

// replaces is whether r is the work's rate rather than cur: a running pair over
// a pause whatever their ages, because the rate is the work's while it runs
// and pausing is not working slower; then the later; then, measured at the
// same reading, the higher.
func (r workRate) replaces(cur workRate) bool {
	if r.running != cur.running {
		return r.running
	}
	if !r.at.Equal(cur.at) {
		return r.at.After(cur.at)
	}
	return r.perMin > cur.perMin
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
// The latest running measurement for each length, over every account the pass
// can see: the work moves between accounts and its rate goes with it, so right
// after a switch it is the rate measured on the account just left, until the
// new live account has a pair of its own. A pause -- a pair that did not move --
// stands only where nothing running was measured (workRate.replaces). An idle
// pair never enters at all: usage.LiveBurn is set only from a live one, and only
// within one live stint (usage.LiveAnchor), so a pair spanning an absence is
// not stored to be read here. And a pair whose first reading precedes the last
// switch onto its own account is never adopted even so: the stint rule rests on
// the cache record the switch writes after the state's, and a crash between the
// two, or a failed write, leaves the pair stored -- spanning the absence, its
// rate diluted by it. TestAPairSpanningAReturnIsNeverTheWorksRate.
//
// Only pairs on the windows that are limits for this pass (bindingWindows on
// the table the pass ranks on). A model family's cap is not one under hover,
// and a Fable cap climbing thirty points while the full week climbs one would
// otherwise set the week's rate and hand an account off two hours early; a
// model named with --model makes its own family's cap a limit, and its rate
// with it. Of the limit windows of one length one pair measured -- a week and a
// surface's weekly cap -- the highest is the work's: a cap that counts part of
// the work moves no faster than the window that counts all of it.
// TestAModelCapsRateIsNotTheWeeksRate.
//
// A rate has no age. It stands until a newer running pair on its length
// replaces it -- across a reset, and across a restart, since the cache keeps it
// -- and two costs follow, named here rather than mended by a decay the engine
// does not have. A five-hour rate measured in a burst seals itself while the
// session is on weekly-only seats, where no five-hour pair is taken: above
// about 12.5 points a minute even a freshly rolled Max is no carrier -- its
// hundred points must outlast a prompt poll, a live cadence and the reserve,
// eight minutes -- so the session stays on the Ultra until that seat's own
// limit. And a weekly rate is most often one quantum over one pair, a point in
// a few minutes, several times what the week is spent at: it keeps a week near
// its end from carrying, and in the simulated week left about one point more of
// the five-hour account's week unspent.
func (o Options) workRatesOf(cands []Candidate, switchedAt time.Time, switchedTo string) workRates {
	out := workRates{}
	for _, c := range cands {
		limits := map[usage.WindowName]bool{}
		for _, w := range bindingWindows(c.Usage, o.Model, o.thresholdsFor(c)) {
			limits[w.Name] = true
		}
		for _, b := range c.LiveBurns {
			if !b.Measured() || !limits[b.Window] {
				continue
			}
			if isActive(c.UUID, switchedTo) && b.From.Before(switchedAt) && b.At.After(switchedAt) {
				continue
			}
			length, ok := usage.WindowLength(b.Window)
			if !ok {
				continue
			}
			r := workRate{perMin: b.PerMin, at: b.At, running: b.Running()}
			if cur, has := out[length]; has && !r.replaces(cur) {
				continue
			}
			out[length] = r
		}
	}
	return out
}

// work is what one pass knows about the work running now.
type work struct {
	// rates is hover's: the work's rate by window length, filled in by Rank
	// once the table the pass ranks on is known (workRatesOf reads it).
	rates workRates
	// seen is every account the pass can see the work's rate on, the
	// quarantined included: the rate belongs to the work.
	seen []Candidate
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
	w.seen = cands
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
		// A window that has reset since the reading is a new window: the work
		// spends it from the reset, not from the reading.
		at, known := o.stopOf(c, w, 100-pct, sinceReset(from, w.Window), live)
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
		room -= rate.perMin * o.work.workedAfterReading(c, w.Window).Minutes()
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

// workedAfterReading is how long the work ran against window win of an account
// after its last reading was taken, by its departure mark: the span between the
// reading and the departure, when the account was left after it was read. The
// live account is asked too -- one the work has come back to is still short
// what it spent before it was left, until its next reading says so.
//
// Only within the window the reading describes. A window that has reset since
// (usage.Window.RolledAt) is a new window, and what the work spent before the
// reset spent the old one: it cannot spend the room the reset restored.
// TestWorkBeforeAResetDoesNotSpendTheRoomTheResetRestored.
func (w *work) workedAfterReading(c Candidate, win usage.Window) time.Duration {
	if w == nil {
		return 0
	}
	m, ok := w.left[c.UUID]
	if !ok {
		return 0
	}
	from := sinceReset(c.FetchedAt, win)
	if !m.At.After(from) {
		return 0
	}
	return m.At.Sub(from)
}

// sinceReset is from, or the instant window win reset after it
// (usage.Window.RolledAt), whichever is later: the work spends a window from
// when it began, and a window that reset is one that began again. Both the live
// account's projection and the departure correction read it.
// TestTheLiveAccountsRunwayRunsFromAResetAfterItsReading.
func sinceReset(from time.Time, win usage.Window) time.Time {
	if at, rolled := win.RolledAt(); rolled && at.After(from) {
		return at
	}
	return from
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
//
// A residual, kept on purpose: NextPollAt can hold an instant the daemon's
// due() gate refuses. The danger band schedules the live account a minute
// after its reading, and no poll is made sooner than ServeTTL, three minutes,
// after the last, so the next reading can come later than this files it. An
// account that will stop before that reading then reads as closing until it
// reads as out: "stopping", and the bridge it opens, is skipped. That is the
// two Max, three Ultras pause load's 5 s. Holding next to FetchedAt +
// ServeTTL, as due() does, was measured over the simulated week and not
// taken: stopped with room on another account went from 3562 s to 3739 s in
// all. It lowered that pause load's 5 s to 2 (a different stop), two Max
// under heavy load from 1641 to 1547 and under production load from 136 to
// 104, and two Max, three Ultras under production load from 346 to 345; it
// raised two Max under burst load from 958 to 1107, and two Max, three Ultras
// under burst load from 142 to 250 and under heavy load from 239 to 289; and
// the pause load switched 70 times where this switches 44.
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
// hand-off makes of the LIVE account's runway, with no reading landing and no
// switch: each lead above, counted back from the runway's end. Its runway runs
// from its reading, so the end is fixed between readings, and its next look can
// be overdue -- no poll is made sooner than ServeTTL after the last -- so each
// lead then runs from the clock.
//
// That is the contract, and it is the live account's alone. The runway of an
// account the session is not on runs from now, so its end moves with the clock
// and these instants never come; its verdicts can still change by the clock --
// the carry test while its prompt poll is ahead of now, a bridge as the live
// account nears its stop -- and clockTurns does not report them. The weekly
// simulation, its one reader, asks every second while the live account is at a
// limit, which is when a bridge or a last resort can matter, and its figures
// were measured equal to asking every second throughout.
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
// (pollpolicy.PromptPollAt, the instant the switch writes, shared among the
// accounts of its identity), and that reading's next poll is at most one live
// cadence later, hoverBlind; at that reading the account is closing if the work
// stops it within the reserve. So outlasting the prompt poll, a live cadence
// and the reserve is exactly "the move is not followed by a second move on the
// same rate": TestACarrierIsNotClosingAtItsFirstLiveReading. On a shared
// identity the live cadence is shared too, outside the danger band, and that
// promise holds only for the first reading: the live account's own limit
// (liveLimitOf) reads its real next poll, so the session is not stopped, but it
// may be moved again sooner than a cadence of one would allow.
//
// An account with no runway is a carrier by its level, which is the cold start;
// an empty one, or one on its last point, never is.
func (o Options) carries(c Candidate, r Ranked, rw runway) bool {
	if !r.roomyByLevel() {
		return false
	}
	if !rw.known || rw.at.IsZero() {
		return true
	}
	first := pollpolicy.PromptPollAt(c.FetchedAt, c.NextPollAt, c.LastRateLimited, o.Now, c.SharedBy)
	return rw.at.After(first.Add(carrierLead))
}

// roomyByLevel is whether an account has room by its levels alone: not empty,
// and not on its last point.
func (r Ranked) roomyByLevel() bool {
	if _, empty := r.Empty(); empty {
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
//
// A live login on a credit seat goes back to free quota on the levels alone
// (creditSeatReturn), and money is entered on the levels alone (creditOpen):
// what the user pays for is never decided on a projection.
//
// The caller asks only when the main pool has an account in it. With none, the
// configured strategies' credit arms are the whole of the answer, under hover
// as everywhere else.
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
		p.hoverMoveWithRoom(live, activeUUID, cfg, st, o.Now, byUUID)
		return
	}

	// At a limit: the first carrier, then a bridge if the live account would
	// stop unseen, then a last resort if it has nothing left, then money.
	if target, ok := firstCarrier(res, activeUUID, byUUID, o.Now); ok {
		p.moveOrHold(target, leaveReason(live, target), activeUUID, cfg, st, o.Now)
		return
	}
	if live.limit >= liveStopping {
		if target, ok := bridge(res, live, cfg.Cooldown); ok {
			p.moveOrHold(target, leaveReason(live, target), activeUUID, cfg, st, o.Now)
			return
		}
	}
	if live.lastResortOpen() {
		if target, ok := lastResort(res, live, o.Now); ok {
			p.moveOrHold(target, leaveReason(live, target), activeUUID, cfg, st, o.Now)
			return
		}
	}
	if p.creditOpen() {
		p.creditWalk(byUUID, activeUUID, cfg, st, o.Now)
		return
	}
	p.hold(ReasonNothingCanServe, soonestRollover(res))
}

// hoverMoveWithRoom is the move from a live account with room: to the first
// account ahead of it in the order that can carry the work, on the order's own
// same-rung rules (hoverGate).
//
// The order's rule is asked first and the carry test second, so a hold names
// what actually kept the session: an account the rule would not move to anyway
// is held by that rule (StayUntilLimit, the weekly-reset margin), and only an
// account the rule would move to and the work would run out is "cannot carry".
// The first such reason in the order is the one reported, because it is the
// move the session would otherwise have made -- a later carrier the rule
// refuses is still named as the move held (Held), but not as the reason. A hold
// names its target only when that target carries, so --force never takes the
// session onto an account the work would run out.
// TestTheHoldNamesTheRuleThatKeptTheSession,
// TestTheHoldNamesTheFirstReasonInTheOrder.
func (p *Plan) hoverMoveWithRoom(live Ranked, activeUUID string, cfg Config, st *State, now time.Time, byUUID map[string]Candidate) {
	if p.seatCost(live) > 0 {
		p.creditSeatReturn(live, activeUUID, cfg, st, now, byUUID)
		return
	}
	stay, retryAt := ReasonAlreadyBest, time.Time{}
	kept := false
	for _, target := range p.Result.Order {
		if isActive(target.UUID, activeUUID) {
			break
		}
		reason, refused, retry := hoverGate(live, target, cfg)
		if refused && reason == ReasonAlreadyBest {
			// Not ahead of the live account by the order's own rules.
			continue
		}
		switch {
		case refused && target.Carries:
			if !kept {
				stay, retryAt = reason, retry
			}
			p.hold(stay, retryAt)
			p.Held = target
			return
		case target.Carries:
			p.moveOrHold(target, reason, activeUUID, cfg, st, now)
			return
		case kept:
			continue
		case refused:
			stay, retryAt, kept = reason, retry, true
		default:
			stay, kept = ReasonCannotCarry, true
		}
	}
	p.hold(stay, retryAt)
}

// creditSeatReturn is the move off a live credit seat with room: a seat of the
// last-resort pool, or a primary seat, whose work is paid for by the point. It
// goes back to quota that is cheaper (seatCost) on the levels alone, whatever
// the account's rung -- one held out included -- and whether or not it can
// carry the work: an account that can is taken first, and with none, any that
// is not empty and not on its last point. Free quota is spent before money even
// when that means another move once the free account runs out, and the move
// out of it, back onto credits, is again decided on its levels (creditOpen).
// The cooldown still applies. With no such account the seat stays.
// TestALiveCreditSeatReturnsToFreeQuota,
// TestALiveCreditSeatReturnsToAHeldOutCarrier.
func (p *Plan) creditSeatReturn(live Ranked, activeUUID string, cfg Config, st *State, now time.Time, byUUID map[string]Candidate) {
	cost := p.seatCost(live)
	cheaper := func(r Ranked) bool { return !isActive(r.UUID, activeUUID) && p.seatCost(r) < cost }
	back := func(target Ranked) {
		// The order's own reason when the order would make this move, so the
		// sentence says which rule it is; else why the seat is left.
		reason, refused, _ := hoverGate(live, target, cfg)
		if refused {
			reason = leaveReason(live, target)
		}
		p.moveOrHold(target, reason, activeUUID, cfg, st, now)
	}
	if target, ok := firstCarrier(p.Result, activeUUID, byUUID, now); ok && cheaper(target) {
		back(target)
		return
	}
	for _, r := range p.Result.Order {
		if cheaper(r) && r.roomyByLevel() {
			back(r)
			return
		}
	}
	p.hold(ReasonNoSubscriptionRoom, time.Time{})
}

// seatCost is what a point of work on r costs the user, cheapest first: a
// subscription's quota is paid for whether it is spent or not, a primary credit
// seat's balance is quota bought ahead, and the last-resort pool is overage.
func (p *Plan) seatCost(r Ranked) int {
	switch {
	case p.isCredit(r.UUID):
		return 2
	case r.Kind == identity.KindCredit:
		return 1
	}
	return 0
}

// creditOpen is whether hover may walk the last-resort credit pool, and it is
// decided on readings alone: the user's own thresholds call the main pool
// exhausted (MainPoolExhausted, Plan.SubscriptionExhausted), there is a seat to
// walk, and every account of the main pool -- the live one included -- reads
// empty on a window no model choice can dodge. An account that could not be
// read is not known to be empty, and holds the pool shut.
//
// Never on a projection, a pause or the work's running flag. Each round of
// review found another way for a forecast to spend money while free quota
// remained -- a paused account, a stale running flag, one quantum of rate on a
// seat's last point -- because the decision rested on the forecast. The cost is
// the other direction: with credits opted in and the whole fleet dry, a session
// whose live account runs out between two readings waits for the reading that
// shows it empty, up to the serve floor (pollpolicy.ServeTTL, three minutes)
// after the last one. TestAPausedLiveAccountDoesNotOpenTheCreditPool,
// TestAStaleRunningFlagDoesNotOpenTheCreditPool,
// TestAWeeklySeatsLastPointDoesNotOpenTheCreditPool.
func (p *Plan) creditOpen() bool {
	if !p.SubscriptionExhausted || len(p.Result.Credit) == 0 {
		return false
	}
	for _, r := range p.Result.Order {
		if _, empty := r.Empty(); !empty {
			return false
		}
	}
	return true
}

// isCredit is whether uuid is one of the pass's last-resort credit seats.
func (p *Plan) isCredit(uuid string) bool {
	for _, r := range p.Result.Credit {
		if isActive(r.UUID, uuid) {
			return true
		}
	}
	return false
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

// lastResortOpen is whether the live account's limit opens the last resort:
// empty by level always, and out by projection -- before the engine looks
// again -- only while the work was seen running (Ranked.workRunning).
//
// The work's rate outlives a pause (workRate.replaces), which is right for a
// carrier: a move to an account that can carry the work is safe whether the
// work resumes or not, and the bridge outlasts the live account by a cooldown.
// But after a pause the projection that has the live account out is a forecast
// of a burst that may not come, and a forecast may justify a move that is safe
// either way, not one that buys nothing: two accounts under a cadence of work
// each trading the login every cooldown while nothing runs. Money is not
// decided here at all (creditOpen). TestTwoAccountsDoNotTradeAPausedSession.
func (r Ranked) lastResortOpen() bool {
	if _, empty := r.Empty(); empty {
		return true
	}
	return r.limit == liveOut && r.workRunning
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
func bridge(res Result, live Ranked, cooldown time.Duration) (Ranked, bool) {
	var best Ranked
	found := false
	beyond := live.RunsOutAt.Add(cooldown)
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
//
// An idle fleet whose accounts all sit at their last point used to trade the
// login between them: the live one projected out at the rate the work last ran,
// any other account with work left was a last resort, and the one just left,
// re-read idle, had its dregs back. The last resort now opens on a projection
// only while the live account's latest pair saw the work running
// (Ranked.lastResortOpen), so a pause no longer reaches here at all. What is left
// of the trade is the bridge's: two accounts each holding more than a cooldown
// of work and less than a carrier's can still trade the login across a pause,
// each move a credential write, a prompt poll and a settings sync, and a
// session that resumes right after one waits out the cooldown for the next.
//
// The rule that would stop the trade here even while the work is seen running
// -- a last resort must outlast the live account, holding more of the work than
// the live one does -- was measured and rejected. It raised the seconds a
// session stopped with room elsewhere where work does run: two Max accounts
// under heavy load from 1641 to 3342, under burst load from 958 to 1004, two
// Max and three Ultras under heavy load from 239 to 322 and under production
// load from 346 to 408 (and lowered that fleet's burst figure from 142 to 76).
// The cause is whole-percent resolution between two readings: a live account
// that has really spent its last point still reads the 99 it was read at until
// the next poll, exactly as one that has not, so the rule refuses the dregs of
// another account in the very moment the session needs them. Stopping a
// session outranks idle churn.
func lastResort(res Result, live Ranked, now time.Time) (Ranked, bool) {
	var best Ranked
	found := false
	for _, r := range res.Order {
		if r.UUID == live.UUID {
			continue
		}
		if _, empty := r.Empty(); empty {
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

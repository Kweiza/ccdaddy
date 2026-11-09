package strategy

import (
	"fmt"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/pollpolicy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// maxAcct is a five-hour account: a five_hour window at fivePct resetting in
// fiveIn, beside a week at weekPct resetting in weekIn. It carries no poll
// stamps, so nothing projects for it and only the levels can put it at a limit.
func maxAcct(uuid string, fivePct float64, fiveIn time.Duration, weekPct float64, weekIn time.Duration) Candidate {
	return sub(uuid, &usage.Snapshot{FiveHour: win(fivePct, fiveIn), SevenDay: win(weekPct, weekIn)})
}

const day = 24 * time.Hour

// ---- the tier -------------------------------------------------------------

// A five-hour window nothing has spent against since it rolled reads
// {null,null}. It is the account with the MOST five-hour room it will ever
// have, and the tier is read off the key's presence, not off a utilization it
// has not reported yet.
func TestAFiveHourKeyMakesAnAccountShortWindowEvenWhenItIsUnspent(t *testing.T) {
	fresh := sub("a-max", &usage.Snapshot{FiveHour: usage.NewWindow(nil, nil), SevenDay: win(59, 4*day)})
	r := Rank([]Candidate{fresh}, hoverOpts()).Order[0]
	if r.ShortWindow != usage.WindowFiveHour {
		t.Errorf("ShortWindow = %q, want five_hour: the key is there, it is only unspent", r.ShortWindow)
	}
	if got := r.Rung(); got != RungShortWindow {
		t.Errorf("rung = %d, want RungShortWindow", got)
	}
}

// A verified Ultra seat sends five_hour null. Every window it carries is a
// week, so it is weekly-only however many weekly windows it has.
func TestAnUltraSeatIsWeeklyOnly(t *testing.T) {
	ultra := sub("u", &usage.Snapshot{SevenDay: win(40, 5*day), SevenDayOpus: win(10, 5*day)})
	r := Rank([]Candidate{ultra}, hoverOpts()).Order[0]
	if r.ShortWindow != "" {
		t.Errorf("ShortWindow = %q, want none: a week is not a short window", r.ShortWindow)
	}
	if got := r.Rung(); got != RungWeeklyOnly {
		t.Errorf("rung = %d, want RungWeeklyOnly", got)
	}
}

// ---- the order ------------------------------------------------------------

// A five-hour account comes first even when a weekly-only seat holds more room
// and its week ends sooner: the five-hour quota comes back every five hours, so
// a point of it not spent now is a point thrown away.
func TestShortWindowAccountsOutrankWeeklyOnlyOnesWhateverTheReset(t *testing.T) {
	pool := []Candidate{
		weekOnly("u-ultra", 10, 1*day),
		maxAcct("a-max", 10, 3*time.Hour, 50, 4*day),
	}
	eq(t, order(Rank(pool, hoverOpts())), []string{"a-max", "u-ultra"})
}

// Among five-hour accounts the one whose week ends first is entered first, and
// how much room either holds does not come into it.
func TestAmongShortWindowAccountsTheSoonestWeeklyResetLeads(t *testing.T) {
	pool := []Candidate{
		maxAcct("a-roomy", 10, 3*time.Hour, 20, 4*day),
		maxAcct("b-sooner", 60, 3*time.Hour, 70, 1*day),
	}
	eq(t, order(Rank(pool, hoverOpts())), []string{"b-sooner", "a-roomy"})
}

// The rule hover exists to keep: a five-hour account is left only at a limit,
// never for another five-hour account, however much sooner its week ends.
func TestAShortWindowAccountIsNotLeftForAnotherShortWindowAccount(t *testing.T) {
	pool := []Candidate{
		maxAcct("a-live", 30, 3*time.Hour, 50, 4*day),
		maxAcct("b-sooner", 10, 3*time.Hour, 20, 1*day),
	}
	p := Decide(pool, hoverOpts(), Config{}, NewState(), "a-live")
	if got := p.Result.Order[0].UUID; got != "b-sooner" {
		t.Fatalf("order[0] = %s; the fixture needs the other account ranked first", got)
	}
	want(t, p, ActionStay, ReasonStayUntilLimit, "")
}

// Every rung in its place: room on a five-hour account, room on a week, then
// the account nobody could read -- a maybe beats a no -- then the account at a
// limit, then the one with nothing left.
func TestAnUnreadableAccountSitsBehindKnownRoomAndAheadOfALimit(t *testing.T) {
	pool := []Candidate{
		weekOnly("e-empty", 100, 1*day),
		{UUID: "c-unread", Kind: identity.KindSubscription},
		maxAcct("d-last-point", 99, 3*time.Hour, 50, 1*day),
		weekOnly("b-ultra", 40, 1*day),
		maxAcct("a-max", 10, 3*time.Hour, 50, 4*day),
	}
	eq(t, order(Rank(pool, hoverOpts())), []string{"a-max", "b-ultra", "c-unread", "d-last-point", "e-empty"})
}

// A primary credit seat carries no window at all, so it is weekly-only with no
// weekly reset: behind every seat whose week perishes, because credits do not,
// and still ahead of an account at its limit.
func TestAPrimaryCreditSeatIsTheBackstopUnderHover(t *testing.T) {
	seat := primarySeat("c-seat", 41)
	pool := []Candidate{
		seat,
		weekOnly("d-last-point", 99, 1*day),
		weekOnly("b-ultra", 40, 5*day),
	}
	eq(t, order(Rank(pool, hoverOpts())), []string{"b-ultra", "c-seat", "d-last-point"})
}

// ---- the limits -----------------------------------------------------------

// The last point is the same line on a five-hour window as on a week. It used to
// be a week's alone, and a five-hour account run to its limit on purpose needs
// it as much.
func TestAShortWindowAccountAtItsLastPointHandsOffToTheNextShortWindowAccount(t *testing.T) {
	pool := []Candidate{
		maxAcct("a-live", 99, 4*time.Hour, 50, 4*day),
		maxAcct("b-max", 20, 4*time.Hour, 30, 2*day),
		weekOnly("u-ultra", 10, 1*day),
	}
	p := Decide(pool, hoverOpts(), Config{}, NewState(), "a-live")
	want(t, p, ActionSwitch, ReasonLastPoint, "b-max")
	if p.Departure != (Departure{From: "a-live", Closing: usage.WindowFiveHour}) {
		t.Errorf("Departure = %+v, want a-live left on its five-hour window", p.Departure)
	}
}

// With no five-hour account holding room, the next place is the weekly-only
// seat whose week ends first.
func TestAShortWindowAccountAtItsLastPointHandsOffToAnUltraWhenNoShortHasRoom(t *testing.T) {
	pool := []Candidate{
		maxAcct("a-live", 99, 4*time.Hour, 50, 4*day),
		weekOnly("u-later", 10, 5*day),
		weekOnly("u-sooner", 10, 2*day),
	}
	want(t, Decide(pool, hoverOpts(), Config{}, NewState(), "a-live"), ActionSwitch, ReasonLastPoint, "u-sooner")
}

// The purpose-2 case for the new order. A live five-hour account with room is
// first in hover's order, and the pre-emptive walk ends at the live account --
// so if a closing account were not on the limit rung, the walk would end before
// it began and the session would run into the wall with an Ultra seat roomy
// beside it.
func TestAShortWindowAccountAboutToRunOutIsPreemptedOntoTheBackstop(t *testing.T) {
	// Ten points left, burning five a minute, read a minute ago and next read in
	// a minute: two minutes of work against a horizon the observed gaps make
	// four minutes long.
	live := polled(maxAcct("a-live", 90, 4*time.Hour, 50, 4*day), 60*time.Second)
	live.BurnPerMin, live.HasBurn, live.BurnWindow = 5, true, usage.WindowFiveHour
	pool := []Candidate{live, weekOnly("u-ultra", 20, 2*day)}

	p := Decide(pool, hoverOpts(), Config{}, NewState(), "a-live")
	want(t, p, ActionSwitch, ReasonProjectedExhaustion, "u-ultra")
	if p.Departure != (Departure{From: "a-live", Closing: usage.WindowFiveHour}) {
		t.Errorf("Departure = %+v, want a-live left on the window that was closing", p.Departure)
	}
}

// A blown cap on ONE model family is not a limit: it stops that family, not the
// session, and OutOfQuota's own reading is the one the last point takes.
//
// The account is LIVE and read on a schedule, because that is the one reading a
// projection is asked of: a blown cap projects its own exhaustion as now, so a
// projection over the cap would file the account at a limit, leave it, and mark
// it until the Fable week rolls.
func TestAModelScopedCapIsNotALimitUnderHover(t *testing.T) {
	live := polled(sub("a-live", &usage.Snapshot{
		SevenDay: win(80, 1*day),
		Limits:   []usage.Limit{scoped("Fable", "", 100, 3*day)},
	}), 180*time.Second)
	pool := []Candidate{live, idleOn(weekOnly("u-ultra", 20, 3*day))}

	p := Decide(pool, hoverOpts(), Config{}, NewState(), "a-live")
	if r, _ := find(p.Result, "a-live"); r.LastPoint != "" || r.AtLimit() != "" {
		t.Errorf("LastPoint = %q, at limit on %q: a blown Fable cap leaves a fifth of the week for every other model",
			r.LastPoint, r.AtLimit())
	}
	want(t, p, ActionStay, ReasonAlreadyBest, "")
}

// When every account's Fable week is gone the pool has not run out: hover's order
// is still short-window first, and a live account at its last point is still
// handed off before the limit rather than at it.
func TestEveryFableWeekBlownStillHandsOffBeforeTheLimit(t *testing.T) {
	fable := func(c Candidate) Candidate {
		c.Usage.Limits = []usage.Limit{scoped("Fable", "", 100, 2*day)}
		return c
	}
	pool := []Candidate{
		fable(idleOn(weekOnly("u-ultra", 50, 1*day))),
		fable(polled(maxAcct("a-max", 99, 3*time.Hour, 50, 4*day), 180*time.Second)),
		fable(idleOn(maxAcct("b-max", 10, 3*time.Hour, 50, 5*day))),
	}
	p := Decide(pool, hoverOpts(), Config{}, NewState(), "a-max")
	eq(t, order(p.Result), []string{"b-max", "u-ultra", "a-max"})
	if p.Action != ActionSwitch || p.Target.UUID != "b-max" {
		t.Errorf("%s (%s) to %q, want the five-hour account with room", p.Action, p.Reason, p.Target.UUID)
	}
}

// ModeRecovery under hover means every account is past its last point -- the
// pool has nothing left to order, and when it comes back is the question. An
// account ON its last point is not past it.
func TestEveryAccountPastItsLastPointIsRecoveryModeUnderHover(t *testing.T) {
	for _, tc := range []struct {
		name string
		pct  float64
		want Mode
	}{
		{"every account empty", 100, ModeRecovery},
		{"one account past its last point", 99.5, ModeRecovery},
		{"one account on its last point", 99, ModeHover},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := []Candidate{weekOnly("a", 100, 1*day), weekOnly("b", tc.pct, 2*day)}
			if got := Rank(pool, hoverOpts()).Mode; got != tc.want {
				t.Errorf("Mode = %v, want %v", got, tc.want)
			}
		})
	}
}

// A cap on one model family is a limit the moment the evaluation names that
// family: a session that will only run Opus cannot go on with another model, so
// an account whose Opus week is gone has nothing to give it. With no model named
// the same account holds most of a week for every other family, and the cap is
// not a limit at all. The fixed seven_day_opus window and a scoped Fable cap are
// the two shapes a family cap arrives in.
func TestANamedModelsOwnCapIsALimitUnderHover(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model string
		capOn func(*usage.Snapshot)
		cap   usage.WindowName
	}{
		{"seven_day_opus", "opus",
			func(s *usage.Snapshot) { s.SevenDayOpus = win(100, 2*day) }, usage.WindowSevenDayOpus},
		{"a scoped Fable week", "fable",
			func(s *usage.Snapshot) { s.Limits = []usage.Limit{scoped("Fable", "", 100, 2*day)} },
			"weekly_scoped:model:Fable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acct := maxAcct("a-max", 10, 3*time.Hour, 40, 4*day)
			tc.capOn(acct.Usage)

			named := hoverOpts()
			named.Model = tc.model
			if r := Rank([]Candidate{acct}, named).Order[0]; r.AtLimit() != tc.cap || r.Rung() != RungEmpty {
				t.Errorf("--model %s: at a limit on %q at rung %s, want empty on %s", tc.model, r.AtLimit(), r.Rung(), tc.cap)
			}
			if r := Rank([]Candidate{acct}, hoverOpts()).Order[0]; r.AtLimit() != "" || r.Rung() != RungShortWindow {
				t.Errorf("no model: at a limit on %q at rung %s, want a short-window account with room", r.AtLimit(), r.Rung())
			}
		})
	}
}

// A blown cap on one model family is not a limit, and that holds for the mode as
// well as for the rung. A fleet whose Fable weeks are all gone, every account
// holding half a week for every other model, is not a pool with nothing left:
// ranked on soonest recovery it would be ordered on when the Fable caps come
// back, and the five-hour account would lose its place to whichever cap rolls
// first.
func TestABlownModelCapEverywhereDoesNotPutHoverIntoRecovery(t *testing.T) {
	fable := func(c Candidate) Candidate {
		c.Usage.Limits = []usage.Limit{scoped("Fable", "", 100, 2*day)}
		return c
	}
	// Both read on a schedule and the five-hour account live, so the projection
	// is asked of every account it can be: a fixture with no stamps could not
	// reach it and would pass under the defect.
	pool := []Candidate{
		fable(idleOn(weekOnly("u-ultra", 50, 1*day))),
		fable(polled(maxAcct("a-max", 10, 3*time.Hour, 50, 4*day), 180*time.Second)),
	}
	r := Decide(pool, hoverOpts(), Config{}, NewState(), "a-max").Result
	if r.Mode != ModeHover {
		t.Errorf("Mode = %v, want %v: every account still has half a week for every model but one", r.Mode, ModeHover)
	}
	eq(t, order(r), []string{"a-max", "u-ultra"})
}

// idleOn is an account read the way the daemon reads one nobody is spending: on
// the ten-minute idle cadence, with no rate of its own.
func idleOn(c Candidate) Candidate {
	return polled(c, 600*time.Second)
}

// measuredLive is an account with the work's rate measured on it the way the
// poller measures it: across two readings both taken live (usage.LiveBurn), the
// later at now, with the same rate as its own last-pair burn.
func measuredLive(c Candidate, perMin float64, on usage.WindowName) Candidate {
	c.BurnPerMin, c.HasBurn, c.BurnWindow = perMin, true, on
	c.LiveBurns = []usage.LiveBurn{{PerMin: perMin, Window: on, From: now.Add(-3 * time.Minute), At: now}}
	c.WorkRunning, c.LastPairAt = c.LiveBurns[0].Running(), c.LiveBurns[0].At
	return c
}

// workingAt is the live account with the work's rate measured on it, and no poll
// stamps -- so nothing projects for it and only its level can put it at a
// limit, which isolates the hand-off this file is about from the projection.
// The rate is still the work's, and every other account is judged on it.
func workingAt(c Candidate, perMin float64, on usage.WindowName) Candidate {
	return measuredLive(c, perMin, on)
}

// A session at its last point is handed off to anything better -- and NOT onto
// an account that is itself at a limit by its level, because the move buys
// nothing there: one on its own last point, or one with nothing left (the
// eighteen-switch incident). With no projection for the live account -- its
// reading carries no stamps -- nothing can say it is about to stop, so neither
// tier that reaches past a carrier applies, and the engine wanted to move and
// had nowhere to go: blocked, not a quiet stay.
func TestTheLastPointHandOffDoesNotMoveOntoALimit(t *testing.T) {
	live := workingAt(maxAcct("a-live", 99, 4*time.Hour, 50, 4*day), 1, usage.WindowFiveHour)
	for _, tc := range []struct {
		name   string
		target Candidate
	}{
		{"a target on its own last point", idleOn(maxAcct("b", 99, 4*time.Hour, 50, 1*day))},
		{"an empty target", idleOn(maxAcct("b", 100, 4*time.Hour, 50, 1*day))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Decide([]Candidate{live, tc.target}, hoverOpts(), Config{}, NewState(), "a-live")
			if b, _ := find(p.Result, "b"); b.Carries {
				t.Fatalf("b carries the work; the fixture is not testing what it says")
			}
			if p.Action != ActionBlocked || p.Reason != ReasonNothingCanServe {
				t.Errorf("%s (%s) to %q, want blocked: the live account wanted to move and had nowhere better",
					p.Action, p.Reason, p.Target.UUID)
			}
		})
	}
}

// R1, the case the round-1 rule failed: two five-hour accounts, the Ultras
// absent, the work at 0.84 points a minute. A was left at a limit and is held
// out; B is live and read on a three-minute cadence. Whether B may hand off to A
// is a question about the two of them, asked in tiers: A as a carrier if it
// would not be at a limit at its first live reading, as a bridge if B would stop
// before its next reading and A outlasts it by a cooldown, and as a last resort
// if B has nothing left at all. The points are derived from the horizons, so the
// tiers hold for any reserve the poll policy sets.
func TestALastPointHandsOffToAnAccountThatOutlastsIt(t *testing.T) {
	const rate = 0.84
	points := func(d time.Duration) float64 { return rate * d.Minutes() }
	// A is read idle at now and next at the idle cadence, so its prompt poll is
	// the serve floor after now.
	carrierNeeds := pollpolicy.ServeTTL + hoverBlind + hoverReserve
	cadence := 180 * time.Second

	for _, tc := range []struct {
		name       string
		bRoom      float64 // B's points left, live
		aRoom      float64 // A's points left, held out
		wantSwitch bool
		reason     Reason
	}{
		{"B with room stays", points(cadence + hoverReserve + 5*time.Minute), points(carrierNeeds + 2*time.Minute), false, ReasonAlreadyBest},
		{"B closing hands to A, a carrier", points(cadence + hoverReserve/2), points(carrierNeeds + 2*time.Minute), true, ReasonProjectedExhaustion},
		{"B closing does not take a bridge", points(cadence + hoverReserve/2), points(cadence + HoverCooldown + 3*time.Minute), false, ReasonNothingCanServe},
		{"B stopping hands to A, a bridge", points(cadence), points(cadence + HoverCooldown + time.Minute), true, ReasonProjectedExhaustion},
		{"B stopping keeps a bridge short of a cooldown", points(cadence), points(cadence + HoverCooldown/2), false, ReasonNothingCanServe},
		{"B out hands to A, a last resort", 0.005, points(HoverCooldown / 2), true, ReasonLastPoint},
		{"B out never takes an empty A", 0.005, 0, false, ReasonNothingCanServe},
		{"B out never takes an A the work would empty before the next look", 0.005, points(EvaluationInterval / 2), false, ReasonNothingCanServe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := measuredLive(polled(maxAcct("b-live", 100-tc.bRoom, 3*time.Hour, 50, 4*day), cadence), rate, usage.WindowFiveHour)
			a := idleOn(maxAcct("a-left", 100-tc.aRoom, 3*time.Hour, 50, 3*day))
			p := Decide([]Candidate{a, b}, hoverOpts(), Config{}, leftAt("a-left", usage.WindowFiveHour), "b-live")
			if tc.wantSwitch {
				want(t, p, ActionSwitch, tc.reason, "a-left")
				return
			}
			if p.Action == ActionSwitch || p.Reason != tc.reason {
				t.Errorf("%s (%s) to %q, want %s", p.Action, p.Reason, p.Target.UUID, tc.reason)
			}
		})
	}
}

// What the root cause of round one was, and what the rate source has to get
// right: an account the work is not on is judged on the WORK's rate, never on its
// own. A1 was left at 90% and its reading still carries the five points a minute
// it was burning when it was live; A2 is live now, at its last point, with a
// newer measurement of the work. The newer one is the work's rate, so A1 outlasts
// A2 and takes the session before the limit -- at 0.3 a minute as a carrier, and
// at 0.84 a minute, when A2 would stop before its next reading, as a bridge.
func TestALastPointHandsOffToAnAccountWhoseOwnRatesWouldHaveCondemnedIt(t *testing.T) {
	for _, rate := range []float64{0.3, 0.84} {
		t.Run(fmt.Sprintf("%.2f a minute", rate), func(t *testing.T) {
			a1 := idleOn(maxAcct("a1-left", 90, 4*time.Hour, 40, 3*day))
			a1.BurnPerMin, a1.HasBurn, a1.BurnWindow = 5, true, usage.WindowFiveHour
			a1.LiveBurns = []usage.LiveBurn{{PerMin: 5, Window: usage.WindowFiveHour,
				From: now.Add(-2 * time.Hour), At: now.Add(-time.Hour)}}
			a2 := measuredLive(polled(maxAcct("a2-live", 99, 3*time.Hour, 40, 4*day), 180*time.Second),
				rate, usage.WindowFiveHour)
			ultra := idleOn(weekOnly("u-empty", 100, 2*day))

			p := Decide([]Candidate{a1, a2, ultra}, hoverOpts(), Config{}, leftAt("a1-left", usage.WindowFiveHour), "a2-live")
			if r, _ := find(p.Result, "a1-left"); r.AtLimit() != "" || r.HeldOut == "" {
				t.Fatalf("a1-left at a limit on %q (held on %q): its own rates are condemning it again", r.AtLimit(), r.HeldOut)
			}
			want(t, p, ActionSwitch, ReasonLastPoint, "a1-left")
		})
	}
}

// The ordinary move may not take a target the hand-off would refuse. An Ultra
// at 98.5% is one and a half points from a limit the work spends at a point a
// minute: it does not carry the work, and the one verdict every move reads says
// so -- which once handed the session to an account that stopped it ninety
// seconds later, with a two-minute cooldown holding it there.
func TestTheOrdinaryMoveDoesNotTakeATargetTheWalkRefused(t *testing.T) {
	a := measuredLive(polled(weekOnly("a-live", 95, 3*day), 180*time.Second), 1, usage.WindowSevenDay)
	b := idleOn(weekOnly("b-ultra", 98.5, 1*day))

	p := Decide([]Candidate{a, b}, hoverOpts(), Config{}, NewState(), "a-live")
	if r, _ := find(p.Result, "b-ultra"); r.Carries {
		t.Fatalf("b-ultra carries the work; the work would run it out in ninety seconds")
	}
	if p.Action == ActionSwitch {
		t.Errorf("switched to %s (%s): it cannot carry the work", p.Target.UUID, p.Reason)
	}
}

// The walk turns away a target on its last point. Ahead of a closing Max on the
// limit rung sorts an Ultra on its last point whose week ends sooner; taking it
// moved the session A to B, and B's last point moved it straight back.
func TestPreemptionDoesNotRunToAnAccountOnItsLastPoint(t *testing.T) {
	a := polled(maxAcct("a-live", 85, 50*time.Minute, 50, 4*day), 180*time.Second)
	a.BurnPerMin, a.HasBurn, a.BurnWindow = 3, true, usage.WindowFiveHour
	b := idleOn(weekOnly("b-ultra", 99, 3*day))

	p := Decide([]Candidate{a, b}, hoverOpts(), Config{}, NewState(), "a-live")
	if order(p.Result)[0] != "b-ultra" {
		t.Fatalf("order = %v; the fixture needs the last-point Ultra ranked ahead of the live Max", order(p.Result))
	}
	if p.Action == ActionSwitch {
		t.Errorf("switched to %s (%s): an account on its last point is no place to pre-empt to", p.Target.UUID, p.Reason)
	}
}

// A window that resets before it would run out is not closing: the reset hands
// back a whole window first. Leaving it costs two switches -- out now, back at
// the rollover -- for a stop that was never going to happen. The same holds for
// a target: one whose window resets inside the horizon before the work would run
// it out carries the work.
func TestAWindowThatResetsBeforeItRunsOutIsNotClosing(t *testing.T) {
	// Three points at half a point a minute is six minutes; the window resets in
	// four.
	live := polled(maxAcct("a-live", 97, 4*time.Minute, 50, 4*day), 60*time.Second)
	live.BurnPerMin, live.HasBurn, live.BurnWindow = 0.5, true, usage.WindowFiveHour
	pool := []Candidate{live, idleOn(weekOnly("u-ultra", 20, 2*day))}
	if p := Decide(pool, hoverOpts(), Config{}, NewState(), "a-live"); p.Action != ActionStay {
		t.Errorf("%s (%s) to %q, want a stay: the window resets before it runs out", p.Action, p.Reason, p.Target.UUID)
	}

	// At two points a minute the live account runs out half a reserve after its
	// next reading, a minute away: it is closing, whatever the reserve. The
	// target has four points the work would spend in two minutes -- but its
	// window resets in ninety seconds.
	room := 2 * (time.Minute + hoverReserve/2).Minutes()
	closing := polled(maxAcct("a-live", 100-room, 3*time.Hour, 50, 4*day), 60*time.Second)
	closing.BurnPerMin, closing.HasBurn, closing.BurnWindow = 2, true, usage.WindowFiveHour
	resets := idleOn(maxAcct("b-resets", 96, 90*time.Second, 40, 3*day))
	p := Decide([]Candidate{closing, resets}, hoverOpts(), Config{}, NewState(), "a-live")
	want(t, p, ActionSwitch, ReasonProjectedExhaustion, "b-resets")
}

// A reading is as old as its poll. An account read empty on a five-hour window
// that rolled a minute ago, and not polled since, has a fresh window -- and
// under hover, whose levels are the whole of how it files an account the
// session is not on, it is filed as the fresh window it is: a short-window
// account with room, which the session on an Ultra moves to. The configured
// strategies rank on the reading as it was taken.
func TestARolledReadingIsReadAsRolled(t *testing.T) {
	rolled := maxAcct("a-rolled", 100, -time.Minute, 40, 3*day)
	pool := []Candidate{rolled, idleOn(weekOnly("u-live", 30, 2*day))}

	r := Rank(pool, hoverOpts())
	if len(r.Order) == 0 || r.Order[0].UUID != "a-rolled" || r.Order[0].Rung() != RungShortWindow {
		t.Fatalf("hover order %v, want the rolled account first as a short-window account", order(r))
	}
	if got := r.Order[0].Headroom.Pct; got != 60 {
		t.Errorf("rolled account's headroom %v under hover, want the week's 60: the five-hour window is fresh", got)
	}
	p := Decide(pool, hoverOpts(), Config{}, NewState(), "u-live")
	want(t, p, ActionSwitch, ReasonShortWindowRoom, "a-rolled")

	for _, c := range Rank(pool, opts()).Order {
		if c.UUID == "a-rolled" && c.Headroom.Pct != 0 {
			t.Errorf("rolled account's headroom %v under a configured strategy, want the reading's 0", c.Headroom.Pct)
		}
	}
}

// Credits do not come back, so a primary credit seat is handed off at the line
// hover sets for its balance -- the last few points are the only warning it gives
// -- and not one point short of empty like a plan window.
func TestAPrimaryCreditSeatIsAtALimitAtHoversCreditThreshold(t *testing.T) {
	for _, tc := range []struct {
		pct  float64
		want bool
	}{{96, true}, {95, true}, {94, false}} {
		r := Rank([]Candidate{primarySeat("c-seat", tc.pct)}, hoverOpts()).Order[0]
		if got := r.AtLimit() != ""; got != tc.want {
			t.Errorf("seat at %v%%: at a limit = %v, want %v", tc.pct, got, tc.want)
		}
	}
}

// The reason for leaving is what was wrong with staying. A live account the
// projection has at its limit, left by the ordinary move because the walk would
// not take the only other account -- nobody could read it -- reports the
// projection, not "a better target".
func TestTheReasonForLeavingIsTheLiveAccountsLimit(t *testing.T) {
	// Three points at two a minute is ninety seconds, inside a horizon of two
	// minutes -- the one observed gap plus the least lead.
	live := polled(maxAcct("a-live", 97, 3*time.Hour, 50, 4*day), 60*time.Second)
	live.BurnPerMin, live.HasBurn, live.BurnWindow = 2, true, usage.WindowFiveHour
	unread := Candidate{UUID: "b-unread", Kind: identity.KindSubscription}

	want(t, Decide([]Candidate{live, unread}, hoverOpts(), Config{}, NewState(), "a-live"),
		ActionSwitch, ReasonProjectedExhaustion, "b-unread")
}

// A live account at a limit with nowhere better is one state, whichever account
// sorts first between two at a limit: blocked, until the soonest rollover. Two
// unreadable accounts are not that state -- neither is known to be at anything.
func TestALiveAccountAtALimitWithNowhereBetterIsBlocked(t *testing.T) {
	soonest := now.Add(3 * time.Hour)
	for _, tc := range []struct {
		name  string
		other Candidate
	}{
		{"the other account sorts first", maxAcct("b", 99, 3*time.Hour, 50, 1*day)},
		{"the live account sorts first", maxAcct("b", 99, 3*time.Hour, 50, 6*day)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := maxAcct("a-live", 99, 4*time.Hour, 50, 4*day)
			p := Decide([]Candidate{live, tc.other}, hoverOpts(), Config{}, NewState(), "a-live")
			if p.Action != ActionBlocked || p.Reason != ReasonNothingCanServe {
				t.Fatalf("%s (%s), want blocked: nothing can serve", p.Action, p.Reason)
			}
			if !p.HasRetryAt || !p.RetryAt.Equal(soonest) {
				t.Errorf("RetryAt = %v (has %v), want the soonest rollover %v", p.RetryAt, p.HasRetryAt, soonest)
			}
		})
	}

	unread := []Candidate{{UUID: "a-live", Kind: identity.KindSubscription}, {UUID: "b", Kind: identity.KindSubscription}}
	want(t, Decide(unread, hoverOpts(), Config{}, NewState(), "a-live"), ActionStay, ReasonAlreadyBest, "")
}

// ---- weekly-only seats ----------------------------------------------------

// Among weekly-only seats the session always goes to the one whose week ends
// first: that is the quota about to be lost.
func TestAmongUltrasTheSoonestWeeklyResetIsMovedTo(t *testing.T) {
	pool := []Candidate{
		weekOnly("u1-live", 40, 5*day+17*time.Hour),
		weekOnly("u2-sooner", 40, 5*day+10*time.Hour),
	}
	p := Decide(pool, hoverOpts(), Config{}, NewState(), "u1-live")
	want(t, p, ActionSwitch, ReasonSoonerWeeklyReset, "u2-sooner")
	if p.Departure.Closing != "" {
		t.Errorf("Departure = %+v: a move to a sooner week leaves no account at a limit", p.Departure)
	}
}

// Two seats whose weeks end seconds apart would otherwise trade the session on
// any jitter in resets_at, and five minutes of difference in when a week
// expires is worth nothing.
func TestTwoUltrasSecondsApartDoNotPingPong(t *testing.T) {
	pool := []Candidate{
		weekOnly("u1", 40, 5*day+17*time.Hour),
		weekOnly("u2", 40, 5*day+17*time.Hour-10*time.Second),
	}
	want(t, Decide(pool, hoverOpts(), Config{}, NewState(), "u1"), ActionStay, ReasonWeeklyResetMargin, "")
	want(t, Decide(pool, hoverOpts(), Config{}, NewState(), "u2"), ActionStay, ReasonAlreadyBest, "")
}

// At the instant the live seat's week resets, its cached reading still names a
// reset that is now in the past. That seat holds a fresh week -- the least
// perishable quota in the pool -- and the next soonest week is where the
// session belongs.
func TestWhenTheLiveUltrasWeekResetsTheNextSoonestTakesOver(t *testing.T) {
	stale := weekOnly("u1-live", 70, -time.Minute)
	pool := []Candidate{stale, weekOnly("u2", 40, 2*day), weekOnly("u3", 40, 5*day)}
	want(t, Decide(pool, hoverOpts(), Config{}, NewState(), "u1-live"), ActionSwitch, ReasonSoonerWeeklyReset, "u2")

	// And once the refresh lands, a week out, the session stays where it went.
	refreshed := []Candidate{weekOnly("u1", 0, 7*day-time.Minute), weekOnly("u2-live", 40, 2*day), weekOnly("u3", 40, 5*day)}
	want(t, Decide(refreshed, hoverOpts(), Config{}, NewState(), "u2-live"), ActionStay, ReasonAlreadyBest, "")
}

// The margin between weekly-only seats is the user's recovery_hysteresis, so a
// user who wants seats to trade places only when a week ends an hour sooner can
// say so -- and the default still moves for thirty minutes.
func TestHoverHonoursTheUsersRecoveryHysteresisAsTheUltraMargin(t *testing.T) {
	pool := []Candidate{
		weekOnly("u1-live", 40, 5*day),
		weekOnly("u2", 40, 5*day-30*time.Minute),
	}
	want(t, Decide(pool, hoverOpts(), Config{RecoveryHysteresis: time.Hour}, NewState(), "u1-live"),
		ActionStay, ReasonWeeklyResetMargin, "")
	want(t, Decide(pool, hoverOpts(), Config{}, NewState(), "u1-live"),
		ActionSwitch, ReasonSoonerWeeklyReset, "u2")
}

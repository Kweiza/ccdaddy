package strategy

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/identity"
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
func TestAModelScopedCapIsNotALimitUnderHover(t *testing.T) {
	c := sub("a", &usage.Snapshot{
		SevenDay: win(80, 3*day),
		Limits:   []usage.Limit{scoped("Fable", "", 100, 3*day)},
	})
	r := Rank([]Candidate{c}, hoverOpts()).Order[0]
	if r.LastPoint || r.AtLimit() != "" {
		t.Errorf("LastPoint = %v, at limit on %q: a blown Fable cap leaves a fifth of the week for every other model",
			r.LastPoint, r.AtLimit())
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
	pool := []Candidate{
		fable(weekOnly("u-ultra", 50, 1*day)),
		fable(maxAcct("a-max", 10, 3*time.Hour, 50, 4*day)),
	}
	r := Rank(pool, hoverOpts())
	if r.Mode != ModeHover {
		t.Errorf("Mode = %v, want %v: every account still has half a week for every model but one", r.Mode, ModeHover)
	}
	eq(t, order(r), []string{"a-max", "u-ultra"})
}

// The last-point waiver asks the target the whole of "at a limit". A target
// projected to run out before its next reading is no more somewhere to go than
// one on its own last point, and the order then keeps the session where it is.
func TestTheLastPointWaiverDoesNotFireOntoAnAccountAtALimit(t *testing.T) {
	// 95% an hour into its five-hour window, read on a three-minute cadence:
	// five points at 95 an hour is three minutes, inside its horizon.
	closing := polled(maxAcct("b-closing", 95, 4*time.Hour, 50, 1*day), 180*time.Second)
	pool := []Candidate{maxAcct("a-live", 99, 4*time.Hour, 50, 4*day), closing}

	p := Decide(pool, hoverOpts(), Config{}, NewState(), "a-live")
	if r, _ := find(p.Result, "b-closing"); r.Closing == "" || p.Result.Order[0].UUID != "b-closing" {
		t.Fatalf("order = %v, b-closing closing on %q; the fixture needs a closing account ranked first",
			order(p.Result), r.Closing)
	}
	if p.Action == ActionSwitch {
		t.Errorf("switched from a last point onto %s, which is itself about to run out", p.Target.UUID)
	}
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

package strategy

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/pollpolicy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// runsOutIn is how long after now a ranked account's runway ends.
func runsOutIn(t *testing.T, r Ranked) time.Duration {
	t.Helper()
	if !r.HasRunway || r.RunsOutAt.IsZero() {
		t.Fatalf("%s has no runway (has %v, at %v)", r.UUID, r.HasRunway, r.RunsOutAt)
	}
	return r.RunsOutAt.Sub(now)
}

// The work's rate is the most recent one measured across two LIVE readings,
// whichever account it was measured on -- not the live account's own last-pair
// figure, which right after a switch is the idle zero the new account was read at,
// and not the largest measurement anywhere, which is the work as it once was.
func TestTheWorksRateIsTheMostRecentLiveMeasurement(t *testing.T) {
	// L was left a minute ago; its last live pair measured half a point a minute.
	l := idleOn(maxAcct("l-left", 60, 3*time.Hour, 40, 3*day))
	l.LiveBurns = []usage.LiveBurn{{PerMin: 0.5, Window: usage.WindowFiveHour, From: now.Add(-4 * time.Minute), At: now.Add(-time.Minute)}}
	// T is live and was read idle: a measured zero, and no live pair.
	tl := idleOn(maxAcct("t-live", 10, 4*time.Hour, 30, 4*day))
	tl.BurnPerMin, tl.HasBurn, tl.BurnWindow = 0, true, usage.WindowFiveHour
	c := idleOn(maxAcct("c", 70, 4*time.Hour, 30, 4*day))

	p := Decide([]Candidate{l, tl, c}, hoverOpts(), Config{}, NewState(), "t-live")
	r, _ := find(p.Result, "c")
	if got := runsOutIn(t, r); math.Abs(got.Minutes()-30/0.5) > 1e-6 {
		t.Errorf("c runs out in %s, want 60m: its thirty points at the half point a minute measured live", got)
	}

	// A later live pair on T wins over an older, higher one.
	tl.LiveBurns = []usage.LiveBurn{{PerMin: 0.3, Window: usage.WindowFiveHour, From: now.Add(-3 * time.Minute), At: now}}
	l.LiveBurns[0].PerMin = 5
	p = Decide([]Candidate{l, tl, c}, hoverOpts(), Config{}, NewState(), "t-live")
	r, _ = find(p.Result, "c")
	if got := runsOutIn(t, r); math.Abs(got.Minutes()-30/0.3) > 1e-6 {
		t.Errorf("c runs out in %s, want 100m: the newest live pair is the work's rate", got)
	}

	// A newer pause on T is not a rate: the work's rate is the older running
	// one on L, the rate the work runs at when it runs.
	tl.LiveBurns = []usage.LiveBurn{{PerMin: 0, Window: usage.WindowFiveHour, From: now.Add(-5 * time.Minute), At: now}}
	l.LiveBurns[0].PerMin = 0.5
	p = Decide([]Candidate{l, tl, c}, hoverOpts(), Config{}, NewState(), "t-live")
	r, _ = find(p.Result, "c")
	if got := runsOutIn(t, r); math.Abs(got.Minutes()-30/0.5) > 1e-6 {
		t.Errorf("c runs out in %s after a pause, want 60m: a pause is not working slower", got)
	}
}

// A pair whose first reading precedes the last switch onto its account spans
// the time the work was elsewhere, and is never the work's rate -- even when it
// was stored, as it is when the daemon stopped between recording the switch and
// marking the account live. T was switched back to two minutes ago; its stored
// pair runs from four minutes ago to now at a quarter point a minute -- a point
// moved, so it runs, and it is later than any other. The work's rate is the
// half point a minute measured on L.
func TestAPairSpanningAReturnIsNeverTheWorksRate(t *testing.T) {
	l := idleOn(maxAcct("l-left", 60, 3*time.Hour, 40, 3*day))
	l.LiveBurns = []usage.LiveBurn{{PerMin: 0.5, Window: usage.WindowFiveHour, From: now.Add(-9 * time.Minute), At: now.Add(-6 * time.Minute)}}
	tl := idleOn(maxAcct("t-live", 10, 4*time.Hour, 30, 4*day))
	tl.LiveBurns = []usage.LiveBurn{{PerMin: 0.25, Window: usage.WindowFiveHour, From: now.Add(-4 * time.Minute), At: now}}
	c := idleOn(maxAcct("c", 70, 4*time.Hour, 30, 4*day))
	st := NewState()
	st.RecordSwitch("t-live", now.Add(-2*time.Minute), Departure{})

	p := Decide([]Candidate{l, tl, c}, hoverOpts(), Config{}, st, "t-live")
	r, _ := find(p.Result, "c")
	if got := runsOutIn(t, r); math.Abs(got.Minutes()-30/0.5) > 1e-6 {
		t.Errorf("c runs out in %s, want 60m: the pair spanning the return is not the work's rate", got)
	}
}

// Only a window that is a limit for the pass sets the work's rate on its
// length. In three minutes the live account's full week went from 49% to 50%
// and its Fable cap from 0 to 30: the week's rate is a third of a point a
// minute, and the Fable cap, which hover dodges, does not make it ten. Its
// fifty points last two and a half hours. Named with --model, the Fable cap is
// the session's limit and its rate is in force: the account runs out no later
// than the seven minutes the cap's seventy points last.
func TestAModelCapsRateIsNotTheWeeksRate(t *testing.T) {
	fable := usage.ScopedWindowName(usage.ScopeModel, "Fable")
	live := polled(sub("a-live", &usage.Snapshot{
		FiveHour: win(10, 4*time.Hour), SevenDay: win(50, 3*day),
		Limits: []usage.Limit{scoped("Fable", "", 30, 3*day)},
	}), 180*time.Second)
	live.LiveBurns = []usage.LiveBurn{
		{PerMin: 1.0 / 3, Window: usage.WindowSevenDay, From: now.Add(-3 * time.Minute), At: now},
		{PerMin: 10, Window: fable, From: now.Add(-3 * time.Minute), At: now},
	}
	p := Decide([]Candidate{live}, hoverOpts(), Config{}, NewState(), "a-live")
	r, _ := find(p.Result, "a-live")
	if got := runsOutIn(t, r); r.RunsOutOn != usage.WindowSevenDay || math.Abs(got.Minutes()-150) > 1e-6 {
		t.Errorf("the live account runs out on %q in %s, want the week in 150m", r.RunsOutOn, got)
	}

	o := hoverOpts()
	o.Model = "fable"
	p = Decide([]Candidate{live}, o, Config{}, NewState(), "a-live")
	r, _ = find(p.Result, "a-live")
	if got := runsOutIn(t, r); got > 7*time.Minute+time.Millisecond {
		t.Errorf("with --model fable the live account runs out on %q in %s, want within the Fable cap's 7m", r.RunsOutOn, got)
	}
}

// R2: the work's rate applies to EVERY window of its length an account carries,
// not only to the one with the least room. B's week has four points and its
// five-hour window five; at three five-hour points a minute the five-hour window
// stops it in a hundred seconds, whatever its week says. It is neither a carrier
// nor a bridge, and the live account is ridden until it is out, then handed to B
// as a last resort.
func TestTheCarryTestReadsEveryWindowOfTheRatesLength(t *testing.T) {
	b := idleOn(maxAcct("b", 95, 3*time.Hour, 96, 2*day))
	riding := measuredLive(polled(maxAcct("a-live", 85, 3*time.Hour, 40, 4*day), 180*time.Second), 3, usage.WindowFiveHour)

	p := Decide([]Candidate{riding, b}, hoverOpts(), Config{}, NewState(), "a-live")
	r, _ := find(p.Result, "b")
	if r.Carries || r.RunsOutOn != usage.WindowFiveHour || math.Abs(runsOutIn(t, r).Seconds()-100) > 1e-6 {
		t.Fatalf("b carries %v, runs out on %q in %s; want the five-hour window in 100s, not a carrier",
			r.Carries, r.RunsOutOn, runsOutIn(t, r))
	}
	if p.Action == ActionSwitch {
		t.Errorf("switched to %s while the live account still had fifteen points", p.Target.UUID)
	}

	out := measuredLive(polled(maxAcct("a-live", 99.99, 3*time.Hour, 40, 4*day), 180*time.Second), 3, usage.WindowFiveHour)
	want(t, Decide([]Candidate{out, b}, hoverOpts(), Config{}, NewState(), "a-live"), ActionSwitch, ReasonLastPoint, "b")
}

// I4: an account left closing is not a carrier, whatever its cadence and the
// work's rate -- so the order never takes the session straight back to an account
// it left at a limit, and the mark is not the only thing standing between the two.
// The reading the live account was left on may be older than the departure, and
// the work spent the difference: the projection counts it.
func TestAnAccountLeftClosingIsNeverACarrier(t *testing.T) {
	for _, cadence := range []time.Duration{60 * time.Second, 180 * time.Second, 300 * time.Second, 1800 * time.Second} {
		for _, rate := range []float64{0.84, 5.4} {
			for _, age := range []time.Duration{0, cadence / 2} {
				name := fmt.Sprintf("%s cadence, %.2f a minute, read %s before leaving", cadence, rate, age)
				t.Run(name, func(t *testing.T) {
					// Throttled is the only way an account is read every half hour.
					throttled := cadence > pollpolicy.CandidateMaxInterval
					fetched := now.Add(-age)
					// On the closing boundary, a second inside it.
					room := rate * (cadence + hoverReserve - time.Second).Minutes()
					l := sub("l", &usage.Snapshot{FiveHour: win(100-room, 4*time.Hour), SevenDay: win(30, 4*day)})
					l.FetchedAt, l.NextPollAt = fetched, fetched.Add(cadence)
					if throttled {
						l.LastRateLimited = now.Add(-time.Minute)
					}
					l.LiveBurns = []usage.LiveBurn{{PerMin: rate, Window: usage.WindowFiveHour, From: fetched.Add(-cadence), At: fetched}}
					x := idleOn(weekOnly("x", 20, 5*day))

					st := NewState()
					st.RecordSwitch("x", now, Departure{From: "l", Closing: usage.WindowFiveHour})
					p := Decide([]Candidate{l, x}, hoverOpts(), Config{}, st, "x")
					if r, _ := find(p.Result, "l"); r.Carries {
						t.Errorf("l carries the work (runs out in %s): the session would be taken straight back", runsOutIn(t, r))
					}
				})
			}
		}
	}
}

// What the work spent on an account between its reading and its departure is
// taken off that reading's window -- and only that window. A was read thirty
// minutes ago and left ten minutes ago at 5.4 points a minute, which is 108
// points; its five-hour window reset just now, so all of them were spent on the
// window that reset, and the fresh one has its whole hundred. A carries.
func TestWorkBeforeAResetDoesNotSpendTheRoomTheResetRestored(t *testing.T) {
	left := sub("a-left", &usage.Snapshot{FiveHour: win(60, 0), SevenDay: win(40, 3*day)})
	left.FetchedAt, left.NextPollAt = now.Add(-30*time.Minute), now.Add(10*time.Minute)
	live := measuredLive(polled(maxAcct("b-live", 50, 3*time.Hour, 50, 4*day), 180*time.Second), 5.4, usage.WindowFiveHour)
	st := NewState()
	st.RecordSwitch("b-live", now.Add(-10*time.Minute), Departure{From: "a-left", Closing: usage.WindowFiveHour})

	p := Decide([]Candidate{left, live}, hoverOpts(), Config{}, st, "b-live")
	if r, _ := find(p.Result, "a-left"); !r.Carries {
		t.Errorf("a-left carries %v, runs out on %q at %s: the reset restored its whole window",
			r.Carries, r.RunsOutOn, r.RunsOutAt.Sub(now))
	}
}

// The live account's projection runs from its window's reset when the window
// reset after the reading. Read twenty minutes ago, its five-hour window reset
// five minutes ago, so the work has been spending a fresh window for five
// minutes: at two points a minute its hundred points last until forty-five
// minutes from now, not thirty.
func TestTheLiveAccountsRunwayRunsFromAResetAfterItsReading(t *testing.T) {
	live := sub("a-live", &usage.Snapshot{FiveHour: win(80, -5*time.Minute), SevenDay: win(20, 4*day)})
	live.FetchedAt, live.NextPollAt = now.Add(-20*time.Minute), now.Add(time.Minute)
	live = measuredLive(live, 2, usage.WindowFiveHour)
	p := Decide([]Candidate{live}, hoverOpts(), Config{}, NewState(), "a-live")
	r, _ := find(p.Result, "a-live")
	if r.RunsOutOn != usage.WindowFiveHour || math.Abs(runsOutIn(t, r).Minutes()-45) > 1e-6 {
		t.Errorf("the live account runs out on %q in %s, want the fresh five-hour window in 45m", r.RunsOutOn, runsOutIn(t, r))
	}
}

// I5: a carrier is not closing when the work arrives, nor at its first live
// reading -- so the move is never followed by a second move on the same rate.
// Swept over rooms on either side of the carrier threshold at two rates: every
// account the verdict calls a carrier, read at its prompt poll with the first
// live cadence ahead of it, is not closing.
func TestACarrierIsNotClosingAtItsFirstLiveReading(t *testing.T) {
	for _, rate := range []float64{0.84, 5.4} {
		carriers := 0
		for room := 1.0; room <= 100; room += 0.5 {
			cand := idleOn(maxAcct("t", 100-room, 4*time.Hour, 30, 4*day))
			src := idleOn(weekOnly("src", 30, 5*day))
			src.LiveBurns = []usage.LiveBurn{{PerMin: rate, Window: usage.WindowFiveHour, From: now.Add(-4 * time.Minute), At: now.Add(-time.Minute)}}
			r, _ := find(Decide([]Candidate{cand, src}, hoverOpts(), Config{}, NewState(), "src").Result, "t")
			if !r.Carries {
				continue
			}
			carriers++
			// The move happens now. The first live reading is at the prompt
			// poll, with the work spent up to it and one live cadence ahead.
			first := pollpolicy.PromptPollAt(cand.FetchedAt, cand.NextPollAt, cand.LastRateLimited, now, 1)
			spent := rate * first.Sub(now).Minutes()
			live := sub("t", &usage.Snapshot{FiveHour: win(100-room+spent, 4*time.Hour-first.Sub(now)), SevenDay: win(30, 4*day)})
			o := hoverOpts()
			o.Now = first
			live.FetchedAt, live.NextPollAt = first, first.Add(pollpolicy.ActiveMaxInterval)
			st := NewState()
			st.RecordSwitch("t", now, Departure{})
			lr, _ := find(Decide([]Candidate{live, src}, o, Config{}, st, "t").Result, "t")
			if lr.Closing != "" {
				t.Fatalf("at %.2f a minute a carrier with %.1f points is closing at its first live reading", rate, room)
			}
		}
		if carriers == 0 || carriers == 199 {
			t.Fatalf("at %.2f a minute %d of 199 rooms carry; the sweep does not straddle the threshold", rate, carriers)
		}
	}
}

// R3: the account the work has just moved to is judged on the work's rate, which
// is still the one measured on the account it left until its own live pair
// lands -- not on its own window average, which describes some earlier session.
// B's average says five minutes; the work says over sixteen. And the account
// left behind is held out, not a carrier, so the session stays.
func TestANewlyLiveAccountIsJudgedOnTheWorksRate(t *testing.T) {
	a := sub("a-left", &usage.Snapshot{FiveHour: win(97, 3*time.Hour), SevenDay: win(40, 4*day)})
	a.FetchedAt, a.NextPollAt = now.Add(-time.Minute), now.Add(9*time.Minute)
	a.LiveBurns = []usage.LiveBurn{{PerMin: 0.3, Window: usage.WindowFiveHour, From: now.Add(-4 * time.Minute), At: now.Add(-time.Minute)}}
	// Forty-five minutes into its five-hour window at 95%: an average of two
	// points a minute, from a session that is not this one.
	b := sub("b-live", &usage.Snapshot{FiveHour: win(95, 4*time.Hour+15*time.Minute), SevenDay: win(30, 5*day)})
	b.FetchedAt, b.NextPollAt = now.Add(-5*time.Minute), now
	b.BurnPerMin, b.HasBurn, b.BurnWindow = 0, true, usage.WindowFiveHour

	st := NewState()
	st.RecordSwitch("b-live", now, Departure{From: "a-left", Closing: usage.WindowFiveHour})
	p := Decide([]Candidate{a, b}, hoverOpts(), Config{}, st, "b-live")
	if r, _ := find(p.Result, "b-live"); r.Closing != "" {
		t.Errorf("b-live closing on %q at %s: judged on its own average rather than the work's rate", r.Closing, runsOutIn(t, r))
	}
	if r, _ := find(p.Result, "a-left"); r.Carries {
		t.Errorf("a-left carries the work; it was left at a limit")
	}
	if p.Action == ActionSwitch {
		t.Errorf("switched to %s (%s)", p.Target.UUID, p.Reason)
	}
}

// R4a: whether an account carries the work does not depend on who is live. The
// rate is per window length and carried across a switch, so a Max with eight
// five-hour points is judged on the five-hour rate last measured, whether the
// session is on another Max or on an Ultra that has no five-hour window at all.
func TestWhoIsLiveDoesNotChangeWhetherAnAccountCarries(t *testing.T) {
	m := idleOn(maxAcct("m", 60, 3*time.Hour, 40, 3*day))
	m.LiveBurns = []usage.LiveBurn{{PerMin: 0.84, Window: usage.WindowFiveHour, From: now.Add(-14 * time.Minute), At: now.Add(-10 * time.Minute)}}
	b := idleOn(maxAcct("b", 92, 3*time.Hour, 40, 4*day))
	u := idleOn(weekOnly("u", 50, 2*day))
	u.LiveBurns = []usage.LiveBurn{{PerMin: 0.02, Window: usage.WindowSevenDay, From: now.Add(-3 * time.Minute), At: now}}

	carries := func(live string) map[string]bool {
		out := map[string]bool{}
		for _, r := range Decide([]Candidate{m, b, u}, hoverOpts(), Config{}, NewState(), live).Result.Order {
			if r.UUID != live {
				out[r.UUID] = r.Carries
			}
		}
		return out
	}
	onMax, onUltra := carries("m"), carries("u")
	if onMax["b"] != onUltra["b"] || onMax["b"] {
		t.Errorf("b carries %v with the Max live and %v with the Ultra live; want false both times", onMax["b"], onUltra["b"])
	}
}

// A live account with room that stays because the account ahead of it in the
// order cannot carry the work says so. The Max sorts first -- it has a
// five-hour window -- but its week has five points, and at half a point a
// minute the work would spend them in ten minutes: "already on the best
// account" would be false of the session on the Ultra.
func TestALiveAccountKeptByAnAccountThatCannotCarryTheWorkSaysSo(t *testing.T) {
	pool := []Candidate{
		idleOn(maxAcct("a-max", 10, 3*time.Hour, 95, 3*day)),
		measuredLive(weekOnly("u-live", 30, 2*day), 0.5, usage.WindowSevenDay),
	}
	p := Decide(pool, hoverOpts(), Config{}, NewState(), "u-live")
	want(t, p, ActionStay, ReasonCannotCarry, "")
}

// The hold names the rule that kept the session, not the carry test, when the
// rule would have kept it anyway. B's week resets sooner, so it sorts ahead of
// the live A, and it could not carry the work -- but a five-hour account is
// left only at a limit, and that is why the session stays. Nothing is held for
// --force to take: B would run the work out.
func TestTheHoldNamesTheRuleThatKeptTheSession(t *testing.T) {
	pool := []Candidate{
		idleOn(maxAcct("b", 95, 3*time.Hour, 40, 2*day)),
		measuredLive(polled(maxAcct("a-live", 40, 3*time.Hour, 50, 5*day), 180*time.Second), 1, usage.WindowFiveHour),
	}
	p := Decide(pool, hoverOpts(), Config{}, NewState(), "a-live")
	want(t, p, ActionStay, ReasonStayUntilLimit, "")
	if p.Held.UUID != "" {
		t.Errorf("held %q, which the work would run out", p.Held.UUID)
	}
}

// The first reason in the order is the one the hold reports. The session is on
// an Ultra. Ahead of it, the Max the order prefers cannot carry the work -- its
// week has five points, ten minutes at half a point a minute -- and after the
// Max, another Ultra whose week ends two minutes sooner, inside the reset
// margin, could. The session stays because the Max cannot carry it; the Ultra is
// the move held, for --force.
func TestTheHoldNamesTheFirstReasonInTheOrder(t *testing.T) {
	pool := []Candidate{
		idleOn(maxAcct("a-max", 10, 3*time.Hour, 95, 3*day)),
		idleOn(weekOnly("b-ultra", 30, 2*day-2*time.Minute)),
		measuredLive(weekOnly("u-live", 30, 2*day), 0.5, usage.WindowSevenDay),
	}
	p := Decide(pool, hoverOpts(), Config{}, NewState(), "u-live")
	want(t, p, ActionStay, ReasonCannotCarry, "")
	if p.Held.UUID != "b-ultra" {
		t.Errorf("held %q, want the Ultra the margin refused", p.Held.UUID)
	}
}

// From a live last-resort credit seat the session goes back to free quota on the
// levels alone: to a carrier, and to an account that is not one as well -- forty
// points the work would spend in four minutes, five it would spend in one --
// because a point of quota already paid for is spent before a point of money,
// even when another move follows. A Max on its last point is not free quota to
// go back to; the seat stays, and nothing is held for --force.
func TestALiveCreditSeatReturnsToFreeQuota(t *testing.T) {
	seat := creditWith("c-seat", usage.ExtraUsageFor(usage.ExtraUsageInput{
		State: usage.ExtraUsageEnabled, Currency: "USD", MonthlyLimit: pf(10000), UsedCredits: pf(0), Utilization: pf(10),
	}))
	for _, tc := range []struct {
		name    string
		pct     float64
		rate    float64
		carries bool
		action  Action
		reason  Reason
		target  string
	}{
		{"a carrier", 50, 1, true, ActionSwitch, ReasonShortWindowRoom, "b"},
		{"forty points that cannot carry the work", 60, 10, false, ActionSwitch, ReasonShortWindowRoom, "b"},
		{"five points that cannot carry the work", 95, 5, false, ActionSwitch, ReasonShortWindowRoom, "b"},
		{"an account on its last point", 99.5, 1, false, ActionStay, ReasonNoSubscriptionRoom, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := measuredLive(idleOn(maxAcct("b", tc.pct, 3*time.Hour, 40, 3*day)), tc.rate, usage.WindowFiveHour)
			p := Decide([]Candidate{b, seat}, hoverOpts(), Config{}, NewState(), "c-seat")
			if r, _ := find(p.Result, "b"); r.Carries != tc.carries {
				t.Fatalf("b carries %v, want %v: the case is not the one named", r.Carries, tc.carries)
			}
			want(t, p, tc.action, tc.reason, tc.target)
			if p.Action != ActionSwitch && p.Held.UUID != "" {
				t.Errorf("held %q for --force", p.Held.UUID)
			}
		})
	}
}

// With nothing in the main pool -- the live Max quarantined, a last-resort
// credit seat the only account left -- hover asks the credit arms exactly as
// the configured strategies do, and the gate answers: refused, at the default
// spend ceiling of zero.
func TestAnEmptyMainPoolUnderHoverIsTheCreditGatesToAnswer(t *testing.T) {
	seat := creditWith("c-seat", usage.ExtraUsageFor(usage.ExtraUsageInput{
		State: usage.ExtraUsageEnabled, Currency: "USD", MonthlyLimit: pf(10000), UsedCredits: pf(0), Utilization: pf(10),
	}))
	st := NewState()
	st.Quarantine("a-live", now, time.Hour, "dead refresh token")
	p := Decide([]Candidate{maxAcct("a-live", 40, 3*time.Hour, 50, 3*day), seat}, hoverOpts(), Config{}, st, "a-live")
	want(t, p, ActionBlocked, ReasonCreditGate, "")
	if p.Credit.Reason != CreditNotOptedIn {
		t.Errorf("Credit.Reason = %v, want %v", p.Credit.Reason, CreditNotOptedIn)
	}
}

// pausedMax is a live Max at pct of its five-hour window, read three minutes
// ago, with the work measured at the production rate before it paused: at that
// rate its fifteen points at 85% ran out of its projection seconds ago. running
// is what its most recent live pair saw.
func pausedMax(uuid string, pct float64, running bool) Candidate {
	c := sub(uuid, &usage.Snapshot{FiveHour: win(pct, 3*time.Hour), SevenDay: win(50, 4*day)})
	c.FetchedAt, c.NextPollAt = now.Add(-3*time.Minute), now.Add(2*time.Minute)
	c.LiveBurns = []usage.LiveBurn{{PerMin: 5.4, Window: usage.WindowFiveHour, From: now.Add(-8 * time.Minute), At: now.Add(-5 * time.Minute)}}
	c.WorkRunning, c.LastPairAt = running, c.FetchedAt
	return c
}

// openSeat is a last-resort credit seat the user has opted into spending on.
func openSeat() Candidate {
	return creditWith("c-seat", usage.ExtraUsageFor(usage.ExtraUsageInput{
		State: usage.ExtraUsageEnabled, Currency: "USD", MonthlyLimit: pf(10000), UsedCredits: pf(0), Utilization: pf(10),
	}))
}

// noCredit fails the test when the plan reached the credit pool: money is not
// spent while a reading shows free quota.
func noCredit(t *testing.T, p Plan, why string) {
	t.Helper()
	if p.Action == ActionSwitch || p.CreditConsulted {
		t.Errorf("%s (%s) to %q, credit consulted %v: %s", p.Action, p.Reason, p.Target.UUID, p.CreditConsulted, why)
	}
}

// A projection does not open the credit pool, paused or running. The live Max
// holds fifteen points; every other subscription account is empty, the user's
// thresholds call the main pool exhausted, and the seat is opted in. Projected
// at the rate the work last ran, the Max is out -- but its reading shows fifteen
// points, and money is decided on readings (creditOpen). The walk opens when
// the Max is read empty.
func TestAPausedLiveAccountDoesNotOpenTheCreditPool(t *testing.T) {
	empty := maxAcct("b-empty", 100, 2*time.Hour, 60, 3*day)
	o := hoverOpts()
	o.Threshold = 80
	cfg := Config{MaxAutoSpend: 500}

	for _, running := range []bool{false, true} {
		p := Decide([]Candidate{pausedMax("a-live", 85, running), empty, openSeat()}, o, cfg, NewState(), "a-live")
		if r, _ := find(p.Result, "a-live"); r.limit != liveOut {
			t.Fatalf("the live Max is at limit %d, not projected out: the case is not the projection", r.limit)
		}
		noCredit(t, p, fmt.Sprintf("a projection opened the credit pool (running %v)", running))
	}
	p := Decide([]Candidate{pausedMax("a-live", 100, true), empty, openSeat()}, o, cfg, NewState(), "a-live")
	want(t, p, ActionSwitch, ReasonBetterTarget, "c-seat")
}

// The work moved to B, which holds twenty-four points, and paused there. B's
// running flag is still true, from the stint before; projected at the rate the
// work last ran, B is out. A is empty and the seat is opted in, but B's reading
// shows room, and no flag and no projection outweighs a reading.
func TestAStaleRunningFlagDoesNotOpenTheCreditPool(t *testing.T) {
	b := sub("b-live", &usage.Snapshot{FiveHour: win(76, 3*time.Hour), SevenDay: win(50, 4*day)})
	b.FetchedAt, b.NextPollAt = now.Add(-5*time.Minute), now.Add(time.Minute)
	b.LiveBurns = []usage.LiveBurn{{PerMin: 5.4, Window: usage.WindowFiveHour, From: now.Add(-10 * time.Minute), At: now.Add(-7 * time.Minute)}}
	b.WorkRunning, b.LastPairAt = true, now.Add(-7*time.Minute)
	o := hoverOpts()
	o.Threshold = 70
	p := Decide([]Candidate{b, maxAcct("a-empty", 100, 2*time.Hour, 60, 3*day), openSeat()}, o, Config{MaxAutoSpend: 500}, NewState(), "b-live")
	if r, _ := find(p.Result, "b-live"); r.limit != liveOut {
		t.Fatalf("B is at limit %d, not projected out: the case is not the projection", r.limit)
	}
	noCredit(t, p, "a stale running flag opened the credit pool while B's reading showed room")
}

// A live Ultra moved from 98% to 99% of its week in three minutes: one quantum,
// a third of a point a minute, which has it out by the next reading. Every
// other account is empty and the seat is opted in, but the Ultra's last point
// is free work, and nothing is bought until a reading shows it spent.
func TestAWeeklySeatsLastPointDoesNotOpenTheCreditPool(t *testing.T) {
	ultra := func(pct float64) Candidate {
		u := weekOnly("u-live", pct, 2*day)
		u.FetchedAt, u.NextPollAt = now.Add(-3*time.Minute), now.Add(30*time.Second)
		u.LiveBurns = []usage.LiveBurn{{PerMin: 1.0 / 3, Window: usage.WindowSevenDay, From: now.Add(-6 * time.Minute), At: now.Add(-3 * time.Minute)}}
		u.WorkRunning, u.LastPairAt = true, now.Add(-3*time.Minute)
		return u
	}
	o := hoverOpts()
	o.Threshold = 70
	cfg := Config{MaxAutoSpend: 500}
	empty := maxAcct("a-empty", 100, 2*time.Hour, 60, 3*day)
	p := Decide([]Candidate{ultra(99), empty, openSeat()}, o, cfg, NewState(), "u-live")
	if r, _ := find(p.Result, "u-live"); r.limit != liveOut {
		t.Fatalf("the Ultra is at limit %d, not projected out: the case is not the projection", r.limit)
	}
	noCredit(t, p, "one quantum of rate on a seat's last point opened the credit pool")
	p = Decide([]Candidate{ultra(100), empty, openSeat()}, o, cfg, NewState(), "u-live")
	want(t, p, ActionSwitch, ReasonBetterTarget, "c-seat")
}

// Nor does a pause open the last resort. The live Max is projected out across a
// pause, and the only other account holds its last point: no carrier, no
// bridge, and a move to the last point would buy nothing while nothing runs --
// the trade two such accounts made every cooldown.
func TestTwoAccountsDoNotTradeAPausedSession(t *testing.T) {
	last := idleOn(maxAcct("b-last", 99, 2*time.Hour, 60, 3*day))
	p := Decide([]Candidate{pausedMax("a-live", 85, false), last}, hoverOpts(), Config{}, NewState(), "a-live")
	if p.Action == ActionSwitch {
		t.Errorf("switched to %s (%s) across a pause", p.Target.UUID, p.Reason)
	}
	p = Decide([]Candidate{pausedMax("a-live", 85, true), last}, hoverOpts(), Config{}, NewState(), "a-live")
	want(t, p, ActionSwitch, ReasonProjectedExhaustion, "b-last")
}

// The work's running flag is the latest live pair's across the pass, whichever
// account made it. B is live and projected out, and its own latest pair, four
// minutes old, says one thing; A, which the work came from and left empty, has
// made one since that says the other. A's is the work's: when it saw a pause
// the last resort onto C's last point stays shut whatever B's record says, and
// when it saw the work running the last resort opens though B's own pair saw
// a pause.
func TestTheWorksRunningFlagIsTheLatestPairs(t *testing.T) {
	last := idleOn(maxAcct("c-last", 99, 2*time.Hour, 60, 3*day))
	for _, aRunning := range []bool{false, true} {
		b := pausedMax("b-live", 85, !aRunning)
		b.LastPairAt = now.Add(-4 * time.Minute)
		a := idleOn(maxAcct("a-left", 100, 2*time.Hour, 60, 3*day))
		a.WorkRunning, a.LastPairAt = aRunning, now.Add(-time.Minute)

		p := Decide([]Candidate{b, a, last}, hoverOpts(), Config{}, NewState(), "b-live")
		if moved := p.Action == ActionSwitch && p.Target.UUID == "c-last"; moved != aRunning {
			t.Errorf("A's later pair running %v: %s (%s) to %q", aRunning, p.Action, p.Reason, p.Target.UUID)
		}
	}
}

// A live credit seat goes back to any account that can carry the work, one held
// out included: money is last, and the hold says only where the session would
// rather be. The Max was left at a limit on its five-hour window, which has not
// rolled, and it has fifty points the work would take hours to spend.
func TestALiveCreditSeatReturnsToAHeldOutCarrier(t *testing.T) {
	seat := creditWith("c-seat", usage.ExtraUsageFor(usage.ExtraUsageInput{
		State: usage.ExtraUsageEnabled, Currency: "USD", MonthlyLimit: pf(10000), UsedCredits: pf(0), Utilization: pf(10),
	}))
	held := measuredLive(idleOn(maxAcct("b-held", 50, 3*time.Hour, 40, 3*day)), 0.5, usage.WindowFiveHour)
	p := Decide([]Candidate{held, seat}, hoverOpts(), Config{}, leftAt("b-held", usage.WindowFiveHour), "c-seat")
	if r, _ := find(p.Result, "b-held"); r.Rung() != RungHeldOut || !r.Carries {
		t.Fatalf("b is at rung %s, carries %v: the case is a held-out carrier", r.Rung(), r.Carries)
	}
	want(t, p, ActionSwitch, ReasonBetterTarget, "b-held")
}

// The target is chosen first and the cooldown applied second. At a limit with
// nowhere to go is blocked whether or not a cooldown is running and whichever
// account sorts first -- at the last point and past it alike -- and a carrier
// inside the cooldown is a hold that names the move it held.
func TestTheTargetIsChosenBeforeTheCooldown(t *testing.T) {
	cooling := NewState()
	cooling.RecordSwitch("a-live", now.Add(-30*time.Second), Departure{})
	for _, used := range []float64{99, 99.5} {
		for _, other := range []time.Duration{1 * day, 6 * day} {
			for _, st := range []*State{NewState(), cooling} {
				name := fmt.Sprintf("%.1f%%, the other week in %s, cooldown %v", used, other, st == cooling)
				t.Run(name, func(t *testing.T) {
					live := workingAt(maxAcct("a-live", used, 4*time.Hour, 50, 4*day), 1, usage.WindowFiveHour)
					b := idleOn(maxAcct("b", used, 3*time.Hour, 50, other))
					p := Decide([]Candidate{live, b}, hoverOpts(), Config{}, st, "a-live")
					if p.Action != ActionBlocked || p.Reason != ReasonNothingCanServe || p.Held.UUID != "" {
						t.Errorf("%s (%s), held %q; want blocked with nothing held", p.Action, p.Reason, p.Held.UUID)
					}
				})
			}
		}
	}

	live := workingAt(maxAcct("a-live", 99, 4*time.Hour, 50, 4*day), 1, usage.WindowFiveHour)
	roomy := idleOn(weekOnly("u-room", 20, 2*day))
	p := Decide([]Candidate{live, roomy}, hoverOpts(), Config{}, cooling, "a-live")
	want(t, p, ActionStay, ReasonCooldown, "")
	if p.Held.UUID != "u-room" {
		t.Errorf("Held = %q, want the carrier the cooldown held", p.Held.UUID)
	}
}

// Every hover move goes to an account that passes the tier the live account's
// state allows: a carrier always; a bridge only when the live account would stop
// before its next reading; a last resort only when it has nothing left. Checked
// over generated pools -- levels, rates, cadences and marks drawn from a fixed
// seed -- so a branch that slipped a non-carrier through shows up somewhere.
func TestEveryHoverMoveIsToACarrierABridgeOrALastResort(t *testing.T) {
	rnd := rand.New(rand.NewSource(20261109))
	cadences := []time.Duration{60 * time.Second, 180 * time.Second, 300 * time.Second, 600 * time.Second}
	moves := 0
	for i := 0; i < 4000; i++ {
		n := 2 + rnd.Intn(3)
		pool := make([]Candidate, 0, n)
		st := NewState()
		for j := 0; j < n; j++ {
			uuid := string(rune('a' + j))
			var c Candidate
			if rnd.Intn(3) == 0 {
				c = weekOnly(uuid, 60+rnd.Float64()*40, time.Duration(1+rnd.Intn(6))*day)
			} else {
				c = maxAcct(uuid, 60+rnd.Float64()*40, time.Duration(30+rnd.Intn(270))*time.Minute, 20+rnd.Float64()*60,
					time.Duration(1+rnd.Intn(6))*day)
			}
			c = polled(c, cadences[rnd.Intn(len(cadences))])
			if rnd.Intn(4) == 0 {
				st.RecordSwitch("someone-else", now.Add(-time.Hour), Departure{From: uuid, Closing: usage.WindowFiveHour})
			}
			pool = append(pool, c)
		}
		rate := []float64{0.1, 0.84, 3, 5.4}[rnd.Intn(4)]
		pool[0].LiveBurns = []usage.LiveBurn{{PerMin: rate, Window: usage.WindowFiveHour, From: now.Add(-3 * time.Minute), At: now}}
		if rnd.Intn(2) == 0 {
			pool[0].LiveBurns = []usage.LiveBurn{{PerMin: rate / 33.6, Window: usage.WindowSevenDay, From: now.Add(-3 * time.Minute), At: now}}
		}

		p := Decide(pool, hoverOpts(), Config{}, st, "a")
		if p.Action != ActionSwitch {
			continue
		}
		moves++
		live, _ := find(p.Result, "a")
		tgt := p.Target
		switch {
		case tgt.Carries:
		case live.AtLimit() == "":
			t.Fatalf("pool %d: a live account with room moved to %s, which does not carry the work", i, tgt.UUID)
		case live.limit >= liveStopping && tgt.HasRunway && tgt.roomyByLevel() && tgt.RunsOutAt.After(live.RunsOutAt.Add(HoverCooldown)):
		case live.lastResortOpen():
			if _, empty := tgt.Empty(); empty {
				t.Fatalf("pool %d: a last resort onto %s, which is empty", i, tgt.UUID)
			}
		default:
			t.Fatalf("pool %d: moved to %s, which is no carrier, no bridge and no last resort", i, tgt.UUID)
		}
	}
	if moves == 0 {
		t.Fatal("no pool produced a move; the generator is not exercising the rule")
	}
}

// The hand-off rule's horizons are the poll policy's own constants, and the
// reserve and the first live cadence together outlast the cooldown: a carrier
// cannot be one the cooldown would have to hold on.
//
// Comparing values cannot see a literal of the same value standing in for the
// constant -- 180 * time.Second passes for pollpolicy.MinInterval until the
// poll policy changes and the hand-off silently does not -- so the declarations
// are read from the source as well.
func TestTheHorizonTermsAreThePollPolicysOwn(t *testing.T) {
	if hoverReserve != pollpolicy.MinInterval {
		t.Errorf("hoverReserve = %s, want pollpolicy.MinInterval", hoverReserve)
	}
	if hoverBlind != pollpolicy.ActiveMaxInterval {
		t.Errorf("hoverBlind = %s, want pollpolicy.ActiveMaxInterval", hoverBlind)
	}
	declared := constDeclarations(t, "handoff.go")
	for name, want := range map[string]string{
		"hoverReserve": "pollpolicy.MinInterval",
		"hoverBlind":   "pollpolicy.ActiveMaxInterval",
	} {
		if got := declared[name]; got != want {
			t.Errorf("%s is declared as %q, want %s itself", name, got, want)
		}
	}
	if hoverBlind+hoverReserve <= HoverCooldown {
		t.Errorf("hoverBlind + hoverReserve = %s, not beyond the cooldown %s", hoverBlind+hoverReserve, HoverCooldown)
	}
	if EvaluationInterval != time.Second {
		t.Errorf("EvaluationInterval = %s, want the daemon's one-second tick", EvaluationInterval)
	}
}

// constDeclarations is every constant a file of this package declares with a
// value, by name, as the source writes that value.
func constDeclarations(t *testing.T, file string) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, d := range f.Decls {
		gen, ok := d.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if i < len(vs.Values) {
					out[name.Name] = types.ExprString(vs.Values[i])
				}
			}
		}
	}
	return out
}

// R5: the configured strategies keep the pool maximum as the carry rate, as they
// always have. The live week at 95% spends a point a minute; C has nothing left
// and still carries the three a minute it was burning when it was live. At the
// pool maximum B's fourteen points do not survive its twelve-minute horizon, so
// the walk refuses it, and the ordinary margin holds the nine-point lead short of
// its ten: the engine stays. At the live account's own rate B would carry, and
// the walk would take it.
func TestNonHoverPreemptionKeepsThePoolMaximum(t *testing.T) {
	week := func(uuid string, pct float64, perMin float64, cadence time.Duration) Candidate {
		c := polled(sub(uuid, &usage.Snapshot{SevenDay: win(pct, 3*day)}), cadence)
		c.BurnPerMin, c.HasBurn, c.BurnWindow = perMin, true, usage.WindowSevenDay
		return c
	}
	pool := []Candidate{
		week("a-live", 95, 1, 3*time.Minute),
		week("b", 86, 0, 10*time.Minute),
		week("c-empty", 100, 3, 10*time.Minute),
	}
	p := Decide(pool, preemptOpts(), Config{}, NewState(), "a-live")
	if p.Action == ActionSwitch {
		t.Errorf("switched to %s (%s): the pool maximum refuses b, and the margin holds", p.Target.UUID, p.Reason)
	}
}

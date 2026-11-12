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

	"github.com/Kweiza/ccdaddy/internal/identity"
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

// A live account that stays because the account ahead of it in the order cannot
// carry the work says so. Only a live account the order would leave reaches the
// carry test -- one with room is left only at a limit -- so the live account
// here is one nobody could read, and the work's rate is the one last measured
// on the Max. The Max sorts first, but its week has five points, and at half a
// point a minute the work would spend them in ten minutes: "already on the best
// account" would be false of the session.
func TestALiveAccountKeptByAnAccountThatCannotCarryTheWorkSaysSo(t *testing.T) {
	pool := []Candidate{
		measuredLive(idleOn(maxAcct("a-max", 10, 3*time.Hour, 95, 3*day)), 0.5, usage.WindowSevenDay),
		{UUID: "u-live", Kind: identity.KindSubscription},
	}
	p := Decide(pool, hoverOpts(), Config{}, NewState(), "u-live")
	want(t, p, ActionStay, ReasonCannotCarry, "")
}

// The hold names the rule that kept the session, not the carry test, when the
// rule would have kept it anyway. B's week resets sooner, so it sorts ahead of
// the live A, and it could not carry the work -- but an account with room is
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

// The first carrier in the order is the move held. The session is on an Ultra
// with room. Ahead of it, the Max the order prefers cannot carry the work -- its
// week has five points, ten minutes at half a point a minute -- and after the
// Max, another Ultra whose week ends two minutes sooner could. The session
// stays because it has room; the Ultra, not the Max, is the move held for
// --force.
func TestTheHoldNamesTheFirstCarrierInTheOrder(t *testing.T) {
	pool := []Candidate{
		idleOn(maxAcct("a-max", 10, 3*time.Hour, 95, 3*day)),
		idleOn(weekOnly("b-ultra", 30, 2*day-2*time.Minute)),
		measuredLive(weekOnly("u-live", 30, 2*day), 0.5, usage.WindowSevenDay),
	}
	p := Decide(pool, hoverOpts(), Config{}, NewState(), "u-live")
	want(t, p, ActionStay, ReasonStayUntilLimit, "")
	if p.Held.UUID != "b-ultra" {
		t.Errorf("held %q, want the Ultra that carries", p.Held.UUID)
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

// The hold that waits for the live account's empty reading says so, and names
// that reading as when it lifts -- B's next poll, a minute out -- rather than a
// rollover hours away: it is the reading, not the clock, that opens the pool.
func TestAnEmptyReadingsWaitNamesTheNextReading(t *testing.T) {
	b := sub("b-live", &usage.Snapshot{FiveHour: win(76, 3*time.Hour), SevenDay: win(50, 4*day)})
	b.FetchedAt, b.NextPollAt = now.Add(-5*time.Minute), now.Add(time.Minute)
	b.LiveBurns = []usage.LiveBurn{{PerMin: 5.4, Window: usage.WindowFiveHour, From: now.Add(-10 * time.Minute), At: now.Add(-7 * time.Minute)}}
	o := hoverOpts()
	o.Threshold = 70
	p := Decide([]Candidate{b, maxAcct("a-empty", 100, 2*time.Hour, 60, 3*day), openSeat()}, o, Config{MaxAutoSpend: 500}, NewState(), "b-live")
	want(t, p, ActionStay, ReasonWaitsForAReading, "")
	if !p.HasRetryAt || !p.RetryAt.Equal(b.NextPollAt) {
		t.Errorf("RetryAt %s (set %v), want B's next reading at %s", p.RetryAt.Sub(now), p.HasRetryAt, b.NextPollAt.Sub(now))
	}
}

// A projection never moves the session onto a primary seat's balance. S, a Max
// at 85%, would stop before its next reading at the rate the work runs, and
// at 70% has room; the primary seat P could carry
// the work, and the move to it would be paid for. S stays until its own reading
// says its last point -- the hold says so, and names S's next reading as when
// it lifts -- and then goes. From P the session goes back to S on S's level --
// S has room -- and the way back up is shut while S reads room, so the two
// cannot trade the login every cooldown across a pause.
func TestAProjectionDoesNotMoveOntoAPrimarySeat(t *testing.T) {
	s := func(pct float64) Candidate {
		return measuredLive(polled(maxAcct("s-max", pct, 3*time.Hour, 40, 3*day), 180*time.Second), 5.4, usage.WindowFiveHour)
	}
	seat := primarySeat("p-seat", 41)
	p := Decide([]Candidate{s(85), seat}, hoverOpts(), Config{}, NewState(), "s-max")
	if r, _ := find(p.Result, "s-max"); r.Closing == "" {
		t.Fatalf("S is not closing: the case is not a projection")
	}
	want(t, p, ActionStay, ReasonWaitsForAReading, "")
	if next := s(85).NextPollAt; !p.HasRetryAt || !p.RetryAt.Equal(next) {
		t.Errorf("RetryAt %s (set %v), want S's next reading at %s", p.RetryAt.Sub(now), p.HasRetryAt, next.Sub(now))
	}
	p = Decide([]Candidate{s(99.5), seat}, hoverOpts(), Config{}, NewState(), "s-max")
	want(t, p, ActionSwitch, ReasonLastPoint, "p-seat")

	p = Decide([]Candidate{s(70), seat}, hoverOpts(), Config{}, leftAt("s-max", usage.WindowFiveHour), "p-seat")
	want(t, p, ActionSwitch, ReasonBetterTarget, "s-max")
}

// The same wait when the dearer seat cannot carry the work. The live Max reads
// 96%, room by its level, and the work running has it out; the primary seat P
// is on its last point, at 96% of its balance, so it carries nothing and
// bridges nothing. A move onto P's balance opens once a reading shows the Max
// at its limit (reachable), and onto P's last point once one shows it empty
// (lastResort): the hold is a wait for the Max's next reading, not "nothing
// can serve" until a rollover. Read empty, the session goes to P.
func TestADearerSeatThatCannotCarryWaitsForTheLiveReading(t *testing.T) {
	seat := primarySeat("p-seat", 96)
	live := pausedMax("a-live", 96, true)
	p := Decide([]Candidate{live, seat}, hoverOpts(), Config{}, NewState(), "a-live")
	l, _ := find(p.Result, "a-live")
	s, _ := find(p.Result, "p-seat")
	if l.limit != liveOut || !l.roomyByLevel() || s.Carries || s.LastPoint == "" {
		t.Fatalf("live at limit %d, roomy by level %v; seat carries %v, on last point %q: the case is a roomy live account projected out and a seat that cannot carry",
			l.limit, l.roomyByLevel(), s.Carries, s.LastPoint)
	}
	want(t, p, ActionStay, ReasonWaitsForAReading, "")
	if next := nextReading(live, now); !p.HasRetryAt || !p.RetryAt.Equal(next) {
		t.Errorf("RetryAt %s (set %v), want the live account's next reading at %s", p.RetryAt.Sub(now), p.HasRetryAt, next.Sub(now))
	}

	p = Decide([]Candidate{pausedMax("a-live", 100, true), seat}, hoverOpts(), Config{}, NewState(), "a-live")
	want(t, p, ActionSwitch, ReasonBetterTarget, "p-seat")
}

// A wait says a move follows the reading, and is judged on the state that
// reading makes. The live Max is roomy by its level and projected out two
// minutes ago. The primary seat P is roomy by its levels too, but it was left
// an hour ago, ten minutes after its reading, and the work spent its ten
// five-hour points and more in that time: its runway ends now. Against the
// Max's stale projection P would bridge it by the cooldown; once the Max reads
// at its limit its stop is now, no bridge outlasts it, and P is no last resort
// either -- the work empties it before the next look. No move follows, so the
// hold is "nothing can serve", blocked, and not a wait.
func TestAWaitIsNotClaimedOnABridgeTheReadingEnds(t *testing.T) {
	live := pausedMax("a-live", 96, true)
	seat := credit("p-seat", &usage.Snapshot{
		FiveHour: win(90, 3*time.Hour),
		ExtraUsage: usage.ExtraUsageFor(usage.ExtraUsageInput{
			State: usage.ExtraUsageEnabled, Currency: "USD", MonthlyLimit: pf(10000), UsedCredits: pf(5000), Utilization: pf(50),
		}),
	})
	seat.Primary = true
	seat.FetchedAt, seat.NextPollAt = now.Add(-70*time.Minute), now.Add(10*time.Minute)

	p := Decide([]Candidate{live, seat}, hoverOpts(), Config{}, leftAt("p-seat", usage.WindowFiveHour), "a-live")
	l, _ := find(p.Result, "a-live")
	s, _ := find(p.Result, "p-seat")
	if l.limit != liveOut || !l.roomyByLevel() || !s.roomyByLevel() || s.Carries || !s.HasRunway || s.RunsOutAt.After(now) {
		t.Fatalf("live at limit %d, roomy %v; seat roomy %v, carries %v, runway %v ending %s: the case is a roomy seat whose runway ends now",
			l.limit, l.roomyByLevel(), s.roomyByLevel(), s.Carries, s.HasRunway, s.RunsOutAt.Sub(now))
	}
	if _, ok := bridge(p.Result, l, HoverCooldown); !ok {
		t.Fatalf("the seat does not bridge the live account's projection; the case is a bridge the reading ends")
	}
	want(t, p, ActionBlocked, ReasonNothingCanServe, "")
}

// A reading wait retries at the live account's next reading, or at a rollover
// that comes before it. The live Max's poll is held off by a 429 for half an
// hour; the other account, on its last point, has its five-hour window back in
// ten minutes, and that is when the hold can next change.
func TestAReadingWaitRetriesAtARolloverThatComesFirst(t *testing.T) {
	live := pausedMax("a-live", 99, true)
	live.NextPollAt, live.LastRateLimited = now.Add(30*time.Minute), now.Add(-time.Minute)
	other := idleOn(maxAcct("b-last", 99, 10*time.Minute, 60, 3*day))
	p := Decide([]Candidate{live, other}, hoverOpts(), Config{}, NewState(), "a-live")
	want(t, p, ActionStay, ReasonWaitsForAReading, "")
	if !p.HasRetryAt || !p.RetryAt.Equal(now.Add(10*time.Minute)) {
		t.Errorf("RetryAt %s (set %v), want the rollover ten minutes out, before the reading thirty minutes out",
			p.RetryAt.Sub(now), p.HasRetryAt)
	}
}

// A last point reached on a reading may move onto a primary seat, but free
// quota comes first: U has never been read, a cold start the order puts behind
// the seat P, and it carries the work by its level, so S on its last point goes
// to U and not onto P's balance. A live login no pass knows goes the same way.
func TestALastPointGoesToFreeQuotaFirst(t *testing.T) {
	s := measuredLive(polled(maxAcct("s-max", 99.5, 3*time.Hour, 40, 3*day), 180*time.Second), 5.4, usage.WindowFiveHour)
	u := sub("u-unread", nil)
	seat := primarySeat("p-seat", 41)
	p := Decide([]Candidate{s, u, seat}, hoverOpts(), Config{}, NewState(), "s-max")
	if r, _ := find(p.Result, "p-seat"); !r.Carries {
		t.Fatalf("the primary seat does not carry the work: the case is not the rung order")
	}
	want(t, p, ActionSwitch, ReasonLastPoint, "u-unread")
	p = Decide([]Candidate{s, u, seat}, hoverOpts(), Config{}, NewState(), "someone-else")
	want(t, p, ActionSwitch, ReasonBetterTarget, "u-unread")
}

// Nor does an unreadable account trade the login with a primary seat. U has
// never been read: from the seat the session goes back to it, a cold start
// being a carrier by its level; from U the order would put the seat ahead -- a
// known weekly-only seat before an unread one -- but U reads no limit, so the
// move up to the seat's balance is not made.
func TestAnUnreadableAccountDoesNotTradeWithAPrimarySeat(t *testing.T) {
	u := sub("u-unread", nil)
	seat := primarySeat("p-seat", 41)
	p := Decide([]Candidate{u, seat}, hoverOpts(), Config{}, NewState(), "p-seat")
	want(t, p, ActionSwitch, ReasonBetterTarget, "u-unread")
	p = Decide([]Candidate{u, seat}, hoverOpts(), Config{}, NewState(), "u-unread")
	if p.Action == ActionSwitch {
		t.Errorf("switched from the unread account to %s (%s)", p.Target.UUID, p.Reason)
	}
}

// From a live pool seat the way back is walked cheapest first: S, a Max with
// forty points the work would spend before its second reading, is free quota,
// and it is taken before the primary seat P that could carry the work.
func TestAPoolSeatReturnsToFreeQuotaBeforeAPrimarySeat(t *testing.T) {
	s := measuredLive(idleOn(maxAcct("s-max", 60, 3*time.Hour, 40, 3*day)), 10, usage.WindowFiveHour)
	seat := primarySeat("p-seat", 41)
	p := Decide([]Candidate{s, seat, openSeat()}, hoverOpts(), Config{}, NewState(), "c-seat")
	if r, _ := find(p.Result, "p-seat"); !r.Carries {
		t.Fatalf("the primary seat does not carry the work: the case is not the ladder's order")
	}
	want(t, p, ActionSwitch, ReasonShortWindowRoom, "s-max")
}

// The credit walk asks the gate of the seat the session is on as of any other,
// with no main pool to go back to. c1 has the most armed room: the session on
// it stays, and from c2 it goes to c1 -- and does not trade the two every
// cooldown. A live seat at its own limit, its plan window spent, walks on to
// the next. A lone seat the gate refuses is blocked by the gate, and says so.
func TestTheCreditWalkSkipsTheLiveSeat(t *testing.T) {
	armed := usage.ExtraUsageFor(usage.ExtraUsageInput{
		State: usage.ExtraUsageEnabled, Currency: "USD", MonthlyLimit: pf(100000), UsedCredits: pf(0), Utilization: pf(0),
	})
	c1, c2 := creditWith("c1", armed), creditWith("c2", usage.ExtraUsageFor(usage.ExtraUsageInput{
		State: usage.ExtraUsageEnabled, Currency: "USD", MonthlyLimit: pf(1000), UsedCredits: pf(0), Utilization: pf(0),
	}))
	spent := credit("c1", &usage.Snapshot{FiveHour: win(100, time.Hour), ExtraUsage: armed})
	cfg := Config{MaxAutoSpend: 500}
	for _, hover := range []bool{false, true} {
		o := opts()
		o.Hover = hover
		want(t, Decide([]Candidate{c1, c2}, o, cfg, NewState(), "c1"), ActionStay, ReasonAlreadyBest, "")
		want(t, Decide([]Candidate{c1, c2}, o, cfg, NewState(), "c2"), ActionSwitch, ReasonBetterTarget, "c1")
		p := Decide([]Candidate{spent, c2}, o, cfg, NewState(), "c1")
		if r, _ := find(p.Result, "c1"); r.AtLimit() == "" {
			t.Fatalf("hover %v: the spent seat is not at a limit: the case is not the one named", hover)
		}
		want(t, p, ActionSwitch, ReasonBetterTarget, "c2")
		p = Decide([]Candidate{c1}, o, Config{}, NewState(), "c1")
		want(t, p, ActionBlocked, ReasonCreditGate, "")
		if p.Credit.Reason != CreditNotOptedIn {
			t.Errorf("hover %v: Credit.Reason = %v, want the gate's own refusal", hover, p.Credit.Reason)
		}
	}
}

// A seat at its own limit is nowhere to go, live or not. From c2 the walk
// passes c1, whose plan window is spent though its balance is armed, and stays;
// it does not trade the session onto c1 and back every cooldown. A lone seat
// with nothing left blocks until its window comes back, with that as its
// RetryAt; a lone seat on its last point still serves, and the session stays.
func TestTheCreditWalkSkipsASpentSeat(t *testing.T) {
	armed := usage.ExtraUsageFor(usage.ExtraUsageInput{
		State: usage.ExtraUsageEnabled, Currency: "USD", MonthlyLimit: pf(100000), UsedCredits: pf(0), Utilization: pf(0),
	})
	c2 := creditWith("c2", usage.ExtraUsageFor(usage.ExtraUsageInput{
		State: usage.ExtraUsageEnabled, Currency: "USD", MonthlyLimit: pf(1000), UsedCredits: pf(0), Utilization: pf(0),
	}))
	seatAt := func(pct float64) Candidate {
		return credit("c1", &usage.Snapshot{FiveHour: win(pct, time.Hour), ExtraUsage: armed})
	}
	cfg := Config{MaxAutoSpend: 500}
	for _, hover := range []bool{false, true} {
		o := opts()
		o.Hover = hover
		want(t, Decide([]Candidate{seatAt(100), c2}, o, cfg, NewState(), "c2"), ActionStay, ReasonAlreadyBest, "")
		p := Decide([]Candidate{seatAt(100)}, o, cfg, NewState(), "c1")
		want(t, p, ActionBlocked, ReasonNothingCanServe, "")
		if r, _ := find(p.Result, "c1"); !p.HasRetryAt || !p.RetryAt.Equal(r.RecoversAt) {
			t.Errorf("hover %v: RetryAt %s (set %v), want the seat's own recovery at %s", hover, p.RetryAt.Sub(now), p.HasRetryAt, r.RecoversAt.Sub(now))
		}
	}
	o := hoverOpts()
	p := Decide([]Candidate{seatAt(99.5)}, o, cfg, NewState(), "c1")
	if r, _ := find(p.Result, "c1"); r.LastPoint == "" {
		t.Fatalf("the seat is not on its last point: the case is not the one named")
	}
	want(t, p, ActionStay, ReasonAlreadyBest, "")
}

// A seat the session left closing is not taken back while it is held out. With
// no main pool, c1 -- the seat with the most armed room -- is live with its
// opted-in week at 98%, spent at half a point a minute and next read in three
// minutes: closing, so the walk passes it and the session moves to c2, leaving
// c1 marked. From c2, after the cooldown, c1 reads roomy again -- a projection
// is the live account's alone -- but it is held out, and the session stays,
// whether the work on c2 is running or paused.
func TestACreditSeatLeftClosingIsNotTakenBack(t *testing.T) {
	armed := func(limit float64) usage.ExtraUsage {
		return usage.ExtraUsageFor(usage.ExtraUsageInput{
			State: usage.ExtraUsageEnabled, Currency: "USD", MonthlyLimit: pf(limit), UsedCredits: pf(0), Utilization: pf(0),
		})
	}
	c1 := credit("c1", &usage.Snapshot{SevenDay: win(98, 3*day), ExtraUsage: armed(100000)})
	c1.FetchedAt, c1.NextPollAt = now, now.Add(3*time.Minute)
	c1.LiveBurns = []usage.LiveBurn{{PerMin: 0.5, Window: usage.WindowSevenDay, From: now.Add(-3 * time.Minute), At: now}}
	c1.WorkRunning, c1.LastPairAt = true, now
	cfg := Config{MaxAutoSpend: 500}

	p := Decide([]Candidate{c1, creditWith("c2", armed(1000))}, hoverOpts(), cfg, NewState(), "c1")
	if r, _ := find(p.Result, "c1"); r.Closing == "" {
		t.Fatalf("c1 is not closing: the case is not the one named")
	}
	want(t, p, ActionSwitch, ReasonBetterTarget, "c2")

	for _, running := range []bool{true, false} {
		c2 := creditWith("c2", armed(1000))
		c2.FetchedAt, c2.NextPollAt = now, now.Add(3*time.Minute)
		c2.WorkRunning, c2.LastPairAt = running, now
		st := NewState()
		st.RecordSwitch("c2", now.Add(-time.Hour), p.Departure)
		back := Decide([]Candidate{c1, c2}, hoverOpts(), cfg, st, "c2")
		if r, _ := find(back.Result, "c1"); r.HeldOut == "" {
			t.Fatalf("c1 is not held out: the departure was not recorded")
		}
		if back.Action == ActionSwitch {
			t.Errorf("running %v: switched back to %s (%s) while c1 is held out", running, back.Target.UUID, back.Reason)
		}
		want(t, back, ActionStay, ReasonAlreadyBest, "")
	}
}

// A held-out seat is a last resort, and a live seat with nothing left takes
// it: c2 is spent, c1 was left at a limit on a week that has not rolled but has
// room now, and the session goes there rather than reporting nothing can serve.
func TestAnEmptyLiveSeatTakesAHeldOutSeat(t *testing.T) {
	armed := usage.ExtraUsageFor(usage.ExtraUsageInput{
		State: usage.ExtraUsageEnabled, Currency: "USD", MonthlyLimit: pf(1000), UsedCredits: pf(0), Utilization: pf(0),
	})
	c1 := credit("c1", &usage.Snapshot{SevenDay: win(60, 3*day), ExtraUsage: armed})
	c2 := credit("c2", &usage.Snapshot{FiveHour: win(100, time.Hour), ExtraUsage: armed})
	p := Decide([]Candidate{c1, c2}, hoverOpts(), Config{MaxAutoSpend: 500}, leftAt("c1", usage.WindowSevenDay), "c2")
	if r, _ := find(p.Result, "c1"); r.HeldOut == "" {
		t.Fatalf("c1 is not held out: the case is not the one named")
	}
	want(t, p, ActionSwitch, ReasonBetterTarget, "c1")
}

// spentPoolSeat is a pool seat metered in credits alone, its $10 monthly limit
// spent as far as used reaches.
func spentPoolSeat(uuid string, used float64) Candidate {
	return creditWith(uuid, usage.ExtraUsageFor(usage.ExtraUsageInput{
		State: usage.ExtraUsageEnabled, Currency: "USD", MonthlyLimit: pf(1000), UsedCredits: pf(used * 10), Utilization: pf(used),
	}))
}

// emptyIn is whether uuid reads empty in res.
func emptyIn(res Result, uuid string) bool {
	r, _ := find(res, uuid)
	_, empty := r.Empty()
	return empty
}

// blockedPoolSeat is a pool seat metered in credits alone that the account
// refuses to spend: out of credits at 77.85% of its monthly limit, the live
// reading in usage's liveMaxUsageBlocked fixture.
func blockedPoolSeat(uuid string) Candidate {
	return creditWith(uuid, usage.ExtraUsageFor(usage.ExtraUsageInput{
		State: usage.ExtraUsageBlocked, DisabledReason: "out_of_credits", Currency: "USD",
		MonthlyLimit: pf(1000), UsedCredits: pf(778.5), Utilization: pf(77.85),
	}))
}

// Under hover a pool seat is empty when its credit balance is spent, as a
// primary seat is when either of its meters is: one predicate, which every
// reader of emptiness -- the rung, the walk, the last resort -- then agrees on.
// With balance left, it is not. The configured strategies read a credit seat's
// emptiness as they always have.
func TestAPoolSeatIsEmptyWhenItsBalanceIsSpent(t *testing.T) {
	pool := []Candidate{spentPoolSeat("c-spent", 100), spentPoolSeat("c-left", 50)}
	res := Rank(pool, hoverOpts())
	if !emptyIn(res, "c-spent") || emptyIn(res, "c-left") {
		t.Errorf("under hover: spent seat empty %v, seat with half left empty %v; want true, false",
			emptyIn(res, "c-spent"), emptyIn(res, "c-left"))
	}
	if res := Rank(pool, opts()); emptyIn(res, "c-spent") {
		t.Error("outside hover, the spent balance made a pool seat empty")
	}
}

// A seat the account refuses credit is empty under hover whatever its
// utilization reads: out of credits at 77.85%. A seat with balance left is not
// empty with max_auto_spend at its default of 0 -- that is ccdad's spend
// policy, not the account's ability to serve.
func TestABlockedSeatIsEmptyAtAnyUtilization(t *testing.T) {
	p := Decide([]Candidate{blockedPoolSeat("c-out"), spentPoolSeat("c-left", 50)}, hoverOpts(), Config{}, NewState(), "c-left")
	if !emptyIn(p.Result, "c-out") {
		t.Error("a seat out of credits at 77.85% is not empty")
	}
	if emptyIn(p.Result, "c-left") {
		t.Error("a seat with half its limit left is empty with max_auto_spend at 0")
	}
}

// A credit meter is spent when a refusal state says so -- a seat whose overage
// is off, or that is blocked, at whatever utilization -- or when the meter,
// where there is one, reads at or past its limit, whatever the state: an
// enabled seat, and one whose state could not be read, at their limit and not
// below it. An unknown state with no meter reading is not spent -- unreadable
// is not empty. Whether spending can be priced is the gate's own question, and
// its refusals are the ones they always were.
func TestACreditMeterIsSpentByStateAndLevel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state usage.ExtraUsageState
		pct   *float64
		spent bool
		gate  CreditReason
	}{
		{"unknown with no reading", usage.ExtraUsageUnknown, nil, false, CreditSpendUnreadable},
		{"unknown at its limit", usage.ExtraUsageUnknown, pf(100), true, CreditSpendUnreadable},
		{"enabled below its limit", usage.ExtraUsageEnabled, pf(50), false, CreditAllowed},
		{"enabled at its limit", usage.ExtraUsageEnabled, pf(100), true, CreditNoRoom},
		{"disabled", usage.ExtraUsageDisabled, pf(10), true, CreditAccountDisabled},
		{"blocked", usage.ExtraUsageBlocked, pf(10), true, CreditAccountBlocked},
	} {
		in := usage.ExtraUsageInput{State: tc.state, Currency: "USD", MonthlyLimit: pf(1000), Utilization: tc.pct}
		if tc.pct != nil {
			in.UsedCredits = pf(*tc.pct * 10)
		}
		e := usage.ExtraUsageFor(in)
		if got := creditSpent(creditWith("c", e), hoverOpts()); got != tc.spent {
			t.Errorf("%s: spent %v, want %v", tc.name, got, tc.spent)
		}
		if got := CreditGate(e, 500, true).Reason; got != tc.gate {
			t.Errorf("%s: the gate says %q, want %q", tc.name, got, tc.gate)
		}
	}
}

// A live seat whose state could not be read but whose meter reads its limit
// has nothing left, and takes a held-out seat with room as its last resort.
// No main pool, the cooldown long over: c1 was left closing on its week, which
// has not rolled, and has room and armed balance; c2, live, reports no
// is_enabled and a utilization of 100.
func TestAnUnknownLiveSeatAtItsLimitTakesAHeldOutSeat(t *testing.T) {
	armed := usage.ExtraUsageFor(usage.ExtraUsageInput{
		State: usage.ExtraUsageEnabled, Currency: "USD", MonthlyLimit: pf(100000), UsedCredits: pf(0), Utilization: pf(0),
	})
	c1 := credit("c1", &usage.Snapshot{SevenDay: win(60, 3*day), ExtraUsage: armed})
	c2 := creditWith("c2", usage.ExtraUsageFor(usage.ExtraUsageInput{State: usage.ExtraUsageUnknown, Currency: "USD", Utilization: pf(100)}))
	p := Decide([]Candidate{c1, c2}, hoverOpts(), Config{MaxAutoSpend: 500}, leftAt("c1", usage.WindowSevenDay), "c2")
	if r, _ := find(p.Result, "c1"); r.HeldOut == "" {
		t.Fatalf("c1 is not held out: the case is not the one named")
	}
	want(t, p, ActionSwitch, ReasonBetterTarget, "c1")
}

// A live seat whose overage is off takes a held-out seat with room as its last
// resort, whatever its utilization reads. No main pool, the cooldown long over:
// c1 was left closing on its week, which has not rolled, and has room and armed
// balance; c2, live, is a pool seat whose extra usage is disabled.
func TestADisabledLiveSeatTakesAHeldOutSeat(t *testing.T) {
	armed := usage.ExtraUsageFor(usage.ExtraUsageInput{
		State: usage.ExtraUsageEnabled, Currency: "USD", MonthlyLimit: pf(100000), UsedCredits: pf(0), Utilization: pf(0),
	})
	c1 := credit("c1", &usage.Snapshot{SevenDay: win(60, 3*day), ExtraUsage: armed})
	c2 := creditWith("c2", usage.ExtraUsageFor(usage.ExtraUsageInput{State: usage.ExtraUsageDisabled, Currency: "USD"}))
	p := Decide([]Candidate{c1, c2}, hoverOpts(), Config{MaxAutoSpend: 500}, leftAt("c1", usage.WindowSevenDay), "c2")
	if r, _ := find(p.Result, "c1"); r.HeldOut == "" {
		t.Fatalf("c1 is not held out: the case is not the one named")
	}
	want(t, p, ActionSwitch, ReasonBetterTarget, "c1")
}

// A live seat out of credits takes a held-out seat with room as its last
// resort, whatever its own utilization reads. No main pool, the cooldown long
// over: c1 was left closing on its week, which has not rolled, and has room and
// armed balance; c2, live, is refused credit at 77.85% of its limit.
func TestAnOutOfCreditsLiveSeatTakesAHeldOutSeat(t *testing.T) {
	armed := usage.ExtraUsageFor(usage.ExtraUsageInput{
		State: usage.ExtraUsageEnabled, Currency: "USD", MonthlyLimit: pf(100000), UsedCredits: pf(0), Utilization: pf(0),
	})
	c1 := credit("c1", &usage.Snapshot{SevenDay: win(60, 3*day), ExtraUsage: armed})
	p := Decide([]Candidate{c1, blockedPoolSeat("c2")}, hoverOpts(), Config{MaxAutoSpend: 500}, leftAt("c1", usage.WindowSevenDay), "c2")
	if r, _ := find(p.Result, "c1"); r.HeldOut == "" {
		t.Fatalf("c1 is not held out: the case is not the one named")
	}
	want(t, p, ActionSwitch, ReasonBetterTarget, "c1")
}

// The configured strategies' margins do not read a pool seat's credit balance:
// hover and pre-emption off, the cooldown over, a threshold of 80. The live
// pool seat's opted-in model cap is at 20% -- sixty points of slack -- though
// its credit is spent; the subscription account is at 40%, forty points. The
// candidate is not ahead, and the session stays for the hysteresis margin, as
// it always has: the empty-live waiver is not reached through the balance.
func TestTheConfiguredMarginsDoNotReadAPoolSeatsBalance(t *testing.T) {
	fable := usage.ScopedWindowName(usage.ScopeModel, "Fable")
	seat := credit("c-live", &usage.Snapshot{
		Limits: []usage.Limit{scoped("Fable", "", 20, 3*day)},
		ExtraUsage: usage.ExtraUsageFor(usage.ExtraUsageInput{
			State: usage.ExtraUsageEnabled, Currency: "USD", MonthlyLimit: pf(1000), UsedCredits: pf(1000), Utilization: pf(100),
		}),
	})
	o := opts()
	o.Threshold = 80
	o.WindowThreshold = map[usage.WindowName]float64{fable: 80}
	p := Decide([]Candidate{seat, maxAcct("s-sub", 40, 3*time.Hour, 40, 3*day)}, o, Config{HysteresisPct: 25}, NewState(), "c-live")
	want(t, p, ActionStay, ReasonHysteresis, "")
}

// A live pool seat whose credit balance is spent has nothing left, and takes a
// held-out seat with room as its last resort. No main pool, the cooldown long
// over: c1 was left closing on its week, which has not rolled, but has room
// and armed balance; c2, live, is metered in credits alone and its $10 limit is
// spent. The session goes to c1.
func TestASpentLivePoolSeatTakesAHeldOutSeat(t *testing.T) {
	armed := usage.ExtraUsageFor(usage.ExtraUsageInput{
		State: usage.ExtraUsageEnabled, Currency: "USD", MonthlyLimit: pf(100000), UsedCredits: pf(0), Utilization: pf(0),
	})
	c1 := credit("c1", &usage.Snapshot{SevenDay: win(60, 3*day), ExtraUsage: armed})
	p := Decide([]Candidate{c1, spentPoolSeat("c2", 100)}, hoverOpts(), Config{MaxAutoSpend: 500}, leftAt("c1", usage.WindowSevenDay), "c2")
	if r, _ := find(p.Result, "c1"); r.HeldOut == "" {
		t.Fatalf("c1 is not held out: the case is not the one named")
	}
	want(t, p, ActionSwitch, ReasonBetterTarget, "c1")
}

// A walk that finds every pool seat's balance spent reports the gate's own
// refusal of the first one -- the allowed spend is used up -- rather than that
// no account has quota: the seats are passed as at their limit, and the
// sentence is still the one a user can act on. Under hover and outside it,
// with a spent subscription account live, and with no main pool at all.
func TestASpentBalanceIsReportedByTheGate(t *testing.T) {
	empty := maxAcct("a-empty", 100, 2*time.Hour, 60, 3*day)
	cfg := Config{MaxAutoSpend: 500}
	for _, hover := range []bool{false, true} {
		for _, tc := range []struct {
			name string
			pool []Candidate
			live string
		}{
			{"a spent subscription account live", []Candidate{empty, spentPoolSeat("c1", 100), spentPoolSeat("c2", 100)}, "a-empty"},
			{"no main pool", []Candidate{spentPoolSeat("c1", 100), spentPoolSeat("c2", 100)}, "c1"},
			{"a lone seat", []Candidate{spentPoolSeat("c1", 100)}, "c1"},
		} {
			o := opts()
			o.Hover = hover
			p := Decide(tc.pool, o, cfg, NewState(), tc.live)
			if p.Action != ActionBlocked || p.Reason != ReasonCreditGate || p.Credit.Reason != CreditNoRoom {
				t.Errorf("hover %v, %s: %s (%s), credit %q; want blocked by the gate: %s",
					hover, tc.name, p.Action, p.Reason, p.Credit.Reason, CreditNoRoom)
			}
		}
	}
}

// A wait for the credit pool is a wait for a move a seat can take. S is on its
// last point, A is empty and a pool seat is registered, but with max_auto_spend
// at its default of 0 the gate arms nothing: the session is blocked as any dry
// fleet is, not told that a paid move is coming. Opted in, it waits for S's
// reading.
func TestACreditWaitNeedsASeatTheGateArms(t *testing.T) {
	s := maxAcct("s-live", 99.5, 3*time.Hour, 40, 3*day)
	a := maxAcct("a-empty", 100, 2*time.Hour, 60, 3*day)
	o := hoverOpts()
	o.Threshold = 70
	p := Decide([]Candidate{s, a, openSeat()}, o, Config{}, NewState(), "s-live")
	if p.Reason == ReasonWaitsForAReading {
		t.Errorf("%s (%s): no seat is armed, so no move onto money is coming", p.Action, p.Reason)
	}
	want(t, p, ActionBlocked, ReasonNothingCanServe, "")
	p = Decide([]Candidate{s, a, openSeat()}, o, Config{MaxAutoSpend: 500}, NewState(), "s-live")
	want(t, p, ActionStay, ReasonWaitsForAReading, "")
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
// pause, and the only other account holds three points, half a minute of the
// work: no carrier, no bridge, and a move to it would buy nothing while nothing
// runs -- the trade two such accounts made every cooldown.
func TestTwoAccountsDoNotTradeAPausedSession(t *testing.T) {
	short := idleOn(maxAcct("b-short", 97, 2*time.Hour, 60, 3*day))
	p := Decide([]Candidate{pausedMax("a-live", 85, false), short}, hoverOpts(), Config{}, NewState(), "a-live")
	if p.Action == ActionSwitch {
		t.Errorf("switched to %s (%s) across a pause", p.Target.UUID, p.Reason)
	}
	p = Decide([]Candidate{pausedMax("a-live", 85, true), short}, hoverOpts(), Config{}, NewState(), "a-live")
	want(t, p, ActionSwitch, ReasonProjectedExhaustion, "b-short")
}

// The work's running flag is the latest live pair's across the pass, whichever
// account made it. B is live and projected out, and its own latest pair, four
// minutes old, says one thing; A, which the work came from and left empty, has
// made one since that says the other. A's is the work's: when it saw a pause
// the last resort onto C's three points stays shut whatever B's record says,
// and when it saw the work running the last resort opens though B's own pair
// saw a pause.
func TestTheWorksRunningFlagIsTheLatestPairs(t *testing.T) {
	short := idleOn(maxAcct("c-short", 97, 2*time.Hour, 60, 3*day))
	for _, aRunning := range []bool{false, true} {
		b := pausedMax("b-live", 85, !aRunning)
		b.LastPairAt = now.Add(-4 * time.Minute)
		a := idleOn(maxAcct("a-left", 100, 2*time.Hour, 60, 3*day))
		a.WorkRunning, a.LastPairAt = aRunning, now.Add(-time.Minute)

		p := Decide([]Candidate{b, a, short}, hoverOpts(), Config{}, NewState(), "b-live")
		if moved := p.Action == ActionSwitch && p.Target.UUID == "c-short"; moved != aRunning {
			t.Errorf("A's later pair running %v: %s (%s) to %q", aRunning, p.Action, p.Reason, p.Target.UUID)
		}
	}
}

// The reserve is the stopping window's length class's (closingReserve): a
// five-hour window is run to its next reading, and a week is handed off a
// reserve before it. Each live account below is read a minute before its next
// reading and the work would stop it half a reserve after that reading, with an
// Ultra beside it that carries the work. On a five-hour window that is room,
// and the session stays; on a week it is closing, and the session moves.
func TestAFiveHourWindowRunsToItsNextReadingAndAWeekKeepsItsReserve(t *testing.T) {
	const cadence = time.Minute
	stopsIn := cadence + hoverReserve/2
	carrier := idleOn(weekOnly("u-carrier", 20, 5*day))

	const fiveRate = 2.0
	five := measuredLive(polled(maxAcct("a-live", 100-fiveRate*stopsIn.Minutes(), 3*time.Hour, 50, 4*day), cadence),
		fiveRate, usage.WindowFiveHour)
	p := Decide([]Candidate{five, carrier}, hoverOpts(), Config{}, NewState(), "a-live")
	if live, _ := find(p.Result, "a-live"); live.limit != liveRoom || live.Closing != "" || runsOutIn(t, live) != stopsIn {
		t.Errorf("five-hour live account at limit %d, closing %q, out in %s: want room, out half a reserve after its next reading",
			live.limit, live.Closing, live.RunsOutAt.Sub(now))
	}
	if p.Action == ActionSwitch {
		t.Errorf("moved to %s (%s) a reserve before the five-hour window's stop", p.Target.UUID, p.Reason)
	}

	const weekRate = 1.0
	week := measuredLive(polled(sub("a-live", &usage.Snapshot{SevenDay: win(100-weekRate*stopsIn.Minutes(), 3*day)}), cadence),
		weekRate, usage.WindowSevenDay)
	p = Decide([]Candidate{week, carrier}, hoverOpts(), Config{}, NewState(), "a-live")
	if live, _ := find(p.Result, "a-live"); live.limit != liveClosing || live.Closing != usage.WindowSevenDay {
		t.Errorf("weekly live account at limit %d, closing %q: want closing on its week", live.limit, live.Closing)
	}
	want(t, p, ActionSwitch, ReasonProjectedExhaustion, "u-carrier")

	// Closing is not stopping: an account that would only bridge the week --
	// eight points, past the live one's stop by more than a cooldown and short
	// of a carrier's -- is not taken a reserve early.
	bridge := idleOn(weekOnly("u-bridge", 92, 5*day))
	p = Decide([]Candidate{week, bridge}, hoverOpts(), Config{}, NewState(), "a-live")
	if b, _ := find(p.Result, "u-bridge"); b.Carries {
		t.Fatalf("u-bridge carries the work; the case is a bridge")
	}
	want(t, p, ActionBlocked, ReasonNothingCanServe, "")
}

// The reserve is each window's own, and the live account is closing when any
// window's stop falls inside its own reserve after the next reading -- not
// only the window that stops first. The live Max is read now and next in nine
// and a half minutes. Its five-hour window, ten points at a point a minute,
// stops at ten minutes: after that reading, and with no reserve it is room.
// Its week, two points at 0.18 a minute, stops a minute later, inside the
// three minutes after the reading a week keeps. The account is closing on its
// week, and is left on it for the Ultra that carries the work.
func TestEveryWindowKeepsItsOwnReserve(t *testing.T) {
	live := polled(maxAcct("a-live", 90, 3*time.Hour, 98, 3*day), 9*time.Minute+30*time.Second)
	live.LiveBurns = []usage.LiveBurn{
		{PerMin: 1, Window: usage.WindowFiveHour, From: now.Add(-3 * time.Minute), At: now},
		{PerMin: 0.18, Window: usage.WindowSevenDay, From: now.Add(-3 * time.Minute), At: now},
	}
	live.WorkRunning, live.LastPairAt = true, now
	carrier := idleOn(weekOnly("u-carrier", 20, 5*day))

	p := Decide([]Candidate{live, carrier}, hoverOpts(), Config{}, NewState(), "a-live")
	r, _ := find(p.Result, "a-live")
	if r.RunsOutOn != usage.WindowFiveHour || runsOutIn(t, r) != 10*time.Minute {
		t.Fatalf("the live account runs out on %q in %s; the case is the five-hour window stopping first, at ten minutes",
			r.RunsOutOn, r.RunsOutAt.Sub(now))
	}
	if r.limit != liveClosing || r.Closing != usage.WindowSevenDay {
		t.Errorf("live account at limit %d, closing %q: want closing on its week", r.limit, r.Closing)
	}
	want(t, p, ActionSwitch, ReasonProjectedExhaustion, "u-carrier")
	if p.Departure != (Departure{From: "a-live", Closing: usage.WindowSevenDay}) {
		t.Errorf("Departure = %+v, want a-live left on its week", p.Departure)
	}
}

// When every account is down to its last point they are spent one at a time,
// each confirmed spent before the session moves on: another account's last
// point is taken only once a reading shows the live account empty. The live
// Max reads 99 with the work running at 5.4 points a minute, three minutes
// after its reading, so the projection has it out; a reading cannot tell that
// from a point still there, and the move waits -- for the live account's next
// reading, which is what the hold names, and not for a rollover hours out.
// Read empty, it goes. Every tier ahead of the last resort is shut throughout:
// the other account holds one point, which carries nothing and bridges
// nothing.
func TestEveryLastPointIsConfirmedSpentBeforeTheNext(t *testing.T) {
	for _, other := range []Candidate{
		idleOn(maxAcct("b-last", 99, 2*time.Hour, 60, 3*day)),
		weekOnly("b-last", 99, 2*day),
	} {
		projected := pausedMax("a-live", 99, true)
		p := Decide([]Candidate{projected, other}, hoverOpts(), Config{}, NewState(), "a-live")
		live, _ := find(p.Result, "a-live")
		b, _ := find(p.Result, "b-last")
		if live.limit != liveOut || !live.lastResortOpen() || b.LastPoint == "" {
			t.Fatalf("live at limit %d, last resort open %v, b on last point %q: the case is two last points with the live one projected out",
				live.limit, live.lastResortOpen(), b.LastPoint)
		}
		want(t, p, ActionStay, ReasonWaitsForAReading, "")
		if next := nextReading(projected, now); !p.HasRetryAt || !p.RetryAt.Equal(next) || next.Sub(now) != 2*time.Minute {
			t.Errorf("RetryAt %s (set %v), want the live account's next reading two minutes out", p.RetryAt.Sub(now), p.HasRetryAt)
		}

		p = Decide([]Candidate{pausedMax("a-live", 100, true), other}, hoverOpts(), Config{}, NewState(), "a-live")
		want(t, p, ActionSwitch, ReasonBetterTarget, "b-last")
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
// before its next reading; a last resort only when it has nothing left, and
// onto another account's last point only when it reads empty. Checked
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
			if _, liveEmpty := live.Empty(); tgt.LastPoint != "" && !liveEmpty {
				t.Fatalf("pool %d: a last resort onto %s's last point from a live account that does not read empty", i, tgt.UUID)
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

// A five-hour account whose poll drew a 429 in the last hour is still where a
// hand-off goes ahead of an Ultra. The throttle preference picks among accounts
// of one rung; across rungs it would put the session on the Ultra, and since a
// seat with room is left only at a limit, keep it there for the rest of its
// week. Another five-hour account that is not throttled is still preferred.
func TestAThrottledFiveHourCarrierStillOutranksAnUltra(t *testing.T) {
	throttled := maxAcct("b-max", 10, 3*time.Hour, 40, 3*day)
	throttled.LastRateLimited = now.Add(-10 * time.Minute)
	pool := []Candidate{
		maxAcct("a-live", 99, 3*time.Hour, 50, 4*day),
		throttled,
		weekOnly("u-ultra", 20, 5*day),
	}
	want(t, Decide(pool, hoverOpts(), Config{}, NewState(), "a-live"), ActionSwitch, ReasonLastPoint, "b-max")

	clear := maxAcct("c-max", 10, 3*time.Hour, 40, 4*day)
	pool = append(pool, clear)
	want(t, Decide(pool, hoverOpts(), Config{}, NewState(), "a-live"), ActionSwitch, ReasonLastPoint, "c-max")
}

// Among Ultra seats the throttle preference does not apply. The seat whose week
// ends first is entered even though its poll drew a 429 ten minutes ago: passed
// over for a later week, it would wait until that seat's limit, and its own
// week could expire unspent meanwhile.
func TestAThrottledUltraWhoseWeekEndsFirstIsStillEnteredFirst(t *testing.T) {
	sooner := weekOnly("u1-sooner", 30, 1*day)
	sooner.LastRateLimited = now.Add(-10 * time.Minute)
	pool := []Candidate{
		maxAcct("a-live", 99, 3*time.Hour, 50, 4*day),
		sooner,
		weekOnly("u2-later", 30, 5*day),
	}
	want(t, Decide(pool, hoverOpts(), Config{}, NewState(), "a-live"), ActionSwitch, ReasonLastPoint, "u1-sooner")
}

package strategy

import (
	"fmt"
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

	// A pair whose first reading precedes the switch back onto its account
	// spans the absence between, and is not the work's.
	rates := workRatesOf([]Candidate{tl}, now.Add(-2*time.Minute), "t-live")
	if _, ok := rates[5*time.Hour]; ok {
		t.Errorf("a pair spanning a return onto its account was taken as the work's rate")
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
			first := pollpolicy.PromptPollAt(cand.FetchedAt, cand.NextPollAt, cand.LastRateLimited, now)
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
		case live.out(now):
			if empty, known := OutOfQuota(tgt.Headroom); known && empty {
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
func TestTheHorizonTermsAreThePollPolicysOwn(t *testing.T) {
	if hoverReserve != pollpolicy.CandidateMaxInterval {
		t.Errorf("hoverReserve = %s, want pollpolicy.CandidateMaxInterval", hoverReserve)
	}
	if hoverBlind != pollpolicy.ActiveMaxInterval {
		t.Errorf("hoverBlind = %s, want pollpolicy.ActiveMaxInterval", hoverBlind)
	}
	if hoverBlind+hoverReserve <= HoverCooldown {
		t.Errorf("hoverBlind + hoverReserve = %s, not beyond the cooldown %s", hoverBlind+hoverReserve, HoverCooldown)
	}
	if EvaluationInterval != time.Second {
		t.Errorf("EvaluationInterval = %s, want the daemon's one-second tick", EvaluationInterval)
	}
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

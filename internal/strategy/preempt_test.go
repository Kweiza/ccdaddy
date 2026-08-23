package strategy

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// burning is an account three hours into a five-hour window with pct of it
// spent, so its burn extrapolates to one definite instant. At 88% that instant
// is 1472 seconds away: 12 points left at 88 points per three hours.
func burning(uuid string, pct float64) Candidate {
	return sub(uuid, &usage.Snapshot{FiveHour: win(pct, 2*time.Hour)})
}

// polled stamps a candidate with the provenance the pre-emptive rule reads: when
// the reading was taken, and when the scheduler means to take the next one. The
// gap between them is the interval the engine is blind for.
func polled(c Candidate, interval time.Duration) Candidate {
	c.FetchedAt = now
	c.NextPollAt = now.Add(interval)
	return c
}

// preemptPool is the pair every case below runs on: a live account burning
// toward its limit and one candidate a single point under its threshold. One
// point is deliberate — it clears NEITHER ordinary margin, so nothing except the
// projection can move this engine.
func preemptPool(interval time.Duration) []Candidate {
	return []Candidate{
		polled(burning("a-live", 88), interval),
		polled(burning("b-room", 79), interval),
	}
}

// preemptOpts is the ranking pass with pre-emption armed. opts() leaves
// PreemptLead at zero, which is OFF, so every other test in this package takes
// the ordinary path untouched.
func preemptOpts() Options {
	o := opts()
	o.PreemptLead = 2 * time.Minute
	return o
}

// The self-correcting property, which is what made a pre-emptive switch
// acceptable at all: the horizon is the engine's OWN blind interval, so the rule
// needs no tuning number to tell it how close is too close. At the 60 s urgent
// cadence it switches late and near the limit, wasting almost no quota; at the
// 1800 s ceiling a 429 imposes it switches early, because polling is blocked and
// the session is not.
//
// The two cases run on identical accounts. The only difference is the interval.
func TestPreemptionScalesWithTheBlindInterval(t *testing.T) {
	// 1472 s from the limit. The tight horizon is 60 + 120 = 180 s and does not
	// reach it; the stretched one is 1800 + 120 = 1920 s and does.
	tight := Decide(preemptPool(60*time.Second), preemptOpts(), Config{}, NewState(), "a-live")
	want(t, tight, ActionStay, ReasonHysteresis, "")

	stretched := Decide(preemptPool(1800*time.Second), preemptOpts(), Config{}, NewState(), "a-live")
	want(t, stretched, ActionSwitch, ReasonProjectedExhaustion, "b-room")
}

// A zero lead switches pre-emption off, and that is a real answer rather than an
// omission: config carries the 2 minute default, so a zero reaching the engine
// means somebody set preempt_lead to nothing on purpose. The same accounts that
// switch above must then stay.
func TestPreemptionIsOffWithoutALead(t *testing.T) {
	p := Decide(preemptPool(1800*time.Second), opts(), Config{}, NewState(), "a-live")
	want(t, p, ActionStay, ReasonHysteresis, "")
}

// The cooldown is not a freshness problem. It is the only thing bounding a
// switch storm, and a projection that could talk its way past it would turn one
// fast-burning account into a swap every tick.
func TestPreemptionDoesNotBypassTheCooldown(t *testing.T) {
	st := NewState()
	st.RecordSwitch("a-live", now.Add(-time.Minute))

	p := Decide(preemptPool(1800*time.Second), preemptOpts(), Config{}, st, "a-live")
	want(t, p, ActionStay, ReasonCooldown, "")
	if !p.HasRetryAt {
		t.Error("HasRetryAt = false; a cooldown knows when it lifts and should say so")
	}
}

// A pre-emptive move never reaches the credit pool, so there is no credit gate
// for it to bypass. Here the only account with room is a credit one under the
// default ceiling of 0, and the engine ends exactly where it would have without
// the projection: blocked at the gate, not switched onto money.
func TestPreemptionNeverReachesTheCreditPool(t *testing.T) {
	cands := []Candidate{
		polled(burning("a-live", 88), 1800*time.Second),
		creditWith("c-money", enabledExtra(f(10000), f(0))),
	}

	p := Decide(cands, preemptOpts(), Config{}, NewState(), "a-live")
	want(t, p, ActionBlocked, ReasonCreditGate, "")
}

// The projection is not a reason on its own. With every candidate past its own
// threshold there is nowhere to go, and moving would trade one limit for another
// while spending the cooldown on it.
//
// It is asserted twice: against the literal answer, and against the SAME pool
// decided with the lead removed. The second one is what keeps the case honest —
// it says pre-emption changed nothing here, rather than that some other rule
// happens to land on the same verdict.
func TestPreemptionNeedsACandidateWithSlack(t *testing.T) {
	cands := []Candidate{
		polled(burning("a-live", 88), 1800*time.Second),
		polled(burning("b-spent", 85), 1800*time.Second),
	}

	armed := Decide(cands, preemptOpts(), Config{}, NewState(), "a-live")
	want(t, armed, ActionStay, ReasonHysteresis, "")

	off := Decide(cands, opts(), Config{}, NewState(), "a-live")
	if armed.Action != off.Action || armed.Reason != off.Reason || armed.Target.UUID != off.Target.UUID {
		t.Fatalf("armed = %v/%v -> %q, off = %v/%v -> %q; with nowhere to go the projection must not move the engine",
			armed.Action, armed.Reason, armed.Target.UUID, off.Action, off.Reason, off.Target.UUID)
	}
}

// The projection runs on the BINDING window, so a second window with room does
// not talk the engine out of a move. The window with the least left is the one
// that ends a session.
func TestPreemptionProjectsTheBindingWindow(t *testing.T) {
	live := polled(sub("a-live", &usage.Snapshot{
		FiveHour: win(88, 2*time.Hour),
		SevenDay: win(20, 4*24*time.Hour),
	}), 1800*time.Second)
	room := polled(sub("b-room", &usage.Snapshot{
		FiveHour: win(79, 2*time.Hour),
		SevenDay: win(20, 4*24*time.Hour),
	}), 1800*time.Second)

	p := Decide([]Candidate{live, room}, preemptOpts(), Config{}, NewState(), "a-live")
	want(t, p, ActionSwitch, ReasonProjectedExhaustion, "b-room")
}

// A zero NextPollAt is the scheduler saying "poll now", which is how the
// daemon's own due() reads it — so the blind interval is nothing and the lead is
// the whole horizon. It is NOT the missing-provenance case below, and reading it
// as one would switch pre-emption off for exactly the account the scheduler has
// queued for an immediate reading.
//
// 99% three hours into a five-hour window is 109 seconds from the limit, which
// is the only burn a 2 minute lead alone reaches: at anything slower the blind
// interval is what gets there first. The ordinary path would move here too, so
// the Reason is what this pins — a stay, or a move called anything else, is the
// zero being read as unknown.
func TestPreemptionReadsAZeroNextPollAsPollNow(t *testing.T) {
	live := burning("a-live", 99)
	live.FetchedAt = now

	p := Decide([]Candidate{live, polled(burning("b-room", 79), time.Minute)}, preemptOpts(), Config{}, NewState(), "a-live")
	want(t, p, ActionSwitch, ReasonProjectedExhaustion, "b-room")
}

// A next poll dated BEFORE the reading is a clock that moved, not a negative
// interval. Subtracting it unclamped gives a horizon in the past, and every
// projection is then "later than that" — so the one condition that would have
// switched pre-emption on switches it off instead, silently, on the account
// closest to being cut off.
func TestPreemptionClampsAPollDatedBeforeItsReading(t *testing.T) {
	live := burning("a-live", 99)
	live.FetchedAt = now
	live.NextPollAt = now.Add(-time.Hour)

	p := Decide([]Candidate{live, polled(burning("b-room", 79), time.Minute)}, preemptOpts(), Config{}, NewState(), "a-live")
	want(t, p, ActionSwitch, ReasonProjectedExhaustion, "b-room")
}

// A reading with no FetchedAt has no blind interval to project across, and the
// answer is the ordinary margins rather than a horizon conjured out of the zero
// time.
func TestPreemptionNeedsTheReadingsProvenance(t *testing.T) {
	cands := []Candidate{burning("a-live", 88), burning("b-room", 79)}
	cands[0].NextPollAt = now.Add(1800 * time.Second)

	p := Decide(cands, preemptOpts(), Config{}, NewState(), "a-live")
	want(t, p, ActionStay, ReasonHysteresis, "")
}

// The case above cannot fail if the guard is deleted, which is why this one
// exists beside it: Sub against the zero time saturates at the largest Duration
// there is, adding the lead wraps that negative, and a horizon in the year 1734
// produces the same "no switch" the guard produces. The two answers are worth
// the same to a caller and nothing like the same to a reader, so the guard is
// pinned where it is decided rather than where its effect is invisible.
func TestPreemptHorizonRefusesAReadingWithNoProvenance(t *testing.T) {
	c := burning("a-live", 88)
	c.NextPollAt = now.Add(1800 * time.Second)

	if h, ok := preemptHorizon(c, 2*time.Minute); ok {
		t.Errorf("preemptHorizon = %v, ok = true; a reading with no FetchedAt has no blind interval to measure", h)
	}
}

// The horizon is INCLUSIVE: an exhaustion landing exactly on it is inside it,
// not one second past. 88% three hours into a five-hour window exhausts at
// 1472 s, so a 1352 s blind interval plus the 120 s lead puts the two instants
// on the same second — the only place this boundary is visible at all.
func TestPreemptionFiresOnAnExhaustionExactlyAtTheHorizon(t *testing.T) {
	p := Decide(preemptPool(1352*time.Second), preemptOpts(), Config{}, NewState(), "a-live")
	want(t, p, ActionSwitch, ReasonProjectedExhaustion, "b-room")
}

// A window inside its first seventh has numbers too noisy to extrapolate from,
// and usage.PaceOf says so by refusing a projection. That refusal has to read as
// "no", never as "yes": an account 88% spent half an hour into a five-hour
// window is the exact shape a straight line gets wrong, and a missing projection
// read as a firing one would pre-empt on every account that has none.
func TestPreemptionSaysNoWhenThereIsNoProjectionToRead(t *testing.T) {
	// The reset is 4.5 hours out, so the window started half an hour ago — under
	// the 2571 s seventh at which pace starts answering.
	live := polled(sub("a-live", &usage.Snapshot{FiveHour: win(88, 4*time.Hour+30*time.Minute)}), 1800*time.Second)
	room := polled(burning("b-room", 79), 1800*time.Second)

	p := Decide([]Candidate{live, room}, preemptOpts(), Config{}, NewState(), "a-live")
	want(t, p, ActionStay, ReasonHysteresis, "")
}

// The projection reads the BINDING window and no other, which is the rule as
// approved: the window with the least slack is the one the ranking, the report
// and this decision all speak about, and letting a second window fire here would
// pre-empt on a cap that does not bind for the model about to run.
//
// It has a seam, recorded here because it is reachable rather than theoretical.
// Binding is the window with the least SLACK; the window that ends a session is
// the one that reaches 100% SOONEST, and the two part company whenever burn
// rates differ — a five-hour window always burns faster than a weekly one. Here
// a-live's weekly window binds at 1 point of slack and is 38 hours from its
// limit, while its five-hour window has 2 points of slack and 846 seconds. The
// engine stays, which is the approved rule rather than an oversight in it.
func TestPreemptionProjectsOnlyTheBindingWindow(t *testing.T) {
	live := polled(sub("a-live", &usage.Snapshot{
		// 3000 s into a five-hour window, past the 2571 s at which pace answers.
		FiveHour: win(78, 15000*time.Second),
		SevenDay: win(79, 24*time.Hour),
	}), 1800*time.Second)
	room := polled(burning("b-room", 75), 1800*time.Second)

	p := Decide([]Candidate{live, room}, preemptOpts(), Config{}, NewState(), "a-live")
	want(t, p, ActionStay, ReasonHysteresis, "")
}

// A projection is about the live account and the move it asks for is AWAY from
// it. Handing back the account it was measured on would spend the cooldown to
// stay exactly where it is, and that is reachable rather than theoretical: an
// account can top the pool on slack and still be burning toward its own
// threshold.
func TestPreemptionDoesNotTargetTheAccountItIsAbout(t *testing.T) {
	// a-live is one point UNDER its threshold, so it tops the ranking, and 3600 s
	// of blind interval reaches the 2870 s its burn needs. b-lower is the only
	// other account and has no slack to offer.
	cands := []Candidate{
		polled(burning("a-live", 79), 3600*time.Second),
		polled(burning("b-lower", 85), 3600*time.Second),
	}

	p := Decide(cands, preemptOpts(), Config{}, NewState(), "a-live")
	want(t, p, ActionStay, ReasonAlreadyBest, "")
}

// preemptTarget walks the subscription order and never the credit pool, and that
// is what keeps a projection from spending money: the credit gate is the only
// door onto a metered account, and a freshness rule does not get to open it.
//
// The seat here is given plan windows so it measures as the best account in the
// pass — Rank calls measure on a credit candidate like any other — because a
// guard that is only ever exercised against an unmeasurable account is not
// exercised at all.
func TestPreemptionWillNotLandOnACreditSeat(t *testing.T) {
	money := credit("c-money", &usage.Snapshot{
		FiveHour:   win(10, 2*time.Hour),
		ExtraUsage: enabledExtra(f(10000), f(0)),
	})
	cands := []Candidate{
		polled(burning("a-live", 88), 1800*time.Second),
		polled(money, 1800*time.Second),
	}

	p := Decide(cands, preemptOpts(), Config{}, NewState(), "a-live")
	if p.Reason == ReasonProjectedExhaustion {
		t.Fatalf("Action = %v, Target = %q; the projection reached the credit pool", p.Action, p.Target.UUID)
	}
	want(t, p, ActionBlocked, ReasonCreditGate, "")
}

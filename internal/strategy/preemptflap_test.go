package strategy

import (
	"testing"
	"time"
)

// The flap this file is about, measured on a live fleet: 65 switches between
// the same two accounts in two hours, one every two minutes -- the poll
// cadence -- until the user turned the engine off by hand to make it stop.
//
// The mechanism was pre-emption skipping the live account in Result.Order
// instead of stopping at it. With the live account already first, the walk took
// SECOND place, which the order itself says is worse; the ordinary
// better-target rule moved back on the next tick; and neither is damped,
// because pre-emption is deliberately answered before the cooldown and before
// every margin.
//
// bestIsLive is that pool: the live account tops the ranking and is burning
// fast enough to be projected out before the next reading, and the only other
// account is worse on the ordering axis.
// bestIsLive is that pool, built on preempt_test.go's own shapes so it cannot
// diverge from them: the live account is burning toward its five-hour limit AND
// tops the ranking, because the only other account is further past its
// threshold than the live one is.
func bestIsLive(interval time.Duration) []Candidate {
	return []Candidate{
		// Live: burning toward its five-hour limit, and still the roomiest
		// thing here on the axis the ranking orders by.
		polled(burning("a-live", 88), interval),
		// Behind it on slack, and NOT itself about to run out -- which is what
		// makes this pool able to tell the fix from the flap. A second account
		// that were also projected out would be filtered by the rule's own
		// "not itself running out" clause, and the walk would decline for a
		// reason that has nothing to do with where it stops. No provenance, so
		// the projection answers "cannot say" and the clause waves it through.
		sub("b-worse", snap(win(5, 4*time.Hour), win(95, 48*time.Hour))),
	}
}

func byUUIDOf(cands []Candidate) map[string]Candidate {
	out := map[string]Candidate{}
	for _, c := range cands {
		out[c.UUID] = c
	}
	return out
}

// The control: the live account really is projected out, so the rule really is
// reached. Without this the test below passes on a build where pre-emption
// never fires at all, which is a different bug wearing the same green.
func TestTheControlPoolReachesThePreemptiveRule(t *testing.T) {
	cands := bestIsLive(1800 * time.Second)
	o := preemptOpts()
	res := Rank(cands, o)
	if len(res.Order) == 0 || res.Order[0].UUID != "a-live" {
		t.Fatalf("Order = %v, want the live account first", order(res))
	}
	// The second account must NOT be filtered by the rule's own clauses, or
	// this pool cannot tell "stopped at the live account" from "declined for
	// another reason".
	var worseRanked Ranked
	for _, r := range res.Order {
		if r.UUID == "b-worse" {
			worseRanked = r
		}
	}
	if empty, known := OutOfQuota(worseRanked.Headroom); known && empty {
		t.Fatal("b-worse reads empty; the emptiness clause would decline it whatever this fix does")
	}
	if _, ok := preemptHorizon(byUUIDOf(cands)["b-worse"], o.PreemptLead); ok {
		t.Fatal("b-worse has a horizon; without provenance the projection must answer \"cannot say\"")
	}

	// And the live account really is projected out inside its own blind
	// interval, so the rule is reached rather than declining earlier for a
	// reason that has nothing to do with the fix.
	live := byUUIDOf(cands)["a-live"]
	horizon, ok := preemptHorizon(live, o.PreemptLead)
	if !ok {
		t.Fatal("the live account has no horizon; the fixture lost its provenance")
	}
	if !projectedExhaustion(live.Usage, o.Model, o.Now, horizon, o.Thresholds()) {
		t.Fatalf("the live account is NOT projected to run out inside %v; this test would pass on a build where pre-emption never fires", horizon)
	}
}

// The fix. Nothing outranks the live account, so there is nowhere better to go
// and staying is the answer. The projection is still true; the premise that
// somewhere else is preferable is not.
func TestPreemptionDoesNotMoveToAnAccountTheOrderCallsWorse(t *testing.T) {
	cands := bestIsLive(1800 * time.Second)
	o := preemptOpts()
	res := Rank(cands, o)

	if target, ok := preempt(byUUIDOf(cands), res, "a-live", o); ok {
		t.Errorf("preempt moved to %q, which Result.Order files behind the live account", target.UUID)
	}
}

// End to end, through Decide, which is what the daemon calls: the whole plan
// stays put rather than naming a worse target.
func TestTheEngineStaysPutWhenNothingOutranksTheLiveAccount(t *testing.T) {
	plan := Decide(bestIsLive(1800*time.Second), preemptOpts(), Config{}, NewState(), "a-live")

	if plan.Action == ActionSwitch {
		t.Errorf("Action = ActionSwitch to %q (%v); the live account already tops the ranking",
			plan.Target.UUID, plan.Reason)
	}
}

// And it still pre-empts when there IS somewhere better, or the fix has simply
// turned the rule off.
func TestPreemptionStillMovesWhenSomethingOutranksTheLiveAccount(t *testing.T) {
	cands := append(bestIsLive(1800*time.Second), polled(burning("c-better", 40), 1800*time.Second))

	plan := Decide(cands, preemptOpts(), Config{}, NewState(), "a-live")
	if plan.Action != ActionSwitch {
		t.Fatalf("Action = %v (%v), want ActionSwitch", plan.Action, plan.Reason)
	}
	// Which rule carried it is not the point and is not asserted: with a much
	// better account available the ordinary margins clear first. What matters
	// is that the engine moves, and moves to the better account.
	if plan.Target.UUID != "c-better" {
		t.Errorf("Target = %q, want c-better", plan.Target.UUID)
	}
}

package strategy

import (
	"testing"
	"time"
)

// manualCands is a pool the engine would MOVE off without the mode: the live
// account is spent and a roomy candidate is sitting behind it.
func manualCands() []Candidate {
	return []Candidate{
		sub("live", snap(win(95, time.Hour), win(90, 48*time.Hour))),
		sub("roomy", snap(win(5, time.Hour), win(10, 48*time.Hour))),
	}
}

// The control. Without the mode this pool switches, and every assertion below is
// only worth anything because this one holds.
func TestTheControlPoolSwitchesWithoutManualMode(t *testing.T) {
	plan := Decide(manualCands(), opts(), Config{}, NewState(), "live")
	if plan.Action != ActionSwitch {
		t.Fatalf("Action = %v, want ActionSwitch — the fixture must move without the mode", plan.Action)
	}
	if plan.Target.UUID != "roomy" {
		t.Fatalf("Target = %q, want roomy", plan.Target.UUID)
	}
}

func TestManualModeStaysPutOnAPoolThatWouldOtherwiseSwitch(t *testing.T) {
	plan := Decide(manualCands(), opts(), Config{Manual: true}, NewState(), "live")

	if plan.Action != ActionStay {
		t.Errorf("Action = %v, want ActionStay", plan.Action)
	}
	if plan.Reason != ReasonManual {
		t.Errorf("Reason = %v, want ReasonManual", plan.Reason)
	}
	if plan.Target.UUID != "" {
		t.Errorf("Target = %q, want empty — nothing is being moved to", plan.Target.UUID)
	}
}

// ActionStay and not ActionBlocked, because the exit contract turns on the
// difference: blocked is worth alerting on, and this is the world already being
// how the caller asked for it.
func TestManualModeIsNotBlocked(t *testing.T) {
	plan := Decide(manualCands(), opts(), Config{Manual: true}, NewState(), "live")
	if plan.Action == ActionBlocked {
		t.Error("Action = ActionBlocked, want ActionStay — blocked is the alertable code")
	}
}

// The whole point of the mode over disabling every account: everything a
// reporting caller renders is still computed. A gate placed before Rank would
// blank the pool, the mode, and the hover table hanging off it.
func TestManualModeStillRanksTheWholePool(t *testing.T) {
	plan := Decide(manualCands(), opts(), Config{Manual: true}, NewState(), "live")

	if len(plan.Result.Order) != 2 {
		t.Fatalf("Order = %v, want both accounts ranked", order(plan.Result))
	}
	if got := order(plan.Result); got[0] != "roomy" {
		t.Errorf("Order = %v, want the roomy account first — the ranking is unaffected", got)
	}
}

// Manual mode is the operative fact whatever else is true. Reporting
// ReasonNoCandidates on an empty pool would send a user to fix a pool that was
// never going to be used.
func TestManualModeOutranksAnEmptyPoolsOwnReason(t *testing.T) {
	plan := Decide(nil, opts(), Config{Manual: true}, NewState(), "live")

	if plan.Action != ActionStay {
		t.Errorf("Action = %v, want ActionStay", plan.Action)
	}
	if plan.Reason != ReasonManual {
		t.Errorf("Reason = %v, want ReasonManual, not the empty pool's own reason", plan.Reason)
	}
}

// Pre-emption runs BEFORE the anti-flap margins and is the one arm that could
// have slipped past a gate placed lower down. This is the shape that flapped:
// a live account projected to run out before the next reading.
func TestManualModeAlsoHoldsThePreemptiveSwitch(t *testing.T) {
	cands := []Candidate{
		sub("live", snap(win(87, 20*time.Minute), win(40, 48*time.Hour))),
		sub("roomy", snap(win(5, 4*time.Hour), win(10, 48*time.Hour))),
	}
	o := opts()
	o.PreemptLead = 6 * time.Minute

	if plan := Decide(cands, o, Config{}, NewState(), "live"); plan.Action != ActionSwitch {
		t.Fatalf("control Action = %v, want ActionSwitch — the fixture must pre-empt without the mode", plan.Action)
	}
	plan := Decide(cands, o, Config{Manual: true}, NewState(), "live")
	if plan.Action != ActionStay || plan.Reason != ReasonManual {
		t.Errorf("Action, Reason = %v, %v, want ActionStay, ReasonManual", plan.Action, plan.Reason)
	}
}

// The mode composes with hover rather than conflicting: hover decides what the
// numbers are, manual decides whether ccdad acts on them, and a manual-mode user
// watching hover's derived table is the case the two are useful together for.
func TestManualModeStillDerivesHoversTable(t *testing.T) {
	o := opts()
	o.Hover = true

	plan := Decide(manualCands(), o, Config{Manual: true}, NewState(), "live")
	if plan.Action != ActionStay || plan.Reason != ReasonManual {
		t.Fatalf("Action, Reason = %v, %v, want ActionStay, ReasonManual", plan.Action, plan.Reason)
	}
	if plan.Hover == nil {
		t.Error("Hover = nil, want the derived plan — watching it is why the two modes compose")
	}
}

func TestReasonManualHasItsOwnSentence(t *testing.T) {
	if got := ReasonManual.String(); got == "unknown" || got == "" {
		t.Errorf("ReasonManual.String() = %q, want a sentence of its own", got)
	}
}

package switcher

import (
	"testing"

	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// Under hover the move --force may take is the one a hold named (Plan.Held),
// and nothing else: hoverMove picks the target before the cooldown, so a hold
// with nothing held is a state with nowhere to go, and the order's first
// account there is at a limit like the live one. The configured strategies have
// no Held on every hold, and keep their old answer, the order's first account.
func TestAForcedSwitchTakesOnlyTheMoveAHoldNamed(t *testing.T) {
	order := []strategy.Ranked{{UUID: "u-2"}, {UUID: "u-1"}}
	hover := &strategy.HoverPlan{}

	held := strategy.Plan{Hover: hover, Held: strategy.Ranked{UUID: "u-3"}}
	held.Result.Order = order
	if got, ok := forceableTarget(held, "u-1"); !ok || got.UUID != "u-3" {
		t.Errorf("forceable %q, %v; want the held u-3", got.UUID, ok)
	}

	nothingHeld := strategy.Plan{Hover: hover}
	nothingHeld.Result.Order = order
	if got, ok := forceableTarget(nothingHeld, "u-1"); ok {
		t.Errorf("forceable %q under hover with nothing held; want none", got.UUID)
	}

	configured := strategy.Plan{}
	configured.Result.Order = order
	if got, ok := forceableTarget(configured, "u-1"); !ok || got.UUID != "u-2" {
		t.Errorf("forceable %q, %v under a configured strategy; want the order's first, u-2", got.UUID, ok)
	}
}

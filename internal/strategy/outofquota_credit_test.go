package strategy

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// seatAt is a PRIMARY credit seat -- the enterprise shape, metered on
// extra_usage rather than on plan windows -- at pct of its allowance.
func seatAt(uuid string, pct float64) Candidate {
	c := creditWith(uuid, usage.ExtraUsageFor(usage.ExtraUsageInput{
		State:        usage.ExtraUsageEnabled,
		Currency:     "USD",
		MonthlyLimit: pf(10000),
		UsedCredits:  pf(0),
		Utilization:  pf(pct),
	}))
	c.Primary = true
	return c
}

// A seat billed in credits carries no plan window, so "empty" has to be read off
// the allowance. It is the same distinction the subscription accounts get: past
// the threshold is one fact, nothing left is another.
func TestOutOfQuotaReadsACreditSeatsAllowance(t *testing.T) {
	o := opts()
	for _, tc := range []struct {
		pct       float64
		wantSpent bool
		wantEmpty bool
		wantTier  int
	}{
		{40, false, false, 0},
		{97, true, false, 2}, // past HoverCreditThreshold territory, still has 3 points
		{100, true, true, 3}, // nothing left
	} {
		r := measure(seatAt("c", tc.pct), o)
		spent, _ := Spent(r.Headroom)
		empty, _ := OutOfQuota(r.Headroom)
		if spent != tc.wantSpent || empty != tc.wantEmpty || headroomTier(r) != tc.wantTier {
			t.Errorf("seat at %.0f%%: spent=%v empty=%v tier=%d, want %v/%v/%d",
				tc.pct, spent, empty, headroomTier(r), tc.wantSpent, tc.wantEmpty, tc.wantTier)
		}
	}
}

// An empty credit seat must not outrank a subscription account that still has
// room -- the inversion the cap produces for subscriptions has the same shape
// here, because HoverCreditThreshold caps a seat's slack at -5.
func TestAnEmptyCreditSeatRanksBehindAUsableSubscription(t *testing.T) {
	o := opts()
	o.Hover = true
	cands := []Candidate{
		seatAt("c-empty", 100),
		sub("s-past-line", snap(win(0, 4*time.Hour), elapsedWindow(7*24*time.Hour, 0.15, 53))),
	}
	r := Rank(cands, o)
	if got := order(r); got[0] != "s-past-line" {
		t.Errorf("order = %v, want the subscription with 47 points left ahead of the empty seat", got)
	}
}

// And the engine must be able to LEAVE an empty seat. The margin runs on slack,
// which saturates for an empty account, so without the gate's own test the
// ranking would file the seat last and then refuse every move off it.
func TestTheEngineCanLeaveAnEmptyCreditSeat(t *testing.T) {
	o := opts()
	o.Hover = true
	cands := []Candidate{
		seatAt("c-empty", 100),
		sub("s-past-line", snap(win(0, 4*time.Hour), elapsedWindow(7*24*time.Hour, 0.15, 53))),
	}
	p := Decide(cands, o, Config{}, NewState(), "c-empty")
	want(t, p, ActionSwitch, ReasonBetterTarget, "s-past-line")
}

// What opens the LAST-RESORT pool, and on whose number.
//
// The CONFIGURED threshold still opens it: that number is the user saying where
// they want ccdad to stop using an account, and stopping is what makes the paid
// pool the next thing to reach for. It is not the money opt-in -- max_auto_spend
// is, and it defaults to 0 -- but it is the line that says the free pool is
// done.
//
// Hover's threshold does NOT open it. See TestHoverThresholdDoesNotOpenTheCreditPool.
func TestTheLastResortPoolOpensOnTheConfiguredThreshold(t *testing.T) {
	overage := creditWith("last-resort", usage.ExtraUsageFor(usage.ExtraUsageInput{
		State:        usage.ExtraUsageEnabled,
		Currency:     "USD",
		MonthlyLimit: pf(10000),
		UsedCredits:  pf(0),
		Utilization:  pf(10),
	}))
	if overage.Kind != identity.KindCredit || overage.Primary {
		t.Fatal("fixture is not a last-resort credit account")
	}

	o := opts()
	full := snap(win(0, 4*time.Hour), win(0, 48*time.Hour))
	empty := snap(win(100, 4*time.Hour), win(100, 48*time.Hour))
	pastLine := snap(win(0, 4*time.Hour), win(85, 48*time.Hour))

	cases := []struct {
		name string
		main []Candidate
		want bool
	}{
		{"a subscription with room holds it closed", []Candidate{sub("a", full), overage}, false},
		{"past the user's own threshold opens it", []Candidate{sub("a", pastLine), overage}, true},
		{"a primary seat with allowance holds it closed", []Candidate{sub("a", empty), seatAt("c", 40), overage}, false},
		{"every main account empty opens it", []Candidate{sub("a", empty), seatAt("c", 100), overage}, true},
	}
	for _, tc := range cases {
		if got := MainPoolExhausted(tc.main, o); got != tc.want {
			t.Errorf("%s: MainPoolExhausted = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestAFleetOfOnlyCreditAccountsStillSwitches is the case an enterprise
// installation is made entirely of, and it was the one case with no test.
//
// Every credit fleet elsewhere in this package carries at least one
// subscription account, so the branch that fires when Result.Order is EMPTY was
// never exercised. It stayed, permanently, with the reason "no subscription
// account has room" — a pool the user does not have and cannot make. Not a
// refusal to spend: the credit pool was ranked correctly, gated correctly, and
// then never consulted, because the guard above it returned first.
//
// The guard's own comment says it is for "an engine already on a credit account
// with no subscription room to return to". A subscription pool that does not
// exist is not a pool with no room, and this is the difference.
func TestAFleetOfOnlyCreditAccountsStillSwitches(t *testing.T) {
	spent := creditWith("b", enabledExtra(f(10000), f(9000)))
	roomy := creditWith("a", enabledExtra(f(10000), f(1000)))
	cands := []Candidate{spent, roomy}

	p := Decide(cands, opts(), Config{MaxAutoSpend: 200}, NewState(), "b")

	want(t, p, ActionSwitch, ReasonBetterTarget, "a")
	if !p.CreditConsulted {
		t.Error("CreditConsulted = false — the credit pool was ranked and never looked at")
	}
}

// TestAFleetOfOnlyCreditAccountsWithNoCeilingStillRefuses is the guard on the
// test above. Opening the branch must not open the WALLET: credit.max_auto_spend
// ships at 0, and that opt-out has to keep refusing exactly as it did before.
func TestAFleetOfOnlyCreditAccountsWithNoCeilingStillRefuses(t *testing.T) {
	cands := []Candidate{
		creditWith("b", enabledExtra(f(10000), f(9000))),
		creditWith("a", enabledExtra(f(10000), f(1000))),
	}

	p := Decide(cands, opts(), Config{}, NewState(), "b")

	want(t, p, ActionBlocked, ReasonCreditGate, "")
	if p.Credit.Reason != CreditNotOptedIn {
		t.Errorf("Credit.Reason = %v, want %v — the ceiling is the opt-in and it was never given", p.Credit.Reason, CreditNotOptedIn)
	}
}

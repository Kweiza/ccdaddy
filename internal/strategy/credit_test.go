package strategy

import (
	"math"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

func f(v float64) *float64 { return &v }

func TestCreditRoomArmsAtNinetyPercentOfTheCap(t *testing.T) {
	// No account limit, so the cap is the configured ceiling.
	room, ok := CreditRoom(SpendInfo{Enabled: true, Used: f(0)}, 100)
	if !ok {
		t.Fatal("ok = false for an enabled account with a $100 ceiling and nothing spent")
	}
	if room != 90 {
		t.Errorf("room = %v, want 90 — a switch arms at 90%% of the cap so it never lands exactly on the ceiling", room)
	}
}

// The cap is min(account limit, configured ceiling), and the arm fraction
// multiplies the CAP, not the ceiling alone.
func TestCreditRoomTakesTheSmallerOfTheAccountLimitAndTheCeiling(t *testing.T) {
	cases := []struct {
		name    string
		limit   *float64
		ceiling float64
		want    float64
	}{
		{"account limit binds", f(50), 100, 45},
		{"ceiling binds", f(200), 100, 90},
		{"no account limit", nil, 100, 90},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			room, ok := CreditRoom(SpendInfo{Enabled: true, Limit: tc.limit, Used: f(0)}, tc.ceiling)
			if !ok || room != tc.want {
				t.Errorf("CreditRoom() = %v, %v; want %v", room, ok, tc.want)
			}
		})
	}
}

func TestCreditRoomSubtractsWhatIsAlreadySpent(t *testing.T) {
	room, ok := CreditRoom(SpendInfo{Enabled: true, Limit: f(100), Used: f(80)}, 100)
	if !ok {
		t.Fatal("ok = false with $10 of armed room left")
	}
	if room != 10 {
		t.Errorf("room = %v, want 10 (90%% of 100, less 80 spent)", room)
	}
}

func TestCreditRoomRefusesWhenTheArmedCapIsSpent(t *testing.T) {
	room, ok := CreditRoom(SpendInfo{Enabled: true, Limit: f(100), Used: f(90)}, 100)
	if ok {
		t.Errorf("ok = true with room = %v; exactly at the armed cap is not room", room)
	}
}

// Two INDEPENDENT opt-ins. The account's own overage switch is one, and the
// user's configured ceiling is the other, so a single boolean cannot express it.
func TestCreditRoomNeedsBothOptIns(t *testing.T) {
	if _, ok := CreditRoom(SpendInfo{Enabled: false, Used: f(0)}, 100); ok {
		t.Error("ok = true with the account's overage switch off")
	}
	if _, ok := CreditRoom(SpendInfo{Enabled: true, Used: f(0)}, 0); ok {
		t.Error("ok = true with a ceiling of 0, which is the default and means 'never'")
	}
}

// max_auto_spend = inf is valid TOML, and an infinite ceiling with no account
// cap is unlimited unattended spending. NaN slips a plain `<= 0` because every
// NaN comparison is false. Both checks are load-bearing, not decoration.
func TestCreditRoomRefusesAnInfiniteOrNaNCeiling(t *testing.T) {
	for _, ceiling := range []float64{math.Inf(1), math.Inf(-1), math.NaN()} {
		room, ok := CreditRoom(SpendInfo{Enabled: true, Used: f(0)}, ceiling)
		if ok {
			t.Errorf("ceiling %v: ok = true with room = %v", ceiling, room)
		}
		// The refusal has to be a refusal WITH a number, not with a NaN: a
		// caller that reports the room it was denied would otherwise print one.
		if room != 0 {
			t.Errorf("ceiling %v: room = %v, want 0", ceiling, room)
		}
	}
}

func TestCreditRoomRefusesANegativeCeiling(t *testing.T) {
	if _, ok := CreditRoom(SpendInfo{Enabled: true, Used: f(0)}, -1); ok {
		t.Error("ok = true for a negative ceiling")
	}
}

// used_credits is number|null in the real schema. Wire drift that drops it must
// not read as "$0 spent, full cap available" — fail closed on money.
func TestCreditRoomRefusesWhenSpendCannotBeRead(t *testing.T) {
	room, ok := CreditRoom(SpendInfo{Enabled: true, Limit: f(100), Used: nil}, 100)
	if ok {
		t.Errorf("ok = true with room = %v; an unreadable spend must not hand back the full cap", room)
	}
	if room != 0 {
		t.Errorf("room = %v, want 0", room)
	}
}

// A NaN that arrives in the account's own numbers has to fail closed too, for
// the same reason a NaN ceiling does.
func TestCreditRoomRefusesNaNSpendFigures(t *testing.T) {
	if _, ok := CreditRoom(SpendInfo{Enabled: true, Limit: f(math.NaN()), Used: f(0)}, 100); ok {
		t.Error("ok = true for a NaN account limit")
	}
	if _, ok := CreditRoom(SpendInfo{Enabled: true, Limit: f(100), Used: f(math.NaN())}, 100); ok {
		t.Error("ok = true for a NaN used figure")
	}
}

// ---- the gate --------------------------------------------------------------

func enabledExtra(limit, used *float64) usage.ExtraUsage {
	return usage.ExtraUsageFor(usage.ExtraUsageEnabled, "", limit, used)
}

// Step 1 and 2: the credit pool is consulted only once the subscription pool is
// EXHAUSTED — not merely once it has failed hysteresis.
func TestCreditGateWaitsForTheSubscriptionPoolToBeExhausted(t *testing.T) {
	g := CreditGate(enabledExtra(f(100), f(0)), 100, false)

	if g.Allow {
		t.Error("Allow = true while the subscription pool still has a target")
	}
	if g.Reason != CreditSubscriptionNotExhausted {
		t.Errorf("Reason = %v, want CreditSubscriptionNotExhausted", g.Reason)
	}
	if g.Blocked {
		t.Error("Blocked = true; there is nothing for the user to act on while subscription quota remains")
	}
}

func TestCreditGateAllowsAnArmedAccountOnceSubscriptionIsExhausted(t *testing.T) {
	g := CreditGate(enabledExtra(f(100), f(10)), 100, true)

	if !g.Allow {
		t.Fatalf("Allow = false, reason %v", g.Reason)
	}
	if g.Room != 80 {
		t.Errorf("Room = %v, want 80", g.Room)
	}
}

// Step 3: max_auto_spend of 0 is the default, and it means exit 4 and a
// notification — never a silent no-op. Reporting "nothing to do" here is the
// exact cswap conflation §9.3 calls out, and it turns a money-blocked engine
// into something a cron job cannot see.
func TestCreditGateBlocksLoudlyWhenSpendingIsNotOptedIn(t *testing.T) {
	g := CreditGate(enabledExtra(f(100), f(0)), 0, true)

	if g.Allow {
		t.Fatal("Allow = true with max_auto_spend at 0")
	}
	if g.Reason != CreditNotOptedIn {
		t.Errorf("Reason = %v, want CreditNotOptedIn", g.Reason)
	}
	if !g.Blocked {
		t.Error("Blocked = false; a refusal the user can act on must be exit 4 and a notification, not a silent no-op")
	}
}

// Step 4: refuse when spend cannot be read.
func TestCreditGateRefusesUnreadableSpend(t *testing.T) {
	g := CreditGate(enabledExtra(f(100), nil), 100, true)

	if g.Allow {
		t.Fatal("Allow = true with an unreadable spend")
	}
	if g.Reason != CreditSpendUnreadable {
		t.Errorf("Reason = %v, want CreditSpendUnreadable", g.Reason)
	}
	if !g.Blocked {
		t.Error("Blocked = false; a credit account ccdad cannot price is something the user should hear about")
	}
}

func TestCreditGateReadsTheFourExtraUsageStates(t *testing.T) {
	cases := []struct {
		state      usage.ExtraUsageState
		reason     string
		wantReason CreditReason
	}{
		{usage.ExtraUsageDisabled, "", CreditAccountDisabled},
		{usage.ExtraUsageBlocked, "org_spend_cap_reached", CreditAccountBlocked},
		{usage.ExtraUsageUnknown, "", CreditSpendUnreadable},
	}
	for _, tc := range cases {
		t.Run(tc.wantReason.String(), func(t *testing.T) {
			g := CreditGate(usage.ExtraUsageFor(tc.state, tc.reason, f(100), f(0)), 100, true)
			if g.Allow {
				t.Fatal("Allow = true")
			}
			if g.Reason != tc.wantReason {
				t.Errorf("Reason = %v, want %v", g.Reason, tc.wantReason)
			}
			if !g.Blocked {
				t.Error("Blocked = false; every credit refusal after the subscription pool is exhausted is actionable")
			}
		})
	}
}

// The organization's own refusal is worth repeating back verbatim: it is the
// difference between "turn overage on" and "your org hit its spend cap".
func TestCreditGateCarriesTheOrganizationsReason(t *testing.T) {
	g := CreditGate(usage.ExtraUsageFor(usage.ExtraUsageBlocked, "out_of_credits", f(100), f(0)), 100, true)

	if g.DisabledReason != "out_of_credits" {
		t.Errorf("DisabledReason = %q, want out_of_credits", g.DisabledReason)
	}
}

func TestCreditGateReportsNoRoomLeft(t *testing.T) {
	g := CreditGate(enabledExtra(f(100), f(95)), 100, true)

	if g.Allow {
		t.Fatal("Allow = true past the armed cap")
	}
	if g.Reason != CreditNoRoom {
		t.Errorf("Reason = %v, want CreditNoRoom", g.Reason)
	}
	if !g.Blocked {
		t.Error("Blocked = false; an exhausted credit pool is exit 4")
	}
}

// An invalid ceiling is a configuration mistake, and saying so is more useful
// than reporting it as "you never opted in".
func TestCreditGateNamesAnInvalidCeilingSeparately(t *testing.T) {
	for _, ceiling := range []float64{math.Inf(1), math.NaN(), -1} {
		g := CreditGate(enabledExtra(f(100), f(0)), ceiling, true)
		if g.Allow {
			t.Fatalf("ceiling %v: Allow = true", ceiling)
		}
		if g.Reason != CreditCeilingInvalid {
			t.Errorf("ceiling %v: Reason = %v, want CreditCeilingInvalid", ceiling, g.Reason)
		}
	}
}

func TestCreditReasonsAllHaveNames(t *testing.T) {
	for r := CreditAllowed; r <= CreditCeilingInvalid; r++ {
		if r.String() == "" || r.String() == "unknown" {
			t.Errorf("CreditReason(%d) has no name; it reaches a user-facing notification", r)
		}
	}
}

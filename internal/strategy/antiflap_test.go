package strategy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/oauth"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// hr builds a subscription account whose single five-hour window leaves
// headroom percent of the plan and rolls over in resetsIn. One window keeps the
// binding window unambiguous, so a test that means to move headroom does not
// also move which window the recovery comes from.
func hr(uuid string, headroom float64, resetsIn time.Duration) Candidate {
	return sub(uuid, &usage.Snapshot{FiveHour: win(100-headroom, resetsIn)})
}

// hrNoReset is the same account with no rollover reported.
func hrNoReset(uuid string, headroom float64) Candidate {
	p := 100 - headroom
	return sub(uuid, &usage.Snapshot{FiveHour: usage.NewWindow(&p, nil)})
}

// weekly builds an account whose perishable seven-day quota expires in
// resetsIn, which is the axis consume-first ranks on.
func weekly(uuid string, headroom float64, resetsIn time.Duration) Candidate {
	return sub(uuid, &usage.Snapshot{SevenDay: win(100-headroom, resetsIn)})
}

func creditWith(uuid string, e usage.ExtraUsage) Candidate {
	return credit(uuid, &usage.Snapshot{ExtraUsage: e})
}

func at(d time.Duration) Options {
	o := opts()
	o.Now = now.Add(d)
	return o
}

func want(t *testing.T, p Plan, action Action, reason Reason, target string) {
	t.Helper()
	if p.Action != action {
		t.Fatalf("Action = %v (%v), want %v", p.Action, p.Reason, action)
	}
	if p.Reason != reason {
		t.Fatalf("Reason = %v, want %v", p.Reason, reason)
	}
	if p.Target.UUID != target {
		t.Fatalf("Target = %q, want %q", p.Target.UUID, target)
	}
}

// ---- the ordinary move -----------------------------------------------------

func TestDecideMovesToAClearlyBetterAccount(t *testing.T) {
	p := Decide([]Candidate{hr("a", 20, time.Hour), hr("b", 50, time.Hour)}, opts(), Config{}, NewState(), "a")
	want(t, p, ActionSwitch, ReasonBetterTarget, "b")
}

func TestDecideStaysWhenTheLiveAccountAlreadyTopsTheRanking(t *testing.T) {
	p := Decide([]Candidate{hr("a", 20, time.Hour), hr("b", 50, time.Hour)}, opts(), Config{}, NewState(), "b")
	want(t, p, ActionStay, ReasonAlreadyBest, "")
}

// ---- the paragraph under the table -----------------------------------------

// The headline requirement of §7.2: "These bound the flap RATE; they do not
// make a reverse move impossible. Headroom changes, so a target that burns down
// must be able to lose its position."
//
// The naive implementation latches the chosen target until it is exhausted,
// which passes every anti-flap test anyone would write and fails the product.
func TestAReverseMoveIsPossibleOnceTheTargetHasBurnedDown(t *testing.T) {
	st := NewState()

	forward := Decide([]Candidate{hr("a", 20, time.Hour), hr("b", 50, time.Hour)}, opts(), Config{}, st, "a")
	want(t, forward, ActionSwitch, ReasonBetterTarget, "b")
	st.RecordSwitch("b", now)

	// b has burned from 50 points of headroom down to 9 while a sat still.
	// Nothing latches, so the numbers alone put a back in front.
	back := Decide([]Candidate{hr("a", 20, time.Hour), hr("b", 9, time.Hour)}, at(10*time.Minute), Config{}, st, "b")
	want(t, back, ActionSwitch, ReasonBetterTarget, "a")
}

// The other half of the same requirement: the margins still bound the RATE, so
// a candidate that is merely ahead does not get to move.
func TestAReverseMoveIsRefusedWhileTheBurnIsSmall(t *testing.T) {
	st := NewState()
	st.RecordSwitch("b", now.Add(-time.Hour))

	p := Decide([]Candidate{hr("a", 20, time.Hour), hr("b", 15, time.Hour)}, opts(), Config{}, st, "b")
	want(t, p, ActionStay, ReasonHysteresis, "")
}

// §7.2's ratio is 2.0 and the spec's own gloss is "a reverse move needs a 4x
// relative burn". That squaring is not a second mechanism: it falls out of
// applying the same 2.0 to every move, so a round trip has to clear it twice in
// opposite directions.
func TestTheHeadroomRatioMakesARoundTripCostFourfold(t *testing.T) {
	st := NewState()

	// The forward move needs 2x: 39 against 20 is not enough, 40 is.
	near := Decide([]Candidate{hr("a", 20, time.Hour), hr("b", 39, time.Hour)}, opts(), Config{}, st, "a")
	want(t, near, ActionStay, ReasonHeadroomRatio, "")

	forward := Decide([]Candidate{hr("a", 20, time.Hour), hr("b", 40, time.Hour)}, opts(), Config{}, st, "a")
	want(t, forward, ActionSwitch, ReasonBetterTarget, "b")

	// Coming back needs 2x in the other direction. b started with twice a's
	// headroom, so it has to fall to half of it — a factor of four.
	stillAhead := Decide([]Candidate{hr("a", 20, time.Hour), hr("b", 11, time.Hour)}, opts(), Config{}, st, "b")
	want(t, stillAhead, ActionStay, ReasonHysteresis, "")

	returned := Decide([]Candidate{hr("a", 20, time.Hour), hr("b", 10, time.Hour)}, opts(), Config{}, st, "b")
	want(t, returned, ActionSwitch, ReasonBetterTarget, "a")
}

// ---- the two headroom margins, isolated from each other ---------------------

// The additive margin refuses a move the RATIO would have allowed, so this
// cannot pass by the ratio doing the work.
func TestTheHysteresisMarginRefusesOnItsOwn(t *testing.T) {
	cfg := Config{HysteresisPct: 30, HeadroomRatio: 1}

	p := Decide([]Candidate{hr("a", 25, time.Hour), hr("b", 45, time.Hour)}, opts(), cfg, NewState(), "a")
	want(t, p, ActionStay, ReasonHysteresis, "")

	// 45 is 1.8x of 25, so the ratio of 1 is satisfied and only the 30-point
	// margin is refusing.
	cfg.HysteresisPct = 20
	p = Decide([]Candidate{hr("a", 25, time.Hour), hr("b", 45, time.Hour)}, opts(), cfg, NewState(), "a")
	want(t, p, ActionSwitch, ReasonBetterTarget, "b")
}

// And the ratio refuses a move the additive margin would have allowed.
func TestTheHeadroomRatioRefusesOnItsOwn(t *testing.T) {
	// 45 beats 30 by 15 points, which clears the 10-point margin, and is 1.5x,
	// which does not clear 2.0.
	p := Decide([]Candidate{hr("a", 30, time.Hour), hr("b", 45, time.Hour)}, opts(), Config{}, NewState(), "a")
	want(t, p, ActionStay, ReasonHeadroomRatio, "")
}

// With the active account at or past its limit there is nothing left to be
// twice as much as, and the ratio must not stand in the way of the switch the
// whole product exists to make.
func TestTheRatioDoesNotBlockAMoveOffASpentAccount(t *testing.T) {
	p := Decide([]Candidate{hr("a", 0, time.Hour), hr("b", 30, time.Hour)}, opts(), Config{}, NewState(), "a")
	want(t, p, ActionSwitch, ReasonBetterTarget, "b")
}

func TestAnUnreadableBaselineDoesNotBlockAMove(t *testing.T) {
	cands := []Candidate{sub("a", snap(unread(), unread())), hr("b", 50, time.Hour)}

	p := Decide(cands, opts(), Config{}, NewState(), "a")
	want(t, p, ActionSwitch, ReasonBetterTarget, "b")
}

// §7.1 files "we have no idea" ahead of "we know it is spent". That is a maybe
// worth trying, and a margin cannot be held against a number nobody has.
func TestAnUntriedCandidateIsNotHeldToAMarginItHasNoNumberFor(t *testing.T) {
	cands := []Candidate{hr("a", 5, time.Hour), sub("b", snap(unread(), unread()))}

	p := Decide(cands, opts(), Config{}, NewState(), "a")
	want(t, p, ActionSwitch, ReasonBetterTarget, "b")
}

// ---- cooldown --------------------------------------------------------------

func TestCooldownHoldsASecondSwitchOffAndSaysWhenItLifts(t *testing.T) {
	st := NewState()
	st.RecordSwitch("b", now)

	p := Decide([]Candidate{hr("a", 20, time.Hour), hr("b", 50, time.Hour)}, at(time.Minute), Config{}, st, "a")
	want(t, p, ActionStay, ReasonCooldown, "")
	if !p.HasRetryAt {
		t.Fatal("HasRetryAt = false; a cooldown knows exactly when it lifts")
	}
	if got, wantAt := p.RetryAt, now.Add(DefaultCooldown); !got.Equal(wantAt) {
		t.Errorf("RetryAt = %v, want %v", got, wantAt)
	}
}

func TestCooldownLapsesAfterFiveMinutes(t *testing.T) {
	st := NewState()
	st.RecordSwitch("b", now)

	p := Decide([]Candidate{hr("a", 20, time.Hour), hr("b", 50, time.Hour)}, at(DefaultCooldown), Config{}, st, "a")
	want(t, p, ActionSwitch, ReasonBetterTarget, "b")
}

// The cooldown bounds churn between two USABLE accounts. With the live login
// unattributable there is nothing underneath to churn against, and waiting is
// downtime rather than caution.
func TestCooldownDoesNotStrandAnEngineWithNothingLive(t *testing.T) {
	st := NewState()
	st.RecordSwitch("b", now)

	p := Decide([]Candidate{hr("a", 20, time.Hour), hr("b", 50, time.Hour)}, at(time.Minute), Config{}, st, "")
	want(t, p, ActionSwitch, ReasonBetterTarget, "b")
}

func TestCooldownDoesNotStrandAnEngineOnAQuarantinedAccount(t *testing.T) {
	st := NewState()
	st.RecordSwitch("a", now)
	st.Quarantine("a", now, time.Hour, "dead")

	p := Decide([]Candidate{hr("a", 50, time.Hour), hr("b", 20, time.Hour)}, at(time.Minute), Config{}, st, "a")
	want(t, p, ActionSwitch, ReasonBetterTarget, "b")
}

// "Already on the best" is not a move, so the cooldown has nothing to refuse
// and the caller must not be told a switch was held back.
func TestBeingOnTheBestAccountOutranksTheCooldown(t *testing.T) {
	st := NewState()
	st.RecordSwitch("b", now)

	p := Decide([]Candidate{hr("a", 20, time.Hour), hr("b", 50, time.Hour)}, at(time.Minute), Config{}, st, "b")
	want(t, p, ActionStay, ReasonAlreadyBest, "")
}

// ---- quarantine ------------------------------------------------------------

// Quarantine filters the pool BEFORE the ranking. Rejecting the winner
// afterwards would leave Result.Mode and AllOverThreshold answering about a
// pool containing an account nothing can use — and here that is the difference
// between §7.1's ordinary situation and its all-above-threshold one.
func TestQuarantineChangesTheSituationTheRankingIsMadeIn(t *testing.T) {
	st := NewState()
	st.Quarantine("a", now, time.Hour, "dead refresh token")
	cands := []Candidate{hr("a", 50, time.Hour), hr("b", 10, 30*time.Minute), hr("c", 5, 20*time.Minute)}

	p := Decide(cands, opts(), Config{}, st, "b")
	if !p.Result.AllOverThreshold {
		t.Error("AllOverThreshold = false; the only account with room is quarantined")
	}
	if p.Result.Mode != ModeRecovery {
		t.Errorf("Mode = %v, want recovery", p.Result.Mode)
	}
	if len(p.Quarantined) != 1 || p.Quarantined[0] != "a" {
		t.Errorf("Quarantined = %v, want [a]", p.Quarantined)
	}
	// And the ranking a quarantined account is absent from cannot hand it back
	// as a target.
	for _, r := range p.Result.Order {
		if r.UUID == "a" {
			t.Fatal("the quarantined account is still in the ranking")
		}
	}
}

func TestAQuarantineLapsesAtItsExpiry(t *testing.T) {
	st := NewState()
	st.Quarantine("b", now, time.Hour, "dead refresh token")
	cands := []Candidate{hr("a", 20, 4*time.Hour), hr("b", 50, 4*time.Hour)}

	held := Decide(cands, opts(), Config{}, st, "a")
	want(t, held, ActionStay, ReasonAlreadyBest, "")

	back := Decide(cands, at(time.Hour+time.Minute), Config{}, st, "a")
	want(t, back, ActionSwitch, ReasonBetterTarget, "b")
}

// A file that lost its timestamp must fail towards holding the account OUT: the
// only reason an account is in here is that its credential did not work.
func TestAQuarantineWithNoExpiryNeverLapses(t *testing.T) {
	q := Quarantine{Since: now}
	if !q.Active(now.Add(10 * 365 * 24 * time.Hour)) {
		t.Error("Active = false; a quarantine with no expiry must hold")
	}
}

// store.Add updates an existing uuid in place, so `ccdad add` re-authenticating
// an account has to lift the quarantine explicitly. Without this the user logs
// in again successfully and the engine goes on refusing to use the account.
func TestClearingAQuarantineLetsAReauthenticatedAccountBack(t *testing.T) {
	st := NewState()
	st.Quarantine("b", now, time.Hour, "dead refresh token")
	cands := []Candidate{hr("a", 20, 4*time.Hour), hr("b", 50, 4*time.Hour)}

	if !st.ClearQuarantine("b") {
		t.Fatal("ClearQuarantine = false; there was one to clear")
	}
	if st.ClearQuarantine("b") {
		t.Error("ClearQuarantine = true on the second call; there was nothing left to clear")
	}
	want(t, Decide(cands, opts(), Config{}, st, "a"), ActionSwitch, ReasonBetterTarget, "b")
}

func TestEveryEligibleAccountQuarantinedIsBlockedNotIdle(t *testing.T) {
	st := NewState()
	st.Quarantine("a", now, 2*time.Hour, "dead")
	st.Quarantine("b", now, time.Hour, "dead")
	cands := []Candidate{hr("a", 50, time.Hour), hr("b", 40, time.Hour)}

	p := Decide(cands, opts(), Config{}, st, "a")
	want(t, p, ActionBlocked, ReasonAllQuarantined, "")
	if !p.HasRetryAt || !p.RetryAt.Equal(now.Add(time.Hour)) {
		t.Errorf("RetryAt = %v/%v, want the soonest expiry %v", p.RetryAt, p.HasRetryAt, now.Add(time.Hour))
	}
}

func TestNoEligibleAccountAtAllIsBlocked(t *testing.T) {
	cands := []Candidate{
		{UUID: "a", Kind: identity.KindSubscription, Disabled: true},
		{UUID: "b", Kind: identity.KindAPIKey},
	}

	want(t, Decide(cands, opts(), Config{}, NewState(), ""), ActionBlocked, ReasonNoCandidates, "")
	want(t, Decide(nil, opts(), Config{}, NewState(), ""), ActionBlocked, ReasonNoCandidates, "")
}

// ---- the recovery axis -----------------------------------------------------

func TestRecoveryHysteresisNeedsThreeHundredSeconds(t *testing.T) {
	shy := []Candidate{hr("a", 10, 30*time.Minute), hr("b", 10, 26*time.Minute)}
	want(t, Decide(shy, opts(), Config{}, NewState(), "a"), ActionStay, ReasonRecoveryHysteresis, "")

	enough := []Candidate{hr("a", 10, 30*time.Minute), hr("b", 10, 25*time.Minute)}
	want(t, Decide(enough, opts(), Config{}, NewState(), "a"), ActionSwitch, ReasonBetterTarget, "b")
}

// §7.1 tiers this mode: an account returning inside the horizon beats one that
// does not, whatever its headroom. That is a categorical difference, so no
// margin measured in headroom may stand in front of it — which is exactly the
// switch §7.1 says the engine has to make.
func TestReturningInsideTheHorizonNeedsNoHeadroomMargin(t *testing.T) {
	cands := []Candidate{hr("a", 19, 2*time.Hour), hr("b", 1, 30*time.Minute)}

	p := Decide(cands, opts(), Config{}, NewState(), "a")
	if p.Result.Mode != ModeRecovery {
		t.Fatalf("Mode = %v, want recovery", p.Result.Mode)
	}
	want(t, p, ActionSwitch, ReasonBetterTarget, "b")
}

// Outside the horizon §7.1 orders on headroom again, so the margin goes back
// onto that axis.
func TestOutsideTheHorizonTheHeadroomMarginsApply(t *testing.T) {
	shy := []Candidate{hr("a", 5, 3*time.Hour), hr("b", 12, 4*time.Hour)}
	p := Decide(shy, opts(), Config{}, NewState(), "a")
	if p.Result.Mode != ModeRecovery {
		t.Fatalf("Mode = %v, want recovery", p.Result.Mode)
	}
	want(t, p, ActionStay, ReasonHysteresis, "")

	enough := []Candidate{hr("a", 5, 3*time.Hour), hr("b", 19, 4*time.Hour)}
	want(t, Decide(enough, opts(), Config{}, NewState(), "a"), ActionSwitch, ReasonBetterTarget, "b")
}

// ---- consume-first ---------------------------------------------------------

func TestConsumeFirstHoldsAMarginOnTheWeeklyResetAxis(t *testing.T) {
	o := opts()
	o.Strategy = StrategyConsumeFirst

	shy := []Candidate{weekly("a", 60, 48*time.Hour), weekly("b", 60, 48*time.Hour-4*time.Minute)}
	p := Decide(shy, o, Config{}, NewState(), "a")
	if p.Result.Mode != ModeConsumeFirst {
		t.Fatalf("Mode = %v, want consume-first", p.Result.Mode)
	}
	want(t, p, ActionStay, ReasonWeeklyResetMargin, "")

	enough := []Candidate{weekly("a", 60, 48*time.Hour), weekly("b", 60, 48*time.Hour-10*time.Minute)}
	want(t, Decide(enough, o, Config{}, NewState(), "a"), ActionSwitch, ReasonBetterTarget, "b")
}

// Consume-first spends perishable quota, so it must not be gated on headroom:
// the account whose week expires soonest is the point, however much is left in
// it.
func TestConsumeFirstIgnoresTheHeadroomMargins(t *testing.T) {
	o := opts()
	o.Strategy = StrategyConsumeFirst
	cands := []Candidate{weekly("a", 90, 48*time.Hour), weekly("b", 30, 24*time.Hour)}

	want(t, Decide(cands, o, Config{}, NewState(), "a"), ActionSwitch, ReasonBetterTarget, "b")
}

func TestConsumeFirstMovesWhenOnlyTheCandidateHasAWeeklyReset(t *testing.T) {
	o := opts()
	o.Strategy = StrategyConsumeFirst
	cands := []Candidate{hrNoReset("a", 60), weekly("b", 60, 48*time.Hour)}

	want(t, Decide(cands, o, Config{}, NewState(), "a"), ActionSwitch, ReasonBetterTarget, "b")
}

// ---- the credit gate's place in the order ----------------------------------

func TestTheCreditPoolIsNotReachedWhileSubscriptionQuotaRemains(t *testing.T) {
	cands := []Candidate{hr("a", 50, time.Hour), creditWith("c", enabledExtra(nil, f(0)))}

	p := Decide(cands, opts(), Config{MaxAutoSpend: 100}, NewState(), "a")
	want(t, p, ActionStay, ReasonAlreadyBest, "")
	if p.CreditConsulted {
		t.Error("CreditConsulted = true; §7.3 reaches the credit pool only once subscription is EXHAUSTED")
	}
	if p.SubscriptionExhausted {
		t.Error("SubscriptionExhausted = true; a has room")
	}
}

// max_auto_spend defaults to 0, so an exhausted user with a credit account gets
// exit 4 and a notification rather than a silent charge.
func TestAnExhaustedPoolWithNoOptInIsBlockedByTheCreditGate(t *testing.T) {
	cands := []Candidate{hr("a", 5, 4*time.Hour), creditWith("c", enabledExtra(nil, f(0)))}

	p := Decide(cands, opts(), Config{}, NewState(), "a")
	want(t, p, ActionBlocked, ReasonCreditGate, "")
	if !p.CreditConsulted {
		t.Fatal("CreditConsulted = false; the pool was exhausted, so the gate had to answer")
	}
	if p.Credit.Reason != CreditNotOptedIn {
		t.Errorf("Credit.Reason = %v, want %v", p.Credit.Reason, CreditNotOptedIn)
	}
}

func TestTheCreditPoolIsUsedOnceSubscriptionIsExhaustedAndOptedIn(t *testing.T) {
	cands := []Candidate{hr("a", 5, 4*time.Hour), creditWith("c", enabledExtra(nil, f(0)))}

	p := Decide(cands, opts(), Config{MaxAutoSpend: 100}, NewState(), "a")
	want(t, p, ActionSwitch, ReasonBetterTarget, "c")
	if !p.Credit.Allow || p.Credit.Room != 90 {
		t.Errorf("Credit = %+v, want an allowed 90 of armed room", p.Credit)
	}
}

// The cooldown still applies to a move that costs money.
func TestTheCooldownAppliesToACreditSwitchToo(t *testing.T) {
	st := NewState()
	st.RecordSwitch("a", now)
	cands := []Candidate{hr("a", 5, 4*time.Hour), creditWith("c", enabledExtra(nil, f(0)))}

	want(t, Decide(cands, at(time.Minute), Config{MaxAutoSpend: 100}, st, "a"), ActionStay, ReasonCooldown, "")
}

func TestAnExhaustedPoolWithNoCreditAccountIsBlocked(t *testing.T) {
	cands := []Candidate{hr("a", 5, 4*time.Hour), hr("b", 3, 5*time.Hour)}

	p := Decide(cands, opts(), Config{}, NewState(), "a")
	want(t, p, ActionBlocked, ReasonAllExhausted, "")
	if p.CreditConsulted {
		t.Error("CreditConsulted = true; there is no credit account to consult about")
	}
}

// A credit account that cannot be priced refuses rather than reading as $0
// spent against the full cap.
func TestAnUnreadableCreditAccountRefusesRatherThanSpending(t *testing.T) {
	cands := []Candidate{hr("a", 5, 4*time.Hour), credit("c", nil)}

	p := Decide(cands, opts(), Config{MaxAutoSpend: 100}, NewState(), "a")
	want(t, p, ActionBlocked, ReasonCreditGate, "")
	if p.Credit.Reason != CreditSpendUnreadable {
		t.Errorf("Credit.Reason = %v, want %v", p.Credit.Reason, CreditSpendUnreadable)
	}
}

// Moving from a working credit account onto a spent subscription one would end
// the session on an account with nothing left and buy nothing.
func TestAnEngineOnCreditStaysWhenNoSubscriptionHasRoom(t *testing.T) {
	cands := []Candidate{hr("a", 5, 4*time.Hour), creditWith("c", enabledExtra(nil, f(0)))}

	want(t, Decide(cands, opts(), Config{MaxAutoSpend: 100}, NewState(), "c"), ActionStay, ReasonNoSubscriptionRoom, "")
}

// The moment paid-for quota is available again the engine leaves the pool that
// costs money.
func TestAnEngineOnCreditReturnsToASubscriptionWithRoom(t *testing.T) {
	cands := []Candidate{hr("a", 50, time.Hour), creditWith("c", enabledExtra(nil, f(0)))}

	want(t, Decide(cands, opts(), Config{MaxAutoSpend: 100}, NewState(), "c"), ActionSwitch, ReasonBetterTarget, "a")
}

// ---- Decide is a decision, not an action -----------------------------------

// A cooldown earned by a switch that was only PLANNED would hold the engine off
// the retry, and a plan is not a switch.
func TestDecideNeverWritesTheState(t *testing.T) {
	st := NewState()
	st.Quarantine("q", now, time.Hour, "dead")
	cands := []Candidate{hr("a", 20, time.Hour), hr("b", 50, time.Hour), hr("q", 90, time.Hour)}

	want(t, Decide(cands, opts(), Config{}, st, "a"), ActionSwitch, ReasonBetterTarget, "b")

	if last, _ := st.LastSwitch(); !last.IsZero() {
		t.Errorf("LastSwitch = %v after a decision; deciding is not switching", last)
	}
	if got := st.QuarantinedUUIDs(now); len(got) != 1 || got[0] != "q" {
		t.Errorf("QuarantinedUUIDs = %v, want [q] unchanged", got)
	}
}

func TestDecideToleratesANilState(t *testing.T) {
	want(t, Decide([]Candidate{hr("a", 20, time.Hour), hr("b", 50, time.Hour)}, opts(), Config{}, nil, "a"),
		ActionSwitch, ReasonBetterTarget, "b")
}

// ---- configuration ---------------------------------------------------------

// An under-populated Config must never read as "every anti-flap mechanism off".
func TestTheZeroConfigIsTheFullDefaultSet(t *testing.T) {
	c := Config{}.withDefaults()
	if c.HysteresisPct != DefaultHysteresisPct {
		t.Errorf("HysteresisPct = %v, want %v", c.HysteresisPct, DefaultHysteresisPct)
	}
	if c.HeadroomRatio != DefaultHeadroomRatio {
		t.Errorf("HeadroomRatio = %v, want %v", c.HeadroomRatio, DefaultHeadroomRatio)
	}
	if c.Cooldown != DefaultCooldown {
		t.Errorf("Cooldown = %v, want %v", c.Cooldown, DefaultCooldown)
	}
	if c.RecoveryHysteresis != DefaultRecoveryHysteresis {
		t.Errorf("RecoveryHysteresis = %v, want %v", c.RecoveryHysteresis, DefaultRecoveryHysteresis)
	}
	if c.QuarantineFor != DefaultQuarantine {
		t.Errorf("QuarantineFor = %v, want %v", c.QuarantineFor, DefaultQuarantine)
	}
}

// MaxAutoSpend is the one field whose zero is an answer rather than an
// omission: 0 IS the documented default and it means "never spend".
func TestTheDefaultsDoNotInventASpendCeiling(t *testing.T) {
	if c := (Config{}).withDefaults(); c.MaxAutoSpend != 0 {
		t.Errorf("MaxAutoSpend = %v, want 0 — spending is an explicit opt-in", c.MaxAutoSpend)
	}
}

func TestConfiguredMarginsAreHonoured(t *testing.T) {
	cands := []Candidate{hr("a", 30, time.Hour), hr("b", 45, time.Hour)}
	// The default 2.0 ratio refuses this move; 1.5 exactly allows it.
	want(t, Decide(cands, opts(), Config{HeadroomRatio: 1.5}, NewState(), "a"), ActionSwitch, ReasonBetterTarget, "b")

	st := NewState()
	st.RecordSwitch("a", now)
	want(t, Decide(cands, at(6*time.Minute), Config{HeadroomRatio: 1.5, Cooldown: 10 * time.Minute}, st, "a"),
		ActionStay, ReasonCooldown, "")
}

func TestActionsAndReasonsAllHaveNames(t *testing.T) {
	for a := ActionStay; a <= ActionBlocked; a++ {
		if a.String() == "unknown" {
			t.Errorf("Action(%d) has no name", a)
		}
	}
	if got := Action(200).String(); got != "unknown" {
		t.Errorf("Action(200) = %q, want unknown", got)
	}
	for r := ReasonBetterTarget; r <= ReasonNoSubscriptionRoom; r++ {
		if r.String() == "unknown" {
			t.Errorf("Reason(%d) has no name", r)
		}
	}
	if got := Reason(200).String(); got != "unknown" {
		t.Errorf("Reason(200) = %q, want unknown", got)
	}
}

// ---- the persisted state ---------------------------------------------------

func isolate(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CCDAD_HOME", root)
	return root
}

func TestCooldownRemainingReadsTheRecordedSwitch(t *testing.T) {
	st := NewState()
	if _, cooling := st.CooldownRemaining(now, DefaultCooldown); cooling {
		t.Error("a state that has never switched is not in cooldown")
	}

	st.RecordSwitch("a", now)
	left, cooling := st.CooldownRemaining(now.Add(2*time.Minute), DefaultCooldown)
	if !cooling || left != 3*time.Minute {
		t.Errorf("CooldownRemaining = %v/%v, want 3m", left, cooling)
	}
	if _, cooling := st.CooldownRemaining(now.Add(DefaultCooldown), DefaultCooldown); cooling {
		t.Error("the cooldown must lapse at exactly its length")
	}
}

// A LastSwitchAt in the future is a clock that moved backwards. Waiting is the
// conservative direction and the wait is bounded by the cooldown itself.
func TestCooldownHonoursAClockThatMovedBackwards(t *testing.T) {
	st := NewState()
	st.RecordSwitch("a", now.Add(time.Hour))

	if _, cooling := st.CooldownRemaining(now, DefaultCooldown); !cooling {
		t.Error("a switch stamped in the future must still hold the cooldown")
	}
}

// §7.2's cooldown exists to stop switch storms, and §8's daemon auto-restarts
// from any CLI command. An in-memory cooldown would reset on every restart and
// become the storm it exists to prevent.
func TestTheCooldownSurvivesARestart(t *testing.T) {
	isolate(t)

	if err := WithState(time.Second, func(s *State) error {
		s.RecordSwitch("b", now)
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}

	// A whole new process would do exactly this and nothing else.
	reloaded, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if last, to := reloaded.LastSwitch(); !last.Equal(now) || to != "b" {
		t.Fatalf("LastSwitch = %v/%q, want %v/b", last, to, now)
	}

	p := Decide([]Candidate{hr("a", 20, time.Hour), hr("b", 50, time.Hour)}, at(time.Minute), Config{}, reloaded, "a")
	want(t, p, ActionStay, ReasonCooldown, "")
}

func TestAQuarantineSurvivesARestart(t *testing.T) {
	isolate(t)

	if err := WithState(time.Second, func(s *State) error {
		s.Quarantine("b", now, time.Hour, "dead refresh token")
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}

	reloaded, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	q, held := reloaded.Quarantined("b", now.Add(time.Minute))
	if !held {
		t.Fatal("the quarantine did not survive the write")
	}
	if q.Reason != "dead refresh token" || !q.Until.Equal(now.Add(time.Hour)) {
		t.Errorf("Quarantine = %+v, want the reason and expiry that were written", q)
	}
	if _, held := reloaded.Quarantined("b", now.Add(2*time.Hour)); held {
		t.Error("the reloaded quarantine ignored its own expiry")
	}
}

func TestWithStateWritesAPrivateFile(t *testing.T) {
	root := isolate(t)

	if err := WithState(time.Second, func(s *State) error {
		s.RecordSwitch("a", now)
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}

	info, err := os.Stat(filepath.Join(root, "strategy.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %v, want 0600", perm)
	}
	if StatePath() != filepath.Join(root, "strategy.json") {
		t.Errorf("StatePath = %q, want it under CCDAD_HOME", StatePath())
	}
}

// A poll that failed halfway must not persist half a change.
func TestWithStateLeavesTheFileAloneWhenFnFails(t *testing.T) {
	root := isolate(t)
	if err := WithState(time.Second, func(s *State) error {
		s.RecordSwitch("a", now)
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(root, "strategy.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	boom := errors.New("boom")
	if err := WithState(time.Second, func(s *State) error {
		s.RecordSwitch("z", now.Add(time.Hour))
		s.Quarantine("z", now, time.Hour, "dead")
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("WithState error = %v, want boom", err)
	}

	after, err := os.ReadFile(filepath.Join(root, "strategy.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("the file changed after a failed fn:\n%s\n%s", before, after)
	}
}

// A state file that cannot be read degrades towards MORE switching, never less:
// a quarantine that cannot be read is not evidence that an account is dead, and
// refusing to switch on a corrupt file parks the engine exactly the way §7.2
// says never to.
func TestACorruptStateFileDegradesToEmptyAndSaysSo(t *testing.T) {
	root := isolate(t)
	if err := os.WriteFile(filepath.Join(root, "strategy.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	st, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState returned a hard error: %v", err)
	}
	if st.LoadError() == nil {
		t.Error("LoadError = nil; `ccdad doctor` has to be able to say the file is broken")
	}
	if _, cooling := st.CooldownRemaining(now, DefaultCooldown); cooling {
		t.Error("a corrupt file must not conjure a cooldown")
	}
	want(t, Decide([]Candidate{hr("a", 20, time.Hour), hr("b", 50, time.Hour)}, opts(), Config{}, st, "a"),
		ActionSwitch, ReasonBetterTarget, "b")
}

func TestAMissingStateFileIsNotAnError(t *testing.T) {
	isolate(t)

	st, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if st.LoadError() != nil {
		t.Errorf("LoadError = %v; a first run has no file and that is not a fault", st.LoadError())
	}
}

func TestLoadStateRefusesARelativeStoreRoot(t *testing.T) {
	// The cwd moves as well as the variable. A relative root resolves against
	// whatever directory the process is in, so if this guard ever regresses the
	// damage lands in a temp directory rather than in the package source tree —
	// which is exactly where a mutation run put it once.
	t.Chdir(t.TempDir())
	t.Setenv("CCDAD_HOME", "relative/ccdad")

	if _, err := LoadState(); err == nil {
		t.Error("LoadState accepted a relative store root; the cooldown would restart in every directory")
	}
	if err := WithState(time.Second, func(*State) error { return nil }); err == nil {
		t.Error("WithState accepted a relative store root")
	}
}

// The file is keyed by uuid and never by idx: store.sortAndReindex recompacts
// idx on every removal, so a file keyed on it would quarantine a different
// account after any `ccdad remove`.
func TestTheStateFileIsKeyedByUUID(t *testing.T) {
	root := isolate(t)
	if err := WithState(time.Second, func(s *State) error {
		s.Quarantine("11111111-2222-3333-4444-555555555555", now, time.Hour, "dead")
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(root, "strategy.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc struct {
		Quarantine map[string]json.RawMessage `json:"quarantine"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := doc.Quarantine["11111111-2222-3333-4444-555555555555"]; !ok {
		t.Errorf("quarantine keys = %v, want the account uuid", doc.Quarantine)
	}
}

// A removed account's quarantine must not come back to haunt the account added
// again under the same uuid.
func TestPruneDropsQuarantinesForAccountsThatAreGone(t *testing.T) {
	st := NewState()
	st.Quarantine("a", now, time.Hour, "dead")
	st.Quarantine("b", now, time.Hour, "dead")

	st.Prune(map[string]bool{"a": true})

	if got := st.QuarantinedUUIDs(now); len(got) != 1 || got[0] != "a" {
		t.Errorf("QuarantinedUUIDs = %v, want [a]", got)
	}
}

func TestQuarantinedUUIDsIsOrderedAndSkipsLapsedEntries(t *testing.T) {
	st := NewState()
	st.Quarantine("c", now, time.Hour, "dead")
	st.Quarantine("a", now, time.Hour, "dead")
	st.Quarantine("b", now, time.Minute, "dead")

	if got := st.QuarantinedUUIDs(now.Add(2 * time.Minute)); len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("QuarantinedUUIDs = %v, want [a c]", got)
	}
}

func TestQuarantineDefaultsItsLengthRatherThanExpiringImmediately(t *testing.T) {
	st := NewState()
	st.Quarantine("a", now, 0, "dead")

	q, held := st.Quarantined("a", now.Add(DefaultQuarantine-time.Minute))
	if !held {
		t.Fatal("a zero length must default, not expire on the spot")
	}
	if !q.Until.Equal(now.Add(DefaultQuarantine)) {
		t.Errorf("Until = %v, want %v", q.Until, now.Add(DefaultQuarantine))
	}
}

// ---- what may quarantine ---------------------------------------------------

// §7.2 allows exactly one trigger. Firing on a transport failure quarantines
// every account the first time the laptop sleeps; firing on a bad status does
// it the first time Anthropic returns a 503.
func TestOnlyADeadRefreshTokenQuarantines(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want RefreshOutcome
	}{
		{"nil", nil, RefreshOK},
		{"transport", &oauth.TokenError{Kind: oauth.TokenErrorTransport}, RefreshUnreachable},
		{"invalid code", &oauth.TokenError{Kind: oauth.TokenErrorInvalidCode, Status: 401}, RefreshDead},
		{"invalid scope", &oauth.TokenError{Kind: oauth.TokenErrorInvalidScope, Status: 400}, RefreshScopeRefused},
		{"status", &oauth.TokenError{Kind: oauth.TokenErrorStatus, Status: 503}, RefreshUpstream},
		{"not a token error", errors.New("context deadline exceeded"), RefreshUnknown},
	}
	seen := map[RefreshOutcome]string{}
	for _, tc := range cases {
		got := ClassifyRefresh(tc.err)
		if got != tc.want {
			t.Errorf("%s: ClassifyRefresh = %v, want %v", tc.name, got, tc.want)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%s and %s collapse onto %v; the branches must diverge", tc.name, prev, got)
		}
		seen[got] = tc.name

		if quarantines := got.Quarantines(); quarantines != (tc.want == RefreshDead) {
			t.Errorf("%s: Quarantines = %v; only a dead refresh token may quarantine", tc.name, quarantines)
		}
	}
}

func TestClassifyRefreshUnwrapsAWrappedTokenError(t *testing.T) {
	wrapped := fmt.Errorf("refreshing %s: %w", "acct", &oauth.TokenError{Kind: oauth.TokenErrorInvalidCode, Status: 401})
	if got := ClassifyRefresh(wrapped); got != RefreshDead {
		t.Errorf("ClassifyRefresh = %v, want dead-refresh-token through the wrapper", got)
	}
}

func TestRefreshOutcomesAllHaveNames(t *testing.T) {
	for o := RefreshOK; o <= RefreshUnknown; o++ {
		if o.String() == "unknown" {
			t.Errorf("RefreshOutcome(%d) has no name", o)
		}
	}
	if got := RefreshOutcome(200).String(); got != "unknown" {
		t.Errorf("RefreshOutcome(200) = %q, want unknown", got)
	}
}

// A State built as a literal has a nil quarantine map, and the engine must not
// panic on one.
func TestTheZeroStateIsUsable(t *testing.T) {
	st := &State{}
	st.Quarantine("a", now, time.Hour, "dead")

	if _, held := st.Quarantined("a", now); !held {
		t.Error("a zero State refused to record a quarantine")
	}
}

func TestLoadStateReportsAFileItCannotReadAtAll(t *testing.T) {
	root := isolate(t)
	// A directory where the document should be is unreadable in a way that is
	// not os.ErrNotExist, which is the branch a missing file does not reach.
	if err := os.Mkdir(filepath.Join(root, "strategy.json"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	st, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState returned a hard error: %v", err)
	}
	if st.LoadError() == nil {
		t.Error("LoadError = nil; an unreadable file is not an empty one")
	}
}

// A TokenErrorKind this build does not know is not evidence about the token,
// and above all is not a reason to quarantine.
func TestAnUnrecognizedTokenErrorKindDoesNotQuarantine(t *testing.T) {
	got := ClassifyRefresh(&oauth.TokenError{Kind: oauth.TokenErrorKind(99)})
	if got != RefreshUnknown {
		t.Errorf("ClassifyRefresh = %v, want unknown-error", got)
	}
	if got.Quarantines() {
		t.Error("an unrecognized kind quarantined the account")
	}
}

// The held list is reported to a user and must not depend on the order the
// accounts happened to arrive in.
func TestTheQuarantinedListIsOrdered(t *testing.T) {
	st := NewState()
	st.Quarantine("c", now, time.Hour, "dead")
	st.Quarantine("a", now, time.Hour, "dead")
	cands := []Candidate{hr("c", 50, time.Hour), hr("a", 40, time.Hour), hr("b", 30, time.Hour)}

	p := Decide(cands, opts(), Config{}, st, "b")
	if len(p.Quarantined) != 2 || p.Quarantined[0] != "a" || p.Quarantined[1] != "c" {
		t.Errorf("Quarantined = %v, want [a c]", p.Quarantined)
	}
}

// Two accounts a second apart either side of the horizon are a tier apart and
// two seconds better. Reading the tier difference as categorical would wave
// that move through, which is precisely the ping-pong the 300 s margin exists
// to stop.
func TestTheRecoveryMarginAppliesAcrossTheHorizonBoundaryToo(t *testing.T) {
	cands := []Candidate{hr("a", 5, DefaultRecoveryHorizon+time.Second), hr("b", 5, DefaultRecoveryHorizon-time.Second)}

	p := Decide(cands, opts(), Config{}, NewState(), "a")
	if p.Result.Mode != ModeRecovery || !p.Result.Order[0].ReturnsInsideHorizon {
		t.Fatalf("setup: mode %v, order %+v", p.Result.Mode, p.Result.Order)
	}
	want(t, p, ActionStay, ReasonRecoveryHysteresis, "")
}

// A candidate that names a return time is a better answer than an active
// account that never said, and there is no instant to hold a margin against.
func TestARecoveryMarginNeedsAnInstantOnBothSides(t *testing.T) {
	cands := []Candidate{hrNoReset("a", 5), hr("b", 5, 30*time.Minute)}

	p := Decide(cands, opts(), Config{}, NewState(), "a")
	if p.Result.Mode != ModeRecovery {
		t.Fatalf("Mode = %v, want recovery", p.Result.Mode)
	}
	want(t, p, ActionSwitch, ReasonBetterTarget, "b")
}

// The cooldown has to hold when the engine is ON a credit account too:
// churning between the pool that costs money and the one that does not is the
// most expensive flap there is.
func TestTheCooldownHoldsAnEngineSittingOnCredit(t *testing.T) {
	st := NewState()
	st.RecordSwitch("c", now)
	cands := []Candidate{hr("a", 50, time.Hour), creditWith("c", enabledExtra(nil, f(0)))}

	want(t, Decide(cands, at(time.Minute), Config{MaxAutoSpend: 100}, st, "c"), ActionStay, ReasonCooldown, "")
}

// "" is Decide's word for "the live login could not be attributed to a managed
// account". It must never match an account, or a nameless candidate becomes a
// baseline conjured out of nothing — and the cooldown starts holding an engine
// that has no login underneath it.
func TestAnUnattributableLoginMatchesNoAccount(t *testing.T) {
	st := NewState()
	st.RecordSwitch("b", now)
	cands := []Candidate{sub("", &usage.Snapshot{FiveHour: win(50, time.Hour)}), hr("b", 20, time.Hour)}

	want(t, Decide(cands, at(time.Minute), Config{}, st, ""), ActionSwitch, ReasonBetterTarget, "")
}

// A quarantined account's quota is not spendable, so it cannot be what keeps
// §7.3's credit pool closed. This is consistent with the rest of the pass:
// quarantine removes an account from the world. It does not weaken "fail closed
// on money" either — that rule is about figures nobody could READ, and a
// quarantine is a definite failure rather than an unknown.
func TestAQuarantinedAccountDoesNotHoldTheCreditPoolClosed(t *testing.T) {
	st := NewState()
	st.Quarantine("q", now, time.Hour, "dead refresh token")
	cands := []Candidate{
		hr("a", 5, 4*time.Hour),
		hr("q", 90, 4*time.Hour),
		creditWith("c", enabledExtra(nil, f(0))),
	}

	p := Decide(cands, opts(), Config{MaxAutoSpend: 100}, st, "a")
	if !p.SubscriptionExhausted {
		t.Error("SubscriptionExhausted = false; the only account with room is one nothing can use")
	}
	want(t, p, ActionSwitch, ReasonBetterTarget, "c")
}

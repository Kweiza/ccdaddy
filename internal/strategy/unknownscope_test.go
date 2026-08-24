package strategy

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// unknownScoped builds a weekly cap filed under a scope key this build does not
// name — the shape Claude Code's own passthrough schema allows and ccdad has no
// name for.
func unknownScoped(scope, display string, pct float64, resetsIn time.Duration) usage.Limit {
	at := now.Add(resetsIn)
	return usage.LimitFor(usage.LimitInput{
		Kind:        "weekly_scoped",
		Group:       scope,
		OtherScopes: map[string]string{scope: display},
		Percent:     &pct,
		ResetsAt:    &at,
	})
}

// A cap under a scope ccdad cannot name is real quota, and ccdad still cannot
// say what it caps — so it does not bind on its own, exactly as an entry naming
// no scope at all does not. Binding on it silently would have `ccdad status`
// report a window whose meaning nobody can state.
func TestAnUnknownScopeWindowDoesNotBindOnItsOwn(t *testing.T) {
	s := &usage.Snapshot{
		FiveHour: win(10, time.Hour),
		Limits:   []usage.Limit{unknownScoped("region", "eu", 95, 48*time.Hour)},
	}

	h := HeadroomOf(s, thr())
	if b := bindingOf(t, h); b != "five_hour" {
		t.Errorf("Binding = %q, want five_hour — a cap this build cannot describe must not decide the ranking by itself", b)
	}
	if h.Pct != 90 {
		t.Errorf("Pct = %v, want 90 — the unnamed scope must not be counted", h.Pct)
	}
}

// Setting a threshold on the name IS the opt-in: it is the user saying they know
// what the scope means. Nothing else about the window changes — it was carried
// and readable all along.
func TestAThresholdOnItsNamePutsAnUnknownScopeWindowIntoTheRanking(t *testing.T) {
	s := &usage.Snapshot{
		FiveHour: win(10, time.Hour),
		Limits:   []usage.Limit{unknownScoped("region", "eu", 95, 48*time.Hour)},
	}

	// 50 rather than 80: DefaultThreshold IS 80, so a Thresholds.For that
	// ignored PerWindow entirely would produce the same Threshold and the same
	// Slack, and the assertions could not tell the two implementations apart.
	h := HeadroomFor(s, "", perWindow(map[usage.WindowName]float64{
		"weekly_scoped:region:eu": 50,
	}).Thresholds())

	if b := bindingOf(t, h); b != "weekly_scoped:region:eu" {
		t.Fatalf("Binding = %q, want weekly_scoped:region:eu — a threshold on the name is the opt-in", b)
	}
	if h.Pct != 5 {
		t.Errorf("Pct = %v, want 5", h.Pct)
	}
	if h.Threshold != 50 {
		t.Errorf("Threshold = %v, want 50 — the window is measured against the threshold that opted it in, not against the default", h.Threshold)
	}
	if h.Slack != -45 {
		t.Errorf("Slack = %v, want -45 (50 - 95)", h.Slack)
	}
}

// A threshold of zero is not consent. Thresholds.For treats a non-positive entry
// as the same omission, so a gate that read it as consent would admit a window
// and then measure it against a threshold nobody set for it.
func TestANonPositiveThresholdIsNotAnOptIn(t *testing.T) {
	s := &usage.Snapshot{
		FiveHour: win(10, time.Hour),
		Limits:   []usage.Limit{unknownScoped("region", "eu", 95, 48*time.Hour)},
	}

	for _, v := range []float64{0, -1} {
		h := HeadroomFor(s, "", perWindow(map[usage.WindowName]float64{
			"weekly_scoped:region:eu": v,
		}).Thresholds())
		if b := bindingOf(t, h); b != "five_hour" {
			t.Errorf("with a threshold of %v, Binding = %q, want five_hour", v, b)
		}
	}
}

// Naming a model NARROWS, and only ever narrows. An unnamed scope has no model
// half to compare a family against, so there is nothing to narrow it BY — and
// dropping it because the family is unreadable would undo the opt-in the user
// just gave, which is the same rule a window of an unrecognized family already
// gets.
func TestNamingAModelDoesNotWithdrawAnUnknownScopeOptIn(t *testing.T) {
	s := &usage.Snapshot{
		FiveHour: win(10, time.Hour),
		Limits:   []usage.Limit{unknownScoped("region", "eu", 95, 48*time.Hour)},
	}
	table := perWindow(map[usage.WindowName]float64{"weekly_scoped:region:eu": 50}).Thresholds()

	for _, model := range []string{"", "claude-opus-5", "claude-sonnet-5"} {
		h := HeadroomFor(s, model, table)
		if b := bindingOf(t, h); b != "weekly_scoped:region:eu" {
			t.Errorf("HeadroomFor(model=%q) binding = %q, want weekly_scoped:region:eu — --model can only raise headroom, never withdraw an opt-in", model, b)
		}
	}
}

// The signature change exists so that the pass measuring headroom and the pass
// picking a weekly reset see ONE window set. Only measure runs both in
// production, so this is the link that has to be pinned: calling weeklyResetOf
// directly proves the function and not the wiring.
func TestTheRankingPassHandsItsOwnThresholdsToTheWeeklyResetPass(t *testing.T) {
	// b's opted-in weekly cap expires first, so consume-first spends it before
	// a's — but only if the table reaches weeklyResetOf. Without it b's only
	// weekly reading is seven_day at 96h and a wins.
	a := sub("a", &usage.Snapshot{
		FiveHour: win(10, time.Hour),
		SevenDay: win(20, 72*time.Hour),
	})
	b := sub("b", &usage.Snapshot{
		FiveHour: win(10, time.Hour),
		SevenDay: win(20, 96*time.Hour),
		Limits:   []usage.Limit{unknownScoped("region", "eu", 30, 24*time.Hour)},
	})

	o := perWindow(map[usage.WindowName]float64{"weekly_scoped:region:eu": 50})
	o.Strategy = StrategyConsumeFirst
	eq(t, order(Rank([]Candidate{a, b}, o)), []string{"b", "a"})

	// Without the opt-in the region cap is invisible to both passes, so a's
	// earlier seven_day reset wins. This is the case that proves the ordering
	// above came from the table rather than from the fixture.
	bare := opts()
	bare.Strategy = StrategyConsumeFirst
	eq(t, order(Rank([]Candidate{a, b}, bare)), []string{"a", "b"})
}

// A threshold on a DIFFERENT window is not consent for this one. Without this,
// any per-window table at all would switch every unnamed scope on at once.
func TestAThresholdOnAnotherWindowDoesNotOptInAnUnknownScope(t *testing.T) {
	s := &usage.Snapshot{
		FiveHour: win(10, time.Hour),
		Limits:   []usage.Limit{unknownScoped("region", "eu", 95, 48*time.Hour)},
	}

	h := HeadroomFor(s, "", perWindow(map[usage.WindowName]float64{
		"five_hour": 90,
	}).Thresholds())

	if b := bindingOf(t, h); b != "five_hour" {
		t.Errorf("Binding = %q, want five_hour — consent is per window, not per table", b)
	}
}

// The opted-in window is weekly, so consume-first spends against its reset like
// any other weekly cap. bindingWindows is one function for exactly this reason:
// a window admitted to the headroom pass and narrowed out of the reset pass
// would have the engine measure one account two ways.
func TestAnOptedInUnknownScopeWindowIsAWeeklyResetLikeAnyOther(t *testing.T) {
	s := &usage.Snapshot{
		FiveHour: win(10, time.Hour),
		SevenDay: win(20, 96*time.Hour),
		Limits:   []usage.Limit{unknownScoped("region", "eu", 30, 48*time.Hour)},
	}

	o := perWindow(map[usage.WindowName]float64{"weekly_scoped:region:eu": 80})
	got := weeklyResetOf(s, o.Model, o.Thresholds())
	if !got.ok {
		t.Fatal("weeklyResetOf() reported no reset; the opted-in window carries one")
	}
	if want := now.Add(48 * time.Hour); !got.at.Equal(want) {
		t.Errorf("weeklyResetOf() = %v, want %v — the opted-in weekly cap resets first", got.at, want)
	}

	if bare := weeklyResetOf(s, opts().Model, thr()); !bare.ok || !bare.at.Equal(now.Add(96*time.Hour)) {
		t.Errorf("weeklyResetOf() without the opt-in = %v, want the seven_day reset — an unnamed scope must not reach this pass either", bare.at)
	}
}

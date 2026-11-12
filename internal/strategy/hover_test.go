package strategy

import (
	"fmt"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// remaining is how long is left of a window that is `share` of the way through
// one of `length`. It is the one place the share is turned into a reset instant,
// so a case below states its input in the same terms a reader would.
func remaining(length time.Duration, share float64) time.Duration {
	return length - time.Duration(share*float64(length))
}

// elapsedWindow builds a window at pct utilization that is `share` of the way
// through a window of `length`.
func elapsedWindow(length time.Duration, share, pct float64) usage.Window {
	return win(pct, remaining(length, share))
}

// oneWindow is a snapshot carrying exactly the named window, so a case that is
// about five_hour cannot be answered by seven_day sitting beside it.
func oneWindow(t *testing.T, name usage.WindowName, w usage.Window) *usage.Snapshot {
	t.Helper()
	switch name {
	case usage.WindowFiveHour:
		return &usage.Snapshot{FiveHour: w}
	case usage.WindowSevenDay:
		return &usage.Snapshot{SevenDay: w}
	}
	t.Fatalf("no fixture for window %q", name)
	return nil
}

// hoverPool is n identical accounts, each carrying one window.
func hoverPool(t *testing.T, n int, name usage.WindowName, w usage.Window) []Candidate {
	t.Helper()
	out := make([]Candidate, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, sub(fmt.Sprintf("u-%d", i), oneWindow(t, name, w)))
	}
	return out
}

// Every window is held to the hand-off line, whatever the pool and whatever the
// clock. The pace rotation this replaced derived a different figure for every
// one of these cases -- 68, 143 and 130 -- and the sweep over pool size and
// elapsed share is what says nothing of that survived.
func TestHoverHoldsEveryWindowToTheHandoffLine(t *testing.T) {
	week, five := 7*24*time.Hour, 5*time.Hour
	for _, tc := range []struct {
		name     string
		window   usage.WindowName
		length   time.Duration
		share    float64
		accounts int
	}{
		{"four accounts, a weekly window 43% elapsed", usage.WindowSevenDay, week, 0.43, 4},
		{"one account left", usage.WindowSevenDay, week, 0.43, 1},
		{"a five-hour window 80% elapsed with two accounts", usage.WindowFiveHour, five, 0.80, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := hoverPool(t, tc.accounts, tc.window, elapsedWindow(tc.length, tc.share, 10))
			p := HoverThresholds(pool, opts())
			if got := p.For(pool[0].UUID).For(tc.window); got != HoverHandoffThreshold {
				t.Errorf("threshold for %s = %v, want the hand-off line %v", tc.window, got, HoverHandoffThreshold)
			}
			if got := p.For(pool[0].UUID).PerWindow[tc.window]; got != HoverHandoffThreshold {
				t.Errorf("the table's own entry for %s = %v, want the hand-off line %v", tc.window, got, HoverHandoffThreshold)
			}
		})
	}
}

// Every door that answers "hover built nothing for this" is the same line, and
// it is strictly positive and short of 100.
//
// Positive is load-bearing rather than tidy: admittedWindows reads a non-positive
// per-window entry as "not opted in", so a zero here would silently revoke a
// user's consent for an unknown-scope cap. Short of 100 is the hand-off itself:
// a line at 100 is crossed only once the session has stopped.
func TestEveryHoverDefaultIsThePositiveHandoffLine(t *testing.T) {
	if HoverHandoffThreshold <= 0 || HoverHandoffThreshold >= 100 {
		t.Fatalf("HoverHandoffThreshold = %v, want strictly between 0 and 100", HoverHandoffThreshold)
	}
	var none HoverPlan
	if got := none.For("nobody").Default; got != HoverHandoffThreshold {
		t.Errorf("zero plan threshold = %v, want %v", got, HoverHandoffThreshold)
	}
	pool := hoverPool(t, 2, usage.WindowSevenDay, elapsedWindow(7*24*time.Hour, 0.43, 10))
	if got := HoverThresholds(pool, opts()).For("u-0").Default; got != HoverHandoffThreshold {
		t.Errorf("Default = %v, want %v", got, HoverHandoffThreshold)
	}
}

// A primary credit seat has no window and no reset, so it gets a fixed figure of
// its own. It is short of the hand-off line because credits do not come back.
func TestHoverGivesAPrimaryCreditSeatAFixedThreshold(t *testing.T) {
	seat := creditWith("c-1", usage.ExtraUsageFor(usage.ExtraUsageInput{
		State:        usage.ExtraUsageEnabled,
		Currency:     "USD",
		MonthlyLimit: pf(10000),
		UsedCredits:  pf(0),
		Utilization:  pf(41),
	}))
	seat.Primary = true

	p := HoverThresholds([]Candidate{seat}, opts())
	if p.CreditThreshold != HoverCreditThreshold {
		t.Errorf("CreditThreshold = %v, want %v", p.CreditThreshold, HoverCreditThreshold)
	}
	if p.For("c-1").CreditThreshold() != HoverCreditThreshold {
		t.Errorf("For(c-1).CreditThreshold() = %v, want %v",
			p.For("c-1").CreditThreshold(), HoverCreditThreshold)
	}
}

// An account nobody could read is not in the pass hover built its tables from,
// and still gets an answer if it is ever asked about: the same line as
// everything else.
func TestAnAccountHoverCouldNotReadIsHeldToTheSameLine(t *testing.T) {
	pool := hoverPool(t, 2, usage.WindowSevenDay, elapsedWindow(7*24*time.Hour, 0.43, 10))
	pool = append(pool, Candidate{UUID: "u-unread", Kind: pool[0].Kind})

	p := HoverThresholds(pool, opts())
	if got := p.For("u-unread").For(usage.WindowSevenDay); got != HoverHandoffThreshold {
		t.Errorf("threshold for the unread account = %v, want %v", got, HoverHandoffThreshold)
	}
}

// A weekly cap filed under a scope key this build cannot name binds only where
// the user has set a threshold on its name, and that opt-in survives hover.
//
// The two rules meet here and they are not the same rule. The VALUE in the table
// is a tuning number, which hover overrides like every other one. The PRESENCE
// of an entry is consent -- the user saying they know what the scope means --
// and consent is not a knob a mode may supply on the user's behalf. So hover
// reads the configured table to decide what may bind, and holds it to its own
// line.
func TestHoverHonoursTheUnknownScopeOptInAndStillDerivesItsValue(t *testing.T) {
	const name usage.WindowName = "weekly_scoped:region:eu"
	week := 7 * 24 * time.Hour
	snap := func() *usage.Snapshot {
		return &usage.Snapshot{
			FiveHour: win(10, time.Hour),
			Limits:   []usage.Limit{unknownScoped("region", "eu", 95, remaining(week, 0.43))},
		}
	}
	pool := []Candidate{sub("u-0", snap()), sub("u-1", snap())}

	// No entry in the configured table: hover's table has none either, and the
	// window does not bind.
	bare := HoverThresholds(pool, opts()).For("u-0")
	if _, ok := bare.PerWindow[name]; ok {
		t.Error("hover's table names a scope nobody opted in to; the mode must not grant consent")
	}
	if h := HeadroomFor(pool[0].Usage, "", bare); h.Binding == name {
		t.Errorf("Binding = %q with no opt-in", h.Binding)
	}

	// 50 rather than 80: DefaultThreshold IS 80, so a hover that simply copied
	// the configured number through would be indistinguishable from one that
	// ignored it.
	o := perWindow(map[usage.WindowName]float64{name: 50})
	p := HoverThresholds(pool, o)

	if _, ok := p.For("u-0").PerWindow[name]; !ok {
		t.Fatalf("PerWindow = %v; a threshold on the name is the opt-in and hover has to honour it", p.For("u-0").PerWindow)
	}
	if got := p.For("u-0").For(name); got != HoverHandoffThreshold {
		t.Errorf("threshold = %v, want %v; the configured 50 is a number hover replaces", got, HoverHandoffThreshold)
	}

	// The set hover built for and the set the ranking measures have to be the
	// same set, or a window admitted to one is narrowed out of the other.
	h := HeadroomFor(pool[0].Usage, "", p.For("u-0"))
	if h.Binding != name {
		t.Errorf("Binding = %q, want %q -- the hover table has to re-admit the window the user opted in to", h.Binding, name)
	}
	if h.Slack != HoverHandoffThreshold-95 {
		t.Errorf("Slack = %v, want %v", h.Slack, HoverHandoffThreshold-95)
	}
}

// The keys hover ignores stop mattering, and the ONE key it does not ignore
// still does. Two accounts identical but for the primary flag, under a ceiling
// of 0 -- which is the default, and one of the two independent opt-ins
// unattended overage requires.
func TestHoverIgnoresTheTuningAndNotTheMoneyGate(t *testing.T) {
	// Past the line the user typed, which is the number the money question is
	// asked of: 95% used under a threshold of 90.
	spent := sub("a", &usage.Snapshot{
		SevenDay: elapsedWindow(7*24*time.Hour, 0.43, 95),
	})
	seat := creditWith("c", usage.ExtraUsageFor(usage.ExtraUsageInput{
		State:        usage.ExtraUsageEnabled,
		Currency:     "USD",
		MonthlyLimit: pf(10000),
		UsedCredits:  pf(0),
		Utilization:  pf(41),
	}))

	o := opts()
	o.Hover = true
	// Deliberately hostile ANTI-FLAP tuning: a margin nothing could clear and a
	// cooldown longer than the pass. The cooldown is a key hover overrides.
	//
	// The threshold is NOT hostile here. It is the one number that still
	// governs the money question -- see the case below -- so leaving it at 99
	// would stop this pass before it ever reached the gate, and the gate is
	// what this case is about.
	o.Threshold = 90
	tuned := Config{HysteresisPct: 90, HeadroomRatio: 5, Cooldown: time.Hour}

	// max_auto_spend is 0, and the seat is not primary. Money is consulted only
	// once every free point is gone -- the live account out, and nothing in
	// the subscription pool to take -- so with room left the gate is not asked
	// at all, and with the account empty it refuses.
	p := Decide([]Candidate{spent, seat}, o, tuned, NewState(), "a")
	want(t, p, ActionStay, ReasonAlreadyBest, "")
	if p.CreditConsulted {
		t.Error("the credit pool was consulted while the live account held free quota")
	}
	empty := sub("a", &usage.Snapshot{SevenDay: elapsedWindow(7*24*time.Hour, 0.43, 100)})
	p = Decide([]Candidate{empty, seat}, o, tuned, NewState(), "a")
	want(t, p, ActionBlocked, ReasonCreditGate, "")
	if p.Credit.Reason != CreditNotOptedIn {
		t.Errorf("Credit.Reason = %v, want %v: hover must not switch on unattended spending",
			p.Credit.Reason, CreditNotOptedIn)
	}

	// The same seat, marked primary by a human typing the command. Now it is
	// ranked in the main pool on its own axis and the money gate never applies:
	// both are weekly-only, and the account whose week ends in four days leads
	// the seat, whose credits do not expire at all.
	seat.Primary = true
	p = Decide([]Candidate{spent, seat}, o, tuned, NewState(), "a")
	want(t, p, ActionStay, ReasonAlreadyBest, "")
	if p.CreditConsulted {
		t.Error("the credit gate was consulted for a primary seat")
	}

	// And from the seat the session goes back to the free week: credits are
	// spent only after free quota (creditSeatReturn). The hour-long cooldown
	// does not hold it, because nothing has switched.
	p = Decide([]Candidate{spent, seat}, o, tuned, NewState(), "c")
	want(t, p, ActionSwitch, ReasonBetterTarget, "a")
	if p.CreditConsulted {
		t.Error("the credit gate was consulted for a primary seat")
	}
}

// Hover's line may not open the paid pool.
//
// The account below is past the hand-off line hover holds it to and short of the
// number the user actually chose. Reading hover's figure here would start buying
// credits while a point of paid-for subscription quota sat unspent -- and would
// do it on a number the user never saw, which is exactly the consent
// HoverThresholds already refuses to invent for a scoped weekly cap.
func TestHoverThresholdDoesNotOpenTheCreditPool(t *testing.T) {
	spent := sub("a", &usage.Snapshot{
		SevenDay: elapsedWindow(7*24*time.Hour, 0.43, 99.5),
	})
	seat := creditWith("c", usage.ExtraUsageFor(usage.ExtraUsageInput{
		State:        usage.ExtraUsageEnabled,
		Currency:     "USD",
		MonthlyLimit: pf(10000),
		UsedCredits:  pf(0),
		Utilization:  pf(41),
	}))

	o := opts()
	o.Hover = true
	o.Threshold = 100

	p := Decide([]Candidate{spent, seat}, o, Config{MaxAutoSpend: 500}, NewState(), "a")
	if p.CreditConsulted {
		t.Errorf("the credit pool was consulted: hover's hand-off line must not authorise a purchase")
	}
	// On its last point with nowhere to go, which is blocked -- and still no
	// purchase on a line the user did not choose.
	want(t, p, ActionBlocked, ReasonNothingCanServe, "")
}

// Under hover money comes after every free point of subscription quota, and
// the same state gives the same answer whichever account the order puts first.
// The user's thresholds say the main pool is exhausted and the seat is opted
// in, so the gate would sell: while the live account still holds its last
// point, and while another account holds one, it is not asked.
func TestCreditsComeAfterEveryFreePointUnderHover(t *testing.T) {
	seat := creditWith("c", usage.ExtraUsageFor(usage.ExtraUsageInput{
		State:        usage.ExtraUsageEnabled,
		Currency:     "USD",
		MonthlyLimit: pf(10000),
		UsedCredits:  pf(0),
		Utilization:  pf(41),
	}))
	o := opts()
	o.Hover = true
	o.Threshold = 90
	cfg := Config{MaxAutoSpend: 500}
	for _, tc := range []struct {
		name  string
		bIn   time.Duration
		first string
	}{
		{"the live account sorting first", 5 * day, "a"},
		{"the other account sorting first", 2 * day, "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := func(pct float64) Candidate { return sub("a", &usage.Snapshot{SevenDay: win(pct, 3*day)}) }
			b := func(pct float64) Candidate { return sub("b", &usage.Snapshot{SevenDay: win(pct, tc.bIn)}) }

			p := Decide([]Candidate{a(99.5), b(99.5), seat}, o, cfg, NewState(), "a")
			if first := p.Result.Order[0].UUID; first != tc.first {
				t.Fatalf("the order puts %s first, want %s", first, tc.first)
			}
			want(t, p, ActionBlocked, ReasonNothingCanServe, "")
			if p.CreditConsulted {
				t.Error("the credit pool was consulted while the live account held its last point")
			}
			p = Decide([]Candidate{a(100), b(99.5), seat}, o, cfg, NewState(), "a")
			want(t, p, ActionSwitch, ReasonBetterTarget, "b")
			if p.CreditConsulted {
				t.Error("the credit pool was consulted while another account held its last point")
			}
			p = Decide([]Candidate{a(100), b(100), seat}, o, cfg, NewState(), "a")
			want(t, p, ActionSwitch, ReasonBetterTarget, "c")
		})
	}
}

// The plan hover ran on is reported, and nil is what says the mode was not in
// force.
func TestDecideReportsThePlanHoverRanOn(t *testing.T) {
	pool := hoverPool(t, 2, usage.WindowSevenDay, elapsedWindow(7*24*time.Hour, 0.43, 10))
	o := opts()
	o.Hover = true

	p := Decide(pool, o, Config{}, NewState(), "u-0")
	if p.Hover == nil {
		t.Fatal("Plan.Hover is nil with hover on; `ccdad status` has no thresholds to print")
	}
	if got := p.Hover.For("u-1").For(usage.WindowSevenDay); got != HoverHandoffThreshold {
		t.Errorf("Plan.Hover holds u-1 to %v, want %v", got, HoverHandoffThreshold)
	}
	if off := Decide(pool, opts(), Config{}, NewState(), "u-0"); off.Hover != nil {
		t.Error("Plan.Hover is set with hover off; nil is what says the mode was not in force")
	}
}

// Hover imposes the cooldown and nothing else. hysteresis_pct, headroom_ratio
// and recovery_hysteresis pass through untouched -- no hover move reads them --
// and the spending ceiling is never a mode's to set.
func TestHoverConfigReplacesTheCooldownAndNothingElse(t *testing.T) {
	tuned := Config{
		HysteresisPct:      40,
		HeadroomRatio:      3,
		Cooldown:           time.Hour,
		RecoveryHysteresis: time.Hour,
		MaxAutoSpend:       25,
	}
	got := HoverConfig(tuned)

	if got.Cooldown != HoverCooldown {
		t.Errorf("Cooldown = %v, want %v", got.Cooldown, HoverCooldown)
	}
	want := tuned
	want.Cooldown = HoverCooldown
	if got != want {
		t.Errorf("HoverConfig(%+v) = %+v; every key but the cooldown must be left exactly as the file set it", tuned, got)
	}
}

// The credit line and the order are ranking inputs rather than anti-flap knobs,
// so hover puts them on the pass and not on the anti-flap set, where `ccdad
// config list` reports what hover overrode.
func TestHoverPutsItsCreditLineAndOrderOnTheRankingPass(t *testing.T) {
	c := sub("u-1", oneWindow(t, usage.WindowFiveHour, elapsedWindow(5*time.Hour, 0.5, 10)))
	c.FetchedAt, c.NextPollAt = now, now.Add(300*time.Second)
	o := opts()
	o.Hover = true

	got := o.withHover([]Candidate{c})
	if got.CreditThreshold != HoverCreditThreshold {
		t.Errorf("CreditThreshold = %v, want %v", got.CreditThreshold, HoverCreditThreshold)
	}
	if got.Strategy != StrategyHeadroom {
		t.Errorf("Strategy = %v, want %v", got.Strategy, StrategyHeadroom)
	}
}

// consume-first is one of the keys hover overrides: the mode ranks in its own
// order, not on the reset instant alone. The soonest week still leads, because
// lessHover compares it inside the rung -- the two accounts here differ only in
// when their week ends.
func TestHoverOverridesTheConfiguredStrategy(t *testing.T) {
	week := 7 * 24 * time.Hour
	acct := func(uuid string, weekElapsed float64) Candidate {
		return sub(uuid, &usage.Snapshot{
			FiveHour: elapsedWindow(5*time.Hour, 0.20, 10),
			SevenDay: elapsedWindow(week, weekElapsed, 5),
		})
	}
	pool := []Candidate{acct("u-later", 0.43), acct("u-sooner", 0.93)}
	o := opts()
	o.Hover, o.Strategy = true, StrategyConsumeFirst

	res := Rank(pool, o)
	if res.Mode != ModeHover {
		t.Errorf("Mode = %v, want %v: hover's order replaces the configured strategy", res.Mode, ModeHover)
	}
	eq(t, order(res), []string{"u-sooner", "u-later"})
}

// Decide installs the pass over the pool quarantine has already filtered, and
// Rank must not derive a second one on the way past. A second pass would take
// its lead from whatever pool that caller happened to hold, which is how the
// anti-flap set comes to be derived from accounts the ranking never ranked.
func TestASecondHoverPassIsNeverDerived(t *testing.T) {
	two := hoverPool(t, 2, usage.WindowSevenDay, elapsedWindow(7*24*time.Hour, 0.43, 10))
	o := opts()
	o.Hover = true

	installed := o.withHover(two)
	if installed.hover == nil {
		t.Fatal("withHover installed nothing with hover on")
	}
	if again := installed.withHover(two[:1]); again.hover != installed.hover {
		t.Error("the second call built a second pass over a different pool")
	}
}

// The two window sets `measure` reads have to come from ONE table. HeadroomFor
// picks the binding window and the weekly floor in a single pass for exactly
// this reason; weeklyResetOf is the third set, and it is built by a separate
// call that has to be handed the same table or a window is admitted to one and
// narrowed out of the other.
//
// The case that tells the two apart: a weekly cap under a scope this build
// cannot name, opted in by a threshold on its name, that reported a reset and no
// utilization. The configured table admits it; hover holds no window with no
// utilization to a line, so hover's table does not -- and a mixed pair would
// have the ranking measure a set the weekly reset was not taken over.
func TestTheWeeklyResetIsReadFromTheSameTableTheHeadroomWas(t *testing.T) {
	const name usage.WindowName = "weekly_scoped:region:eu"
	at := now.Add(48 * time.Hour)
	noPct := usage.LimitFor(usage.LimitInput{
		Kind:        "weekly_scoped",
		Group:       "region",
		OtherScopes: map[string]string{"region": "eu"},
		ResetsAt:    &at,
	})
	pool := []Candidate{sub("u-0", &usage.Snapshot{
		FiveHour: win(10, time.Hour),
		Limits:   []usage.Limit{noPct},
	})}

	o := perWindow(map[usage.WindowName]float64{name: 50})

	// With hover off the opt-in stands and the reset is the account's weekly one.
	if r := Rank(pool, o).Order; len(r) != 1 || !r[0].HasWeeklyReset {
		t.Fatalf("Ranked = %+v; with hover off the opted-in cap is in both sets", r)
	}

	// With hover on it is in neither: no utilization means no row, and a window
	// hover holds to nothing is not one the ranking measures.
	o.Hover = true
	r := Rank(pool, o).Order
	if len(r) != 1 {
		t.Fatalf("Ranked = %+v, want one account", r)
	}
	if r[0].Headroom.Binding != usage.WindowFiveHour {
		t.Fatalf("Binding = %q, want five_hour", r[0].Headroom.Binding)
	}
	if r[0].HasWeeklyReset {
		t.Errorf("HasWeeklyReset = true at %v: the weekly reset came from a window the headroom pass never admitted",
			r[0].WeeklyResetsAt)
	}
}

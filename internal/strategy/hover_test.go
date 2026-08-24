package strategy

import (
	"fmt"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// remaining is how long is left of a window that is `share` of the way through
// one of `length`. It is the one place the share is turned into a reset instant,
// so a case below states its input in the same terms the formula reads.
func remaining(length time.Duration, share float64) time.Duration {
	return length - time.Duration(share*float64(length))
}

// elapsedWindow builds a window at pct utilization that is `share` of the way
// through a window of `length`. The share is exactly what hover reads as the
// elapsed percent, so a case here states its input in the same terms the
// formula does.
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

// hoverPool is n identical accounts, each carrying one window. Identical is the
// point: the count of usable accounts is the only thing that varies between the
// worked examples below.
func hoverPool(t *testing.T, n int, name usage.WindowName, w usage.Window) []Candidate {
	t.Helper()
	out := make([]Candidate, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, sub(fmt.Sprintf("u-%d", i), oneWindow(t, name, w)))
	}
	return out
}

// rowFor is the derived row for one account's one window, or nothing when hover
// derived none. A missing row and a row with a fallback threshold are different
// answers, and a test that could not tell them apart would pass on either.
func rowFor(p HoverPlan, uuid string, name usage.WindowName) (HoverWindow, bool) {
	for _, row := range p.Windows {
		if row.UUID == uuid && row.Window == name {
			return row, true
		}
	}
	return HoverWindow{}, false
}

// The three situations the design was approved on, worked through end to end. A
// threshold is the share of the window that has elapsed plus this account's
// slice of what is left, capped at 99.
func TestHoverDerivesEveryThresholdFromTheClockAndThePool(t *testing.T) {
	week, five := 7*24*time.Hour, 5*time.Hour

	for _, tc := range []struct {
		name     string
		window   usage.WindowName
		length   time.Duration
		share    float64
		accounts int
		want     float64
	}{{
		// Running ahead of pace hands work on: at 43% of the week gone, an
		// account may hold 68% before a peer is the better place for the next
		// session.
		name:     "four accounts, a weekly window 43% elapsed",
		window:   usage.WindowSevenDay,
		length:   week,
		share:    0.43,
		accounts: 4,
		want:     68,
	}, {
		// Nobody to hand to. Spend everything that is left.
		name:     "one account left",
		window:   usage.WindowSevenDay,
		length:   week,
		share:    0.43,
		accounts: 1,
		want:     99,
	}, {
		// It resets within the hour, so holding quota back buys nothing.
		name:     "a five-hour window 80% elapsed with two accounts",
		window:   usage.WindowFiveHour,
		length:   five,
		share:    0.80,
		accounts: 2,
		want:     99,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			pool := hoverPool(t, tc.accounts, tc.window, elapsedWindow(tc.length, tc.share, 10))

			p := HoverThresholds(pool, opts())
			if p.Usable != tc.accounts {
				t.Fatalf("Usable = %d, want %d", p.Usable, tc.accounts)
			}
			if got := p.For(pool[0].UUID).For(tc.window); got != tc.want {
				t.Errorf("threshold for %s = %v, want %v", tc.window, got, tc.want)
			}
		})
	}
}

// A window that reported no reset has no elapsed share, so there is no pace to
// derive from. It falls back to a fixed figure and is marked as the row a probe
// would fix, which is the only thing that can put a reset on the window at all.
func TestHoverFallsBackAndMarksAWindowWithNoReset(t *testing.T) {
	pct := 30.0
	pool := []Candidate{sub("u-1", &usage.Snapshot{SevenDay: usage.NewWindow(&pct, nil)})}

	p := HoverThresholds(pool, opts())
	if got := p.For("u-1").For(usage.WindowSevenDay); got != HoverFallbackThreshold {
		t.Errorf("threshold = %v, want %v for a window that named no reset", got, HoverFallbackThreshold)
	}
	if len(p.Windows) != 1 {
		t.Fatalf("Windows = %+v, want one row", p.Windows)
	}
	if !p.Windows[0].ProbeWanted || p.Windows[0].HasExpected {
		t.Errorf("row = %+v, want ProbeWanted with no elapsed share", p.Windows[0])
	}
}

// A window with a reset needs no probe, however tight its threshold turns out.
// A probe spends the user's quota, so it is marked only where it is the one
// thing that can answer the question.
func TestHoverMarksNoProbeWhenTheWindowNamedItsReset(t *testing.T) {
	pool := hoverPool(t, 2, usage.WindowFiveHour, elapsedWindow(5*time.Hour, 0.80, 10))

	for _, row := range HoverThresholds(pool, opts()).Windows {
		if row.ProbeWanted {
			t.Errorf("row = %+v is marked for a probe: it named a reset", row)
		}
	}
}

// A primary credit seat has no reset and therefore no pace at all, so it gets a
// figure rather than a derivation. It is high because credits are that seat's
// ordinary metering rather than an overage to be conserved.
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
	if len(p.Windows) != 1 {
		t.Fatalf("Windows = %+v; `ccdad hover status` has one row to print for this seat", p.Windows)
	}
	row := p.Windows[0]
	if !row.Credit || row.Window != creditWindow {
		t.Errorf("row = %+v, want the credit row", row)
	}
	if row.Threshold != HoverCreditThreshold || row.Utilization != 41 || row.Slack != 54 {
		t.Errorf("credit row = %+v, want threshold %v against 41%% used", row, HoverCreditThreshold)
	}
	if row.HasExpected {
		t.Error("HasExpected = true; a credit seat has no window and no reset, so no share of one has elapsed")
	}
}

// An account nobody could read is not part of the pool the quota is divided
// between: dividing by it would tighten every other account's threshold on the
// strength of an account that cannot be switched to.
func TestHoverCountsOnlyAccountsItCanRead(t *testing.T) {
	pool := hoverPool(t, 2, usage.WindowSevenDay, elapsedWindow(7*24*time.Hour, 0.43, 10))
	pool = append(pool, Candidate{UUID: "u-unread", Kind: pool[0].Kind})

	p := HoverThresholds(pool, opts())
	if p.Usable != 2 {
		t.Fatalf("Usable = %d, want 2: an account with no reading is not one the work can go to", p.Usable)
	}
	if got := p.For("u-0").For(usage.WindowSevenDay); got != 93 {
		t.Errorf("threshold = %v, want 93 -- the unread account must not narrow the share", got)
	}
	// And it still gets an answer if it is ever asked about.
	if got := p.For("u-unread").For(usage.WindowSevenDay); got != HoverFallbackThreshold {
		t.Errorf("threshold for the unread account = %v, want %v", got, HoverFallbackThreshold)
	}
}

// A weekly cap filed under a scope key this build cannot name binds only where
// the user has set a threshold on its name, and that opt-in survives hover.
//
// The two rules meet here and they are not the same rule. The VALUE in the table
// is a tuning number, which hover overrides like every other one. The PRESENCE
// of an entry is consent -- the user saying they know what the scope means --
// and consent is not a knob a mode may supply on the user's behalf. So hover
// reads the configured table to decide what may bind, and derives what it is
// worth.
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

	// No entry in the table: hover derives nothing for it, exactly as the
	// ranking binds nothing on it.
	if _, ok := rowFor(HoverThresholds(pool, opts()), "u-0", name); ok {
		t.Error("hover derived a threshold for a scope nobody opted in to; the mode must not grant consent")
	}

	// 50 rather than 80: DefaultThreshold IS 80 and the fallback IS 80, so a
	// hover that simply copied the configured number through would be
	// indistinguishable from one that ignored it.
	o := perWindow(map[usage.WindowName]float64{name: 50})
	p := HoverThresholds(pool, o)

	row, ok := rowFor(p, "u-0", name)
	if !ok {
		t.Fatalf("Windows = %+v; a threshold on the name is the opt-in and hover has to honour it", p.Windows)
	}
	if row.Threshold != 93 || !row.HasExpected || row.ExpectedPct != 43 {
		t.Errorf("row = %+v, want threshold 93 from a 43%% elapsed share and a two-account slice", row)
	}
	if got := p.For("u-0").For(name); got != 93 {
		t.Errorf("threshold = %v, want 93; the configured 50 is a number hover replaces", got)
	}

	// The set hover derived for and the set the ranking measures have to be the
	// same set, or a window admitted to one is narrowed out of the other.
	h := HeadroomFor(pool[0].Usage, "", p.For("u-0"))
	if h.Binding != name {
		t.Errorf("Binding = %q, want %q -- the derived table has to re-admit the window it derived for", h.Binding, name)
	}
	if h.Slack != -2 {
		t.Errorf("Slack = %v, want -2 (93 - 95)", h.Slack)
	}
}

// The lead comes from the gap the scheduler actually left, not from a number in
// the file. A 429 that stretches the cadence stretches the lead with it, and a
// tight cadence spends less quota getting out of the way early.
func TestHoverTakesThePreemptLeadFromTheObservedPollGap(t *testing.T) {
	for _, tc := range []struct {
		name string
		gap  time.Duration
		want time.Duration
	}{
		{"nothing observed yet", 0, HoverMinPreemptLead},
		{"below the floor", 30 * time.Second, HoverMinPreemptLead},
		{"an ordinary cadence", 300 * time.Second, 300 * time.Second},
		{"the post-429 ceiling", 1800 * time.Second, HoverMaxPreemptLead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := sub("u-1", oneWindow(t, usage.WindowFiveHour, elapsedWindow(5*time.Hour, 0.5, 10)))
			if tc.gap > 0 {
				c.FetchedAt, c.NextPollAt = now, now.Add(tc.gap)
			}
			if got := HoverThresholds([]Candidate{c}, opts()).PreemptLead; got != tc.want {
				t.Errorf("PreemptLead = %v, want %v", got, tc.want)
			}
		})
	}
}

// A reading with a next poll and no FetchedAt has no gap, and subtracting the
// zero time does not produce one either: Sub saturates at about 292 years, so an
// account with no provenance would otherwise drag every OTHER account's lead to
// the ceiling. The refusal is preemptHorizon's, and going through it is what
// keeps one answer to "how blind is this engine" rather than two.
func TestHoverIgnoresAPollGapWithNoProvenance(t *testing.T) {
	blind := sub("u-0", oneWindow(t, usage.WindowFiveHour, elapsedWindow(5*time.Hour, 0.5, 10)))
	blind.NextPollAt = now.Add(time.Hour)
	seen := sub("u-1", oneWindow(t, usage.WindowFiveHour, elapsedWindow(5*time.Hour, 0.5, 10)))
	seen.FetchedAt, seen.NextPollAt = now, now.Add(120*time.Second)

	if got := HoverThresholds([]Candidate{blind, seen}, opts()).PreemptLead; got != 120*time.Second {
		t.Errorf("PreemptLead = %v, want the one gap that was actually observed, 2m", got)
	}
}

// The keys hover ignores stop mattering, and the ONE key it does not ignore
// still does. Two accounts identical but for the primary flag, under a ceiling
// of 0 -- which is the default, and one of the two independent opt-ins
// unattended overage requires.
func TestHoverIgnoresTheTuningAndNotTheMoneyGate(t *testing.T) {
	// Spent on the axis hover derives: 43% of the week gone, two usable
	// accounts, so the threshold is 93 and 95% used is two points past it.
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
	// cooldown longer than the pass. Both are keys hover overrides.
	//
	// The threshold is NOT hostile here, and that is the change. It is the one
	// number that still governs the money question -- see the case below -- so
	// leaving it at 99 would stop this pass before it ever reached the gate,
	// and the gate is what this case is about.
	o.Threshold = 90
	tuned := Config{HysteresisPct: 90, HeadroomRatio: 5, Cooldown: time.Hour}

	// max_auto_spend is 0, and the seat is not primary.
	p := Decide([]Candidate{spent, seat}, o, tuned, NewState(), "a")
	want(t, p, ActionBlocked, ReasonCreditGate, "")
	if p.Credit.Reason != CreditNotOptedIn {
		t.Errorf("Credit.Reason = %v, want %v: hover must not switch on unattended spending",
			p.Credit.Reason, CreditNotOptedIn)
	}

	// The same seat, marked primary by a human typing the command. Now it is
	// ranked in the main pool on its own axis and the money gate never applies.
	seat.Primary = true
	p = Decide([]Candidate{spent, seat}, o, tuned, NewState(), "c")
	want(t, p, ActionStay, ReasonAlreadyBest, "")

	p = Decide([]Candidate{spent, seat}, o, tuned, NewState(), "a")
	want(t, p, ActionSwitch, ReasonBetterTarget, "c")
}

// Hover's threshold may not open the paid pool.
//
// Hover derives a threshold from how far through its window an account is: a
// PACE target, not a stop line, and one nobody typed. The account below is two
// points past that derived figure and nine points short of the number the user
// actually chose. Reading hover's figure here would start buying credits while
// nine points of paid-for subscription quota sat unspent -- and would do it on
// a number the user never saw, which is exactly the consent HoverThresholds
// already refuses to invent for a scoped weekly cap.
func TestHoverThresholdDoesNotOpenTheCreditPool(t *testing.T) {
	// 43% of the week gone with two usable accounts puts hover's threshold at
	// 93. The user's own threshold is 99. Utilization is 95: past hover's,
	// short of theirs.
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
	o.Threshold = 99

	p := Decide([]Candidate{spent, seat}, o, Config{MaxAutoSpend: 500}, NewState(), "a")
	if p.CreditConsulted {
		t.Errorf("the credit pool was consulted: hover's pace threshold must not authorise a purchase")
	}
	want(t, p, ActionStay, ReasonAlreadyBest, "")
}

// The plan hover ran on is reported, because an automatic mode a user cannot
// audit is one they have to take on trust.
func TestDecideReportsThePlanHoverRanOn(t *testing.T) {
	pool := hoverPool(t, 2, usage.WindowSevenDay, elapsedWindow(7*24*time.Hour, 0.43, 10))
	o := opts()
	o.Hover = true

	p := Decide(pool, o, Config{}, NewState(), "u-0")
	if p.Hover == nil {
		t.Fatal("Plan.Hover is nil with hover on; `ccdad hover status` has nothing to print")
	}
	if p.Hover.Usable != 2 {
		t.Errorf("Plan.Hover.Usable = %d, want 2", p.Hover.Usable)
	}
	if off := Decide(pool, opts(), Config{}, NewState(), "u-0"); off.Hover != nil {
		t.Error("Plan.Hover is set with hover off; nil is what says the mode was not in force")
	}
}

// Hover's own margins, and the one figure it hands back untouched.
func TestHoverConfigReplacesTheMarginsAndNotTheCeiling(t *testing.T) {
	tuned := Config{
		HysteresisPct:      40,
		HeadroomRatio:      3,
		Cooldown:           time.Hour,
		RecoveryHysteresis: time.Hour,
		MaxAutoSpend:       25,
	}
	got := HoverConfig(tuned)

	if got.HysteresisPct != HoverHysteresisPct || got.HeadroomRatio != HoverHeadroomRatio {
		t.Errorf("margins = %v, %v; want %v, %v",
			got.HysteresisPct, got.HeadroomRatio, HoverHysteresisPct, HoverHeadroomRatio)
	}
	if got.Cooldown != HoverCooldown || got.RecoveryHysteresis != HoverRecoveryHysteresis {
		t.Errorf("cooldowns = %v, %v; want %v, %v",
			got.Cooldown, got.RecoveryHysteresis, HoverCooldown, HoverRecoveryHysteresis)
	}
	if got.MaxAutoSpend != 25 {
		t.Errorf("MaxAutoSpend = %v, want 25 left exactly as the file set it", got.MaxAutoSpend)
	}
}

// preempt_lead is a ranking input rather than an anti-flap knob, so hover's
// derived lead has to land on the pass and not on the anti-flap set. A lead left
// behind here is the pre-emptive switch silently off.
func TestHoverPutsItsDerivedLeadOnTheRankingPass(t *testing.T) {
	c := sub("u-1", oneWindow(t, usage.WindowFiveHour, elapsedWindow(5*time.Hour, 0.5, 10)))
	c.FetchedAt, c.NextPollAt = now, now.Add(300*time.Second)
	o := opts()
	o.Hover = true

	got := o.withHover([]Candidate{c})
	if got.PreemptLead != 300*time.Second {
		t.Errorf("PreemptLead = %v, want the observed gap of 300s", got.PreemptLead)
	}
	if got.CreditThreshold != HoverCreditThreshold {
		t.Errorf("CreditThreshold = %v, want %v", got.CreditThreshold, HoverCreditThreshold)
	}
	if got.Strategy != StrategyHeadroom {
		t.Errorf("Strategy = %v, want %v", got.Strategy, StrategyHeadroom)
	}
}

// consume-first is one of the keys hover overrides. Hover has already spent the
// perishable window first by giving it a high threshold as its reset nears, and
// re-ordering by reset instant would discard every threshold it derived.
func TestHoverOverridesTheConfiguredStrategy(t *testing.T) {
	pool := hoverPool(t, 2, usage.WindowSevenDay, elapsedWindow(7*24*time.Hour, 0.43, 10))
	o := opts()
	o.Hover, o.Strategy = true, StrategyConsumeFirst

	if got := Rank(pool, o).Mode; got != ModeHeadroom {
		t.Errorf("Mode = %v, want %v: hover derives the thresholds the ranking runs on", got, ModeHeadroom)
	}
}

// A pool of none is a pool of one, not a division by zero. Decide installs the
// pass BEFORE it discovers the pool is empty -- an all-quarantined store reaches
// here -- and an infinite share would put every later threshold at the cap
// through arithmetic rather than through the reason the cap exists.
func TestHoverShareOfAnEmptyPoolIsTheWholeQuota(t *testing.T) {
	if got := hoverShare(0); got != 100 {
		t.Errorf("hoverShare(0) = %v, want 100 -- with nobody to hand to, the honest share is everything", got)
	}
	if got := hoverShare(4); got != 25 {
		t.Errorf("hoverShare(4) = %v, want 25", got)
	}
}

// A disabled account is not somewhere the work can go, so it does not narrow
// anybody else's share. It is the same rule the unreadable account follows and
// it is one of the two flags hover does NOT override.
func TestHoverDoesNotDivideTheQuotaWithADisabledAccount(t *testing.T) {
	pool := hoverPool(t, 2, usage.WindowSevenDay, elapsedWindow(7*24*time.Hour, 0.43, 10))
	off := sub("u-off", oneWindow(t, usage.WindowSevenDay, elapsedWindow(7*24*time.Hour, 0.43, 10)))
	off.Disabled = true

	p := HoverThresholds(append(pool, off), opts())
	if p.Usable != 2 {
		t.Fatalf("Usable = %d, want 2: a disabled account is not one the work can go to", p.Usable)
	}
	if got := p.For("u-0").For(usage.WindowSevenDay); got != 93 {
		t.Errorf("threshold = %v, want 93 -- the disabled account must not narrow the share", got)
	}
}

// Decide installs the pass over the pool quarantine has already filtered, and
// Rank must not derive a second one on the way past. A second pass would take
// its share and its lead from whatever pool that caller happened to hold, which
// is how the anti-flap set comes to be derived from accounts the ranking never
// ranked.
func TestASecondHoverPassIsNeverDerived(t *testing.T) {
	two := hoverPool(t, 2, usage.WindowSevenDay, elapsedWindow(7*24*time.Hour, 0.43, 10))
	o := opts()
	o.Hover = true

	installed := o.withHover(two)
	if installed.hover == nil {
		t.Fatal("withHover installed nothing with hover on")
	}
	again := installed.withHover(two[:1])
	if again.hover != installed.hover {
		t.Errorf("the second call re-derived: Usable %d became %d", installed.hover.Usable, again.hover.Usable)
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
// utilization. The configured table admits it; hover derives nothing for a
// window with no utilization, so hover's table does not -- and a mixed pair
// would have the ranking measure a set the weekly reset was not taken over.
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

	// With hover on it is in neither: no utilization means no derived threshold,
	// and a window hover derived nothing for is not one the ranking measures.
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

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
		// Nobody to hand to. Spend everything that is left -- and the figure
		// says so by running past 100, which is what "no restraint" looks like
		// once nothing clamps it. 43 elapsed plus the whole 100 of the share.
		name:     "one account left",
		window:   usage.WindowSevenDay,
		length:   week,
		share:    0.43,
		accounts: 1,
		want:     143,
	}, {
		// It resets within the hour, so holding quota back buys nothing: 80
		// elapsed plus half the pool's share.
		name:     "a five-hour window 80% elapsed with two accounts",
		window:   usage.WindowFiveHour,
		length:   five,
		share:    0.80,
		accounts: 2,
		want:     130,
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
// derive from -- and the figure it gets is still on the derived scale, because
// the share the pool decides is added to an ASSUMED elapsed share rather than
// replaced by a constant.
//
// The pool sweep is the point. A flat number would answer the same whatever the
// pool was, and that is exactly what was wrong with the 80 this replaced: it was
// the threshold of a window 80 - 100/n percent elapsed, an implied position that
// moved with the pool and moved the wrong way. The row is still marked as one a
// probe would fix, and still says its share was not measured.
func TestHoverPutsAWindowWithNoClockOnTheDerivedScale(t *testing.T) {
	for _, tc := range []struct {
		accounts int
		want     float64
	}{
		{1, 150}, // nobody to hand to: the share is the whole 100
		{2, 100},
		{4, 75},
		{8, 62.5},
	} {
		pct := 30.0
		pool := make([]Candidate, 0, tc.accounts)
		for i := 0; i < tc.accounts; i++ {
			pool = append(pool, sub(fmt.Sprintf("u-%d", i), &usage.Snapshot{SevenDay: usage.NewWindow(&pct, nil)}))
		}

		p := HoverThresholds(pool, opts())
		if got := p.For("u-0").For(usage.WindowSevenDay); got != tc.want {
			t.Errorf("threshold with %d accounts = %v, want %v", tc.accounts, got, tc.want)
		}
		row, ok := rowFor(p, "u-0", usage.WindowSevenDay)
		if !ok {
			t.Fatalf("no row derived with %d accounts", tc.accounts)
		}
		if !row.ProbeWanted || row.HasExpected {
			t.Errorf("row = %+v, want ProbeWanted with no MEASURED elapsed share", row)
		}
	}
}

// A window with no clock is not a spent account, and this is the half no margin
// could ever have recovered.
//
// Under the flat 80 an account 85% through a window it will not date reported
// slack -5, which makes it SPENT -- and headroomTier is compared before slack in
// lessHeadroom, so it went to a strictly worse tier than every account with room
// and no value of HoverHysteresisPct could have brought it back. Fifteen points
// of a week left is not a spent account.
//
// The ORDER is deliberately not asserted. Where such an account sorts depends on
// the elapsed share hover ASSUMED for it, and asserting that would be asserting
// the guess. What the fix guarantees is the tier, which is a statement about
// evidence rather than about pace: nothing here is known to be over anything.
//
// It also cannot be probed away. ColdWindow refuses a window that has been spent
// against -- correctly, because another turn buys the same unreadable field back
// -- so this figure is the one the account lives with.
func TestAWindowWithNoClockDoesNotFileTheAccountAsSpent(t *testing.T) {
	noclock := 85.0
	b := sub("b-noclock", &usage.Snapshot{
		FiveHour: win(0, 5*time.Hour),
		SevenDay: usage.NewWindow(&noclock, nil),
	})
	pool := []Candidate{b, sub("a-roomy", snap(win(0, 5*time.Hour), win(10, 7*24*time.Hour)))}

	res := Rank(pool, hoverOpts())
	got, ok := find(res, "b-noclock")
	if !ok {
		t.Fatal("b-noclock is not in the ranking")
	}
	if spent, known := Spent(got.Headroom); known && spent {
		t.Errorf("Spent = %v; 15 points of a week left is not a spent account", spent)
	}
	if got.Headroom.Slack <= 0 {
		t.Errorf("slack = %v, want a positive figure", got.Headroom.Slack)
	}
	if tier := headroomTier(got); tier != 0 {
		t.Errorf("tier = %d, want 0 -- a window that will not say is not evidence of anything", tier)
	}

	// And it stays that way: nothing is going to give the window a reset.
	if _, _, ok := ColdWindow(b.Usage, "", opts().Thresholds(), now); ok {
		t.Error("ColdWindow targets a window that has been spent against; the threshold cannot be probed away and must be right on its own")
	}
}

// A reset FURTHER OUT than the window is long is not an unknown window. It is a
// window that has just started, seen through a clock a little behind the
// endpoint's, and the elapsed share is zero.
//
// It used to take the same flat figure as a window with no reset at all, which
// on a fresh five-hour window against a pool at 25 bought a 55-point lead with
// one minute of skew -- on a row nothing marked as a guess.
func TestAResetPastTheWindowLengthReadsAsAWindowThatHasJustStarted(t *testing.T) {
	five := 5 * time.Hour
	skewed := 0.0
	skew := sub("skew", &usage.Snapshot{FiveHour: usage.NewWindow(&skewed, tp(now.Add(five+time.Minute)))})
	pool := []Candidate{
		skew,
		sub("u-a", &usage.Snapshot{FiveHour: win(0, five)}),
		sub("u-b", &usage.Snapshot{FiveHour: win(0, five)}),
		sub("u-c", &usage.Snapshot{FiveHour: win(0, five)}),
	}

	p := HoverThresholds(pool, hoverOpts())
	row, ok := rowFor(p, "skew", usage.WindowFiveHour)
	if !ok {
		t.Fatal("no row for the skewed account")
	}
	if row.ExpectedPct != 0 || !row.HasExpected {
		t.Errorf("elapsed = %v (measured %v), want a measured 0", row.ExpectedPct, row.HasExpected)
	}
	if row.Threshold != 25 {
		t.Errorf("threshold = %v, want 25 -- the same figure the three unskewed accounts get", row.Threshold)
	}
}

// Every door that answers "hover derived nothing for this" is on the derived
// scale too, and strictly positive.
//
// Positive is load-bearing rather than tidy: bindingWindows reads a non-positive
// per-window entry as "not opted in", so a zero here would silently revoke a
// user's consent for an unknown-scope cap.
func TestEveryHoverDefaultIsPositiveAndOnTheDerivedScale(t *testing.T) {
	var none HoverPlan
	if got := none.For("nobody").Default; got != 150 {
		t.Errorf("zero plan threshold = %v, want 150 -- hoverPoolShare clamps a pool of none to a pool of one", got)
	}
	week := 7 * 24 * time.Hour
	pool := hoverPool(t, 2, usage.WindowSevenDay, elapsedWindow(week, 0.43, 10))
	p := HoverThresholds(pool, opts())
	if got := p.For("u-0").Default; got != 100 {
		t.Errorf("Default = %v, want 100 -- 50 assumed elapsed plus a two-account share of 50", got)
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
		t.Fatalf("Windows = %+v; `ccdad status` has one row to print for this seat", p.Windows)
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
	// And it still gets an answer if it is ever asked about -- on the same
	// derived scale, from the same pool: 50 assumed elapsed plus a share of 50.
	if got := p.For("u-unread").For(usage.WindowSevenDay); got != 100 {
		t.Errorf("threshold for the unread account = %v, want 100", got)
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

	// 50 rather than 80: DefaultThreshold IS 80, so a hover that simply copied
	// the configured number through would be indistinguishable from one that
	// ignored it.
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
		t.Fatal("Plan.Hover is nil with hover on; `ccdad status` has no thresholds to print")
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

// fleetOf20260825 is the pool `ccdad status` measured on 2026-08-25: six
// accounts, every one of them past the pace target hover derived for it, so the
// pass runs in recovery mode and nobody returns inside the horizon.
//
// The two that matter are 4-ejalrnrmf and 5-junseong. Both bind on seven_day, so
// their thresholds rise at exactly the same rate and the four points between
// their slacks are FROZEN against the clock -- the only thing that closes that
// gap is the live account spending another point, which is a point spent on the
// account holding ten of raw room rather than the one holding thirty.
func fleetOf20260825() []Candidate {
	return []Candidate{
		hoverAt("1-official", 0.92, 100),   // 0 points left, slack -1 under the cap
		hoverAt("2-kweizaa", 0.33, 85),     // 15 left, slack -35.33
		hoverAt("3-tlfyvhsdlek", 0.17, 64), // 36 left, slack -30.33
		hoverAt("4-ejalrnrmf", 0.38, 70),   // 30 left, slack -15.33
		hoverAt("5-junseong", 0.54, 90),    // 10 left, slack -19.33
		hoverAt("6-chan", 0.37, 80),        // 20 left, slack -26.33
	}
}

// The margin has to sit UNDER the spread a real pool shows, or it strands the
// engine on the emptier account for as long as the gap stays frozen.
func TestTheHoverMarginIsUnderTheSpreadARealPoolShows(t *testing.T) {
	p := Decide(fleetOf20260825(), hoverOpts(), Config{}, NewState(), "5-junseong")

	// Stated rather than assumed: if the fixture ever drifts, the failure should
	// name the gap and not just the verdict.
	active, ok := find(p.Result, "5-junseong")
	if !ok {
		t.Fatal("the live account is not in the pass")
	}
	best := p.Result.Order[0]
	if best.UUID != "4-ejalrnrmf" {
		t.Fatalf("best = %q, want 4-ejalrnrmf: the pool this test is about is not the pool it ranked", best.UUID)
	}
	if gap := best.Headroom.Slack - active.Headroom.Slack; gap < HoverHysteresisPct {
		t.Fatalf("gap = %.2f points, margin = %.2f: the margin is back above the spread", gap, HoverHysteresisPct)
	}
	if p.Action != ActionSwitch {
		t.Errorf("action = %s (%s), want switch off the account with ten points of room "+
			"onto the one with thirty", p.Action, p.Reason)
	}
	if p.Target.UUID != "4-ejalrnrmf" {
		t.Errorf("target = %q, want 4-ejalrnrmf", p.Target.UUID)
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

// consume-first is one of the keys hover overrides, and hover has to reach the
// same answer for it to be allowed to.
//
// The override is what it always was: the mode ranks in ModeHeadroom on the
// thresholds it derived, rather than re-sorting on the reset instant and
// discarding them. What this case did not used to check is whether the derived
// order AGREES with the strategy it overrides -- its pool was two identical
// accounts carrying one window each, so seven_day was the binding window by
// construction and no ordering difference could have been observed even if
// there were one. That is exactly the shape the subsumption is true in, and it
// is not the shape a real account has; withHover's own comment says why.
//
// So the pool here is two accounts that differ ONLY in when their week ends,
// each carrying both windows, and the five-hour window is what binds for both.
// The mode must still be headroom, and the sooner week must still lead.
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
	if res.Mode != ModeHeadroom {
		t.Errorf("Mode = %v, want %v: hover derives the thresholds the ranking runs on", res.Mode, ModeHeadroom)
	}
	for _, r := range res.Order {
		if r.Headroom.Binding != usage.WindowFiveHour {
			t.Fatalf("%s binds on %s, want five_hour: the override is only worth checking where the perishable window does not bind", r.UUID, r.Headroom.Binding)
		}
	}
	eq(t, order(res), []string{"u-sooner", "u-later"})
}

// The pool size reaches the ORDER now, and not only the thresholds.
//
// 100/N cancels from every comparison, so under the flat share alone a pool of
// two and a pool of four rank the same accounts the same way. The stranded half
// does not cancel: N is what says how much of a week the rotation can absorb, so
// the same account can strand quota in a small pool and none in a large one.
// This is a real behaviour change and it is asserted rather than left to be
// discovered.
func TestThePoolSizeNowReachesTheOrderAndNotOnlyTheThresholds(t *testing.T) {
	week := 7 * 24 * time.Hour
	// A week 80% elapsed at 10% used: 90 points left and a fifth of the week to
	// spend them in. One account absorbs 20, two absorb 40, five absorb 100.
	s := &usage.Snapshot{
		FiveHour: elapsedWindow(5*time.Hour, 0.20, 10),
		SevenDay: elapsedWindow(week, 0.80, 10),
	}
	th := opts().Thresholds()

	if _, stranded, _ := hoverStranded(s, "", th, 2, now); stranded != 50 {
		t.Errorf("with two accounts stranded = %v, want 50: the pair reaches 40 of the 90 points left", stranded)
	}
	if _, stranded, _ := hoverStranded(s, "", th, 5, now); stranded != 0 {
		t.Errorf("with five accounts stranded = %v, want 0: the rotation reaches all 90", stranded)
	}
}

// A pool of none is a pool of one, not a division by zero. Decide installs the
// pass BEFORE it discovers the pool is empty -- an all-quarantined store reaches
// here -- and an infinite share would put every later threshold at the cap
// through arithmetic rather than through the reason the cap exists.
func TestHoverShareOfAnEmptyPoolIsTheWholeQuota(t *testing.T) {
	if got := hoverShare(0, 0); got != 100 {
		t.Errorf("hoverShare(0) = %v, want 100 -- with nobody to hand to, the honest share is everything", got)
	}
	if got := hoverShare(4, 0); got != 25 {
		t.Errorf("hoverShare(4) = %v, want 25", got)
	}
	// The two halves are a MAXIMUM, pinned in both directions: stranded quota
	// widens the slice when it is the larger claim, and an account already
	// licensed to run 33 points ahead needs no second licence for 7.
	if got := hoverShare(4, 63); got != 63 {
		t.Errorf("hoverShare(4, 63) = %v, want 63 -- stranded quota widens the slice", got)
	}
	if got := hoverShare(2, 33); got != 50 {
		t.Errorf("hoverShare(2, 33) = %v, want the wider pool slice of 50", got)
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

// `ccdad status` must not print a slack the engine did not rank on. Otherwise
// the table says an empty window has room while the ranking says it has none.
func TestHoverStatusNeverShowsPositiveSlackOnAnEmptyWindow(t *testing.T) {
	pool := []Candidate{hoverAt("solo", 0.92, 100)}
	plan := HoverThresholds(pool, hoverOpts())

	found := false
	for _, row := range plan.Windows {
		if row.UUID != "solo" || row.Window != usage.WindowSevenDay {
			continue
		}
		found = true
		if row.Threshold <= 100 {
			t.Fatalf("Threshold = %v; this test is pointless unless the pace target ran past 100", row.Threshold)
		}
		if row.Slack > 0 {
			t.Errorf("Slack = %+v on a window at 100%% used, held to %v — the table would show room the engine does not rank on",
				row.Slack, row.Threshold)
		}
	}
	if !found {
		t.Fatal("no seven_day row for the account; the fixture no longer says what it claims")
	}
}

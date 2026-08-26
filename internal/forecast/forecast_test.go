package forecast

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/history"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// pf is a pointer to a float, which is how usage.NewWindow says "reported".
func pf(v float64) *float64 { return &v }

// pt is a pointer to a time, likewise.
func pt(t time.Time) *time.Time { return &t }

// snapshotWithWeekly is one account's CURRENT reading with a single weekly
// window at pct. The reset is far enough out that no rollover can reach into a
// test that is asking about levels rather than about time.
func snapshotWithWeekly(pct float64, reset time.Time) *usage.Snapshot {
	return &usage.Snapshot{SevenDay: usage.NewWindow(pf(pct), pt(reset))}
}

// weeklySeries builds a series carrying `names` at the given percentages, one
// sample per percentage, evenly spread backwards from `last`.
//
// The samples are spread over `span` rather than bunched, because the rate this
// package measures refuses a series that does not clear its span floor, and a
// fixture that failed that gate would report unmeasured and pass a test looking
// for a zero.
func weeklySeries(last time.Time, span time.Duration, pcts []float64, reset time.Time, names ...usage.WindowName) []history.Sample {
	out := make([]history.Sample, 0, len(pcts))
	step := span / time.Duration(len(pcts)-1)
	for i, p := range pcts {
		at := last.Add(-span + time.Duration(i)*step)
		w := make(map[usage.WindowName]history.Reading, len(names))
		for _, n := range names {
			w[n] = history.Reading{Pct: p, Reset: reset}
		}
		out = append(out, history.Sample{At: at, Windows: w})
	}
	return out
}

// namedSeries builds a series in which every window named in pcts follows its
// OWN column of percentages, one sample per row, evenly spread backwards from
// last.
//
// weeklySeries above cannot express that: it files one percentage under every
// name, which is the right fixture for "these windows rise together" and the
// wrong one for any test about WHICH window was picked, because a fixture that
// ties four windows at one level rules out summing them and cannot tell which
// of the four the code chose.
func namedSeries(last time.Time, span time.Duration, reset time.Time, pcts map[usage.WindowName][]float64) []history.Sample {
	n := -1
	for _, col := range pcts {
		if n >= 0 && len(col) != n {
			panic("namedSeries: every window needs one percentage per sample")
		}
		n = len(col)
	}
	out := make([]history.Sample, 0, n)
	step := span / time.Duration(n-1)
	for i := range n {
		w := make(map[usage.WindowName]history.Reading, len(pcts))
		for name, col := range pcts {
			w[name] = history.Reading{Pct: col[i], Reset: reset}
		}
		out = append(out, history.Sample{At: last.Add(-span + time.Duration(i)*step), Windows: w})
	}
	return out
}

// The weekly axis is a SET of windows that all meter the same consumption: one
// prompt raises seven_day, seven_day_opus and -- Claude Code being an OAuth app
// -- seven_day_oauth_apps together. Summing them counts the same tokens three to
// five times over, and the replenishment figure the reader is told to compare
// against is counted one window per account, so the two columns would not be on
// one basis.
func TestAnAccountContributesOneWeeklyFigureHoweverManyWeeklyWindowsItHas(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	reset := now.Add(72 * time.Hour)
	// Four weekly windows rising together at half a point an hour -- two points
	// over the four-hour measured span -- but at four DIFFERENT levels, so the
	// fixture says which one was picked as well as that they were not summed.
	// seven_day_opus is the tightest, with five points left against the others'
	// 88, 78 and 68.
	series := namedSeries(now, 4*time.Hour, reset, map[usage.WindowName][]float64{
		usage.WindowSevenDay:          {10, 11, 12},
		usage.WindowSevenDayOpus:      {93, 94, 95},
		usage.WindowSevenDayOAuthApps: {20, 21, 22},
		usage.WindowSevenDaySonnet:    {30, 31, 32},
	})
	snap := &usage.Snapshot{
		SevenDay:          usage.NewWindow(pf(12), pt(reset)),
		SevenDayOpus:      usage.NewWindow(pf(95), pt(reset)),
		SevenDayOAuthApps: usage.NewWindow(pf(22), pt(reset)),
		SevenDaySonnet:    usage.NewWindow(pf(32), pt(reset)),
	}
	got := Of([]Input{{UUID: "uuid-a", Idx: 1, Eligible: true, Snapshot: snap, Series: series}}, now)

	if !got.Weekly.Burn.Known {
		t.Fatal("the weekly burn is unmeasured; three readings over four hours clear both contribution gates")
	}
	if got.Weekly.Burn.Low != 0.5 {
		t.Fatalf("Weekly.Burn.Low = %v, want 0.5 -- four windows metering one consumption were summed into four times the burn", got.Weekly.Burn.Low)
	}
	// The account's one weekly figure is also what its room is counted from,
	// and the two have to be the same window or the burn and the points left
	// would describe different quotas. LEAST room, not most: the window that
	// runs out first is the one that says how much work this account can still
	// take, and picking the roomiest would report sixteen times the capacity
	// this account actually has.
	if got.PointsLeft != 5 {
		t.Fatalf("PointsLeft = %v, want 5 -- one window per account, the tightest of the four", got.PointsLeft)
	}
	if got.PointsTotal != 100 {
		t.Fatalf("PointsTotal = %v, want 100 -- one readable account", got.PointsTotal)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("len(Rows) = %d, want 1 -- one row per account, not one per window", len(got.Rows))
	}
	if got.Rows[0].Window != usage.WindowSevenDayOpus || got.Rows[0].Left != 5 {
		t.Errorf("Rows[0] = %s at %v left, want seven_day_opus at 5 -- the account's least-room weekly window",
			got.Rows[0].Window, got.Rows[0].Left)
	}
	if got.Rows[0].Burn.Low != 0.5 {
		t.Errorf("Rows[0].Burn.Low = %v, want 0.5 -- the row's rate must be the same figure the axis sums", got.Rows[0].Burn.Low)
	}
}

// The binding window is WEEKLY, whatever else is tighter. The five-hour window
// comes back every five hours, so a fleet's capacity is not measured by a quota
// that refills before lunch -- and the points beside it are counted on the
// weekly axis, so binding an account to its five-hour window would put a
// five-hour room into a weekly total.
func TestTheBindingWindowIsWeeklyEvenWhenAFiveHourWindowIsTighter(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	weeklyReset, fiveReset := now.Add(72*time.Hour), now.Add(2*time.Hour)
	// Ten points left on the five-hour window against seventy on the weekly
	// one, and both measurable: an account under real five-hour pressure.
	series := namedSeries(now, 4*time.Hour, weeklyReset, map[usage.WindowName][]float64{
		usage.WindowSevenDay: {26, 28, 30},
		usage.WindowFiveHour: {74, 82, 90},
	})
	snap := &usage.Snapshot{
		FiveHour: usage.NewWindow(pf(90), pt(fiveReset)),
		SevenDay: usage.NewWindow(pf(30), pt(weeklyReset)),
	}
	got := Of([]Input{{UUID: "uuid-a", Idx: 1, Eligible: true, Snapshot: snap, Series: series}}, now)

	if len(got.Rows) != 1 || got.Rows[0].Window != usage.WindowSevenDay || got.Rows[0].Left != 70 {
		t.Fatalf("Rows = %+v, want one row on seven_day with 70 left", got.Rows)
	}
	if got.PointsLeft != 70 {
		t.Errorf("PointsLeft = %v, want 70 -- the fleet's points are the weekly axis's room", got.PointsLeft)
	}
	// And the rate reported beside those points is the weekly one, not the
	// five-hour window's four points an hour.
	if got.Weekly.Burn.Low != 1 {
		t.Errorf("Weekly.Burn.Low = %v, want 1 -- the weekly window rose four points over the four-hour span", got.Weekly.Burn.Low)
	}
}

// "Holds" is a claim the upper bound of the measurement had to survive. When the
// two runs disagree the answer is neither of them: the basis is too thin to
// decide, and saying so is the honest third state. Deciding on the low end alone
// would promise safety the evidence does not carry.
func TestABandThatStraddlesTheBoundaryDecidesNothing(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	reset := now.Add(30 * time.Minute)
	// One account on the five-hour axis replenishes 100 points every five
	// hours, so twenty points an hour is exactly the boundary. 79.5 points
	// filled over the four-hour span puts the band at 19.875 and 20.125 -- one
	// run either side of it.
	series := weeklySeries(now, 4*time.Hour, []float64{0, 39.75, 79.5}, reset, usage.WindowFiveHour)
	snap := &usage.Snapshot{FiveHour: usage.NewWindow(pf(79.5), pt(reset))}
	got := Of([]Input{{UUID: "uuid-a", Idx: 1, Eligible: true, Snapshot: snap, Series: series}}, now)

	if got.FiveHour.Burn.Low != 19.875 || got.FiveHour.Burn.High != 20.125 {
		t.Fatalf("burn band = %v/%v, want 19.875/20.125", got.FiveHour.Burn.Low, got.FiveHour.Burn.High)
	}
	if got.FiveHour.Verdict != VerdictUnknown {
		t.Errorf("FiveHour.Verdict = %v, want VerdictUnknown -- the low run holds and the high run runs dry, so the measurement decides nothing", got.FiveHour.Verdict)
	}
	if got.FiveHour.HasDryAt {
		t.Errorf("FiveHour.HasDryAt = true (%v); a moment named from a verdict that was not reached is a fact the evidence does not carry", got.FiveHour.DryAt)
	}
}

// rate_limit_tier is a tri-state field: an account added while the profile
// endpoint was unreachable carries "" and is still polled and ranked. Summing
// percentage points across accounts assumes their quotas are the same size, and
// an unknown tier is never counted as agreement.
func TestAFleetWithNoTiersAtAllSaysSoRatherThanAgreeing(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	reset := now.Add(72 * time.Hour)
	in := []Input{
		{UUID: "uuid-a", Idx: 1, Eligible: true, Snapshot: snapshotWithWeekly(10, reset)},
		{UUID: "uuid-b", Idx: 2, Eligible: true, Snapshot: snapshotWithWeekly(20, reset)},
	}
	if got := Of(in, now); got.TierNotice == "" {
		t.Fatal("a fleet reporting no tier at all was treated as a fleet whose tiers agree")
	}

	// The same fleet with one tier reported and one absent is still undecided:
	// one known tier says nothing about the other account's quota size.
	in[0].Tier = "default_claude_max_20x"
	if got := Of(in, now); got.TierNotice == "" {
		t.Fatal("a fleet with one tier missing was treated as a fleet whose tiers agree")
	}

	// Agreement is agreement, and a notice on a uniform fleet would be noise on
	// every ordinary machine.
	in[1].Tier = "default_claude_max_20x"
	if got := Of(in, now); got.TierNotice != "" {
		t.Fatalf("TierNotice = %q on a fleet whose tiers are present and equal", got.TierNotice)
	}

	// Two tiers that are present and different is the case the notice was
	// written for, and it names them.
	in[1].Tier = "default_claude_pro"
	got := Of(in, now)
	if got.TierNotice == "" {
		t.Fatal("a fleet mixing two known tiers reported no notice")
	}
	for _, want := range []string{"default_claude_max_20x", "default_claude_pro"} {
		if !contains(got.TierNotice, want) {
			t.Errorf("TierNotice = %q, which does not name %q -- a reader cannot check a mix that is not spelled out", got.TierNotice, want)
		}
	}
}

// contains is strings.Contains under another name, so this file's one assertion
// about wording does not pull a second import in for it.
func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// The history supplies slopes; usage.json supplies levels. internal/daemon's
// status.go states the rule every file in this store answers to -- a quantity
// has exactly one authoritative file, so two commands cannot disagree about one
// number -- and history.json stores utilization, resets and credit spend, so its
// newest sample duplicates the cache's current reading. It must never be the one
// that is read.
func TestTheHistoryNeverSuppliesALevel(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	reset := now.Add(72 * time.Hour)
	in := []Input{{
		UUID: "uuid-a", Idx: 1, Eligible: true,
		// The cache says 40; the newest sample says 90. The two disagree on
		// purpose: only a reader of the wrong file can tell them apart.
		Snapshot: snapshotWithWeekly(40, reset),
		Series:   weeklySeries(now, 4*time.Hour, []float64{86, 88, 90}, reset, usage.WindowSevenDay),
	}}
	got := Of(in, now)
	if got.PointsLeft != 60 {
		t.Fatalf("PointsLeft = %v, want 60 -- the level came from the series, not from the snapshot", got.PointsLeft)
	}
	if len(got.Rows) != 1 || got.Rows[0].Left != 60 {
		t.Fatalf("Rows = %+v, want one row with Left 60", got.Rows)
	}
	// The slope, by contrast, IS the series': four points over four hours.
	if !got.Weekly.Burn.Known || got.Weekly.Burn.Low != 1 {
		t.Fatalf("Weekly.Burn = %+v, want a known 1.0 -- the series is still what a rate is measured from", got.Weekly.Burn)
	}
}

// An axis with no window of its own decides NOTHING, however hard the fleet is
// burning elsewhere.
//
// This is the account whose weekly windows dropped out of one response while
// its five-hour one came back. Nothing weekly was read, so a weekly run has no
// event to run: it reaches the horizon without spending anything and reports
// that the axis holds. "The 7-day axis holds" over a fleet nobody took a single
// weekly reading of is the one output this whole measurement exists to refuse,
// and it is worse than saying nothing because it reads exactly like an answer.
func TestAnAxisWithNoWindowOfItsOwnPromisesNothing(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	fiveReset := now.Add(2 * time.Hour)
	// Eight points an hour on the five-hour window, and no weekly window at all.
	series := weeklySeries(now, 4*time.Hour, []float64{8, 24, 40}, fiveReset, usage.WindowFiveHour)
	snap := &usage.Snapshot{FiveHour: usage.NewWindow(pf(40), pt(fiveReset))}
	got := Of([]Input{{UUID: "uuid-a", Idx: 1, Eligible: true, Snapshot: snap, Series: series}}, now)

	// The five-hour axis IS measured here, so this is not a fleet that was
	// simply never read: one axis answers and the other must not.
	if !got.FiveHour.Burn.Known || got.FiveHour.Burn.Low != 8 {
		t.Fatalf("FiveHour.Burn = %+v, want a known 8.0", got.FiveHour.Burn)
	}
	if got.Weekly.Verdict != VerdictUnknown {
		t.Errorf("Weekly.Verdict = %v, want VerdictUnknown -- not one weekly reading exists", got.Weekly.Verdict)
	}
	if got.Weekly.Burn.Known {
		t.Errorf("Weekly.Burn = %+v, want unmeasured -- a five-hour rate is not a weekly one", got.Weekly.Burn)
	}
	if got.Weekly.Replenish != 0 {
		t.Errorf("Weekly.Replenish = %v, want 0 -- a window nobody reported cannot roll over and give anything back", got.Weekly.Replenish)
	}
	if got.PointsLeft != 0 || got.PointsTotal != 0 {
		t.Errorf("points = %v of %v, want none of neither: points are counted on the weekly axis", got.PointsLeft, got.PointsTotal)
	}
	// The account keeps its row -- it is in the fleet and the run still has
	// something to say about it -- and the row carries no weekly quantity.
	if len(got.Rows) != 1 || got.Rows[0].HasWindow {
		t.Fatalf("Rows = %+v, want one row carrying no weekly window", got.Rows)
	}
}

// A five-hour window is not a weekly one, and an account that reported only the
// first lends the weekly column nothing.
//
// The two are the quantities this package refuses to add anywhere else: the
// windows run over 168 hours and five, so a percentage point of one is nothing
// like a percentage point of the other and borrowing one for the other is not a
// rounding effect. It would land in the weekly BURN figure, in the per-account
// burn column that is meant to sum to it, and in the fleet's remaining points.
func TestAnAccountWithNoWeeklyWindowLendsTheWeeklyAxisNothing(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	weeklyReset, fiveReset := now.Add(72*time.Hour), now.Add(2*time.Hour)
	in := []Input{{
		// Half a point an hour on the weekly axis, with 88 points left.
		UUID: "uuid-a", Idx: 1, Eligible: true,
		Snapshot: snapshotWithWeekly(12, weeklyReset),
		Series:   weeklySeries(now, 4*time.Hour, []float64{10, 11, 12}, weeklyReset, usage.WindowSevenDay),
	}, {
		// Seven and a half points an hour on the five-hour axis, and no weekly
		// window in the response at all.
		UUID: "uuid-b", Idx: 2, Eligible: true,
		Snapshot: &usage.Snapshot{FiveHour: usage.NewWindow(pf(40), pt(fiveReset))},
		Series:   weeklySeries(now, 4*time.Hour, []float64{10, 25, 40}, fiveReset, usage.WindowFiveHour),
	}}
	got := Of(in, now)

	if got.Weekly.Burn.Low != 0.5 {
		t.Errorf("Weekly.Burn.Low = %v, want 0.5 -- the fleet's measured weekly consumption is account a's alone", got.Weekly.Burn.Low)
	}
	if got.PointsLeft != 88 || got.PointsTotal != 100 {
		t.Errorf("points = %v of %v, want 88 of 100 -- b has no weekly quota to count", got.PointsLeft, got.PointsTotal)
	}
	// b is still in the fleet and still burning, which is why it is still
	// measured on the axis it does have a window for.
	if got.FiveHour.Burn.Low != 7.5 {
		t.Errorf("FiveHour.Burn.Low = %v, want 7.5 -- b is in the fleet, on the axis it reported", got.FiveHour.Burn.Low)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("len(Rows) = %d, want 2 -- one per readable account", len(got.Rows))
	}
	for _, r := range got.Rows {
		switch r.UUID {
		case "uuid-a":
			if !r.HasWindow || r.Window != usage.WindowSevenDay || r.Left != 88 {
				t.Errorf("row a = %+v, want seven_day with 88 left", r)
			}
		case "uuid-b":
			if r.HasWindow {
				t.Errorf("row b = %+v, want no weekly window: its five-hour room is not weekly room", r)
			}
		}
	}
}

// An account the rotation cannot switch to is not the fleet's capacity.
//
// eligible() in internal/strategy/rank.go is the rule for what the engine can
// hand work to, and an account outside it is unreachable in exactly the way an
// unreadable one is: the run must not make it live, its quota must not be added
// to the fleet's, and its readings are not evidence about a rotation that
// cannot reach it. A runway counting a disabled seat promises hours the fleet
// has no way to spend.
func TestAnAccountTheRotationCannotReachIsNotFleetCapacity(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	reset := now.Add(72 * time.Hour)
	in := []Input{{
		// Two points an hour with five points left: two and a half hours.
		UUID: "uuid-a", Idx: 1, Eligible: true,
		Snapshot: snapshotWithWeekly(95, reset),
		Series:   weeklySeries(now, 4*time.Hour, []float64{87, 91, 95}, reset, usage.WindowSevenDay),
	}, {
		// The same burn against ninety points of room, held out of rotation.
		UUID: "uuid-b", Idx: 2, Eligible: false,
		Snapshot: snapshotWithWeekly(10, reset),
		Series:   weeklySeries(now, 4*time.Hour, []float64{2, 6, 10}, reset, usage.WindowSevenDay),
	}}
	got := Of(in, now)

	if got.Basis.Accounts != 2 || got.Basis.Ineligible != 1 {
		t.Errorf("basis = %+v, want 2 accounts of which 1 is out of rotation", got.Basis)
	}
	if got.Basis.Readings != 3 {
		t.Errorf("Basis.Readings = %d, want 3 -- only the reachable account's readings were weighed", got.Basis.Readings)
	}
	if got.PointsLeft != 5 || got.PointsTotal != 100 {
		t.Errorf("points = %v of %v, want 5 of 100 -- b's ninety points are not the fleet's to spend", got.PointsLeft, got.PointsTotal)
	}
	if len(got.Rows) != 1 || got.Rows[0].UUID != "uuid-a" {
		t.Fatalf("Rows = %+v, want a's row alone", got.Rows)
	}
	if got.Weekly.Burn.Low != 2 {
		t.Errorf("Weekly.Burn.Low = %v, want 2 -- b's burn is not the rotation's", got.Weekly.Burn.Low)
	}
	want := now.Add(2*time.Hour + 30*time.Minute)
	if got.Weekly.Verdict != VerdictRunsDry || !got.Weekly.DryAt.Equal(want) {
		t.Errorf("weekly = %v at %v, want VerdictRunsDry at %v -- five points at two an hour",
			got.Weekly.Verdict, got.Weekly.DryAt, want)
	}
}

// A rate is measured over the last four hours and retention keeps eight, so on
// any machine that has been running half a day roughly half the samples on disk
// lie outside the window. Three independent filters keep them out -- the rate's
// own, the shared span's, and the reading count's -- and none of them is
// visible to a fixture that fits inside the window.
//
// The failure they prevent is silent rather than loud: dropping only the rate's
// filter sums eight hours of consumption and still divides by the four-hour
// span, so the burn doubles while the basis printed beside it still reads four
// hours and nine readings. The evidence on the screen would not betray the
// number it was supposed to support.
func TestReadingsOlderThanTheMeasuredSpanAreNotMeasured(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	reset := now.Add(72 * time.Hour)
	// Eight hours of half-hourly polling: six points an hour for the first
	// four, two an hour for the last four. The two halves differ so that a
	// measurement reaching past the boundary cannot come out right by accident.
	var series []history.Sample
	for i := range 17 {
		pct := 3 * float64(i)
		if i > 8 {
			pct = 24 + float64(i-8)
		}
		series = append(series, history.Sample{
			At:      now.Add(-8*time.Hour + time.Duration(i)*30*time.Minute),
			Windows: map[usage.WindowName]history.Reading{usage.WindowSevenDay: {Pct: pct, Reset: reset}},
		})
	}
	got := Of([]Input{{
		UUID: "uuid-a", Idx: 1, Eligible: true,
		Snapshot: snapshotWithWeekly(32, reset), Series: series,
	}}, now)

	if got.Weekly.Burn.Low != 2 || got.Weekly.Burn.High != 2.25 {
		t.Errorf("Weekly.Burn = %+v, want 2.00/2.25 -- the eight points spent inside the window, not the thirty-two on disk", got.Weekly.Burn)
	}
	if got.Basis.Observed != 4*time.Hour {
		t.Errorf("Basis.Observed = %v, want 4h -- the span the readings inside the window reach across", got.Basis.Observed)
	}
	if got.Basis.Readings != 9 {
		t.Errorf("Basis.Readings = %d, want 9 -- the samples inside the window, not the seventeen retained", got.Basis.Readings)
	}
}

// An axis whose window stopped being reported decides nothing, even while the
// fleet's basis is known.
//
// The two are different gates and it is easy to think one covers the other:
// every renderer asks Basis.Known, which is fleet-wide, while a verdict is
// decided per axis. A window that dropped out of the newest response is
// unmeasured -- rating it would freeze a slope from evidence that stopped
// arriving -- and an unmeasured axis burns nothing in the run, holds to the
// horizon, and would print "holds" beside a burn cell reading "?".
func TestAnAxisWhoseWindowStoppedBeingReportedDecidesNothing(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	weeklyReset, fiveReset := now.Add(72*time.Hour), now.Add(2*time.Hour)
	var series []history.Sample
	for i := range 5 {
		w := map[usage.WindowName]history.Reading{
			usage.WindowSevenDay: {Pct: float64(10 + i), Reset: weeklyReset},
		}
		// The five-hour window is in every response but the newest.
		if i < 4 {
			w[usage.WindowFiveHour] = history.Reading{Pct: float64(20 + 5*i), Reset: fiveReset}
		}
		series = append(series, history.Sample{At: now.Add(-4*time.Hour + time.Duration(i)*time.Hour), Windows: w})
	}
	got := Of([]Input{{
		UUID: "uuid-a", Idx: 1, Eligible: true,
		Snapshot: &usage.Snapshot{
			FiveHour: usage.NewWindow(pf(35), pt(fiveReset)),
			SevenDay: usage.NewWindow(pf(14), pt(weeklyReset)),
		},
		Series: series,
	}}, now)

	if !got.Basis.Known || !got.Weekly.Burn.Known {
		t.Fatalf("basis = %+v, weekly = %+v; the weekly axis is measurable here and the fleet has a basis", got.Basis, got.Weekly.Burn)
	}
	if got.FiveHour.Burn.Known {
		t.Errorf("FiveHour.Burn = %+v, want unmeasured -- the window is absent from the newest reading", got.FiveHour.Burn)
	}
	if got.FiveHour.Verdict != VerdictUnknown {
		t.Errorf("FiveHour.Verdict = %v, want VerdictUnknown -- an axis nobody measured cannot be reported as holding", got.FiveHour.Verdict)
	}
	if got.FiveHour.HasDryAt {
		t.Errorf("FiveHour.DryAt = %v on an axis that decided nothing", got.FiveHour.DryAt)
	}
}

// The credit runway, end to end: the balance from the current snapshot, the
// spend rate from the series, joined over one shared span.
//
// Both halves are unit-tested apart and neither test can see the join, which is
// the one path the rule about failing closed on money is written for. It is
// also where the tri-states are re-read: a limit of nil is unlimited and never
// a cap of zero, and used is money already spent rather than money left.
func TestTheCreditRunwayIsAssembledFromTheSnapshotAndTheSeries(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	reset := now.Add(72 * time.Hour)
	// The wire reports money in MINOR units: a $100 cap against $50 spent.
	// usage.ExtraUsage does that conversion, and nothing in the forecast
	// divides by 100 a second time.
	extra := usage.ExtraUsageFor(usage.ExtraUsageInput{
		State: usage.ExtraUsageEnabled, Currency: "USD",
		MonthlyLimit: pf(10000), UsedCredits: pf(5000),
	})
	// The series is in major units, which is what the recorder writes, and its
	// newest sample disagrees with the snapshot on purpose: $20 spent there
	// against the snapshot's $50. Only a reader of the wrong document can tell
	// the two apart, and the balance must come from the snapshot.
	used := []float64{14, 17, 20}
	var series []history.Sample
	for i, u := range used {
		series = append(series, history.Sample{
			At:      now.Add(-4*time.Hour + time.Duration(i)*2*time.Hour),
			Windows: map[usage.WindowName]history.Reading{usage.WindowSevenDay: {Pct: float64(10 + i), Reset: reset}},
			Credit:  &history.Credit{Used: u, Limit: pf(100), Currency: "USD"},
		})
	}
	snap := snapshotWithWeekly(12, reset)
	snap.ExtraUsage = extra
	got := Of([]Input{{UUID: "uuid-a", Idx: 1, Eligible: true, Snapshot: snap, Series: series}}, now)

	if !got.Credit.Known {
		t.Fatal("Credit.Known = false; a readable balance and a measured spend rate are exactly what this figure is made of")
	}
	if got.Credit.Currency != "USD" {
		t.Errorf("Credit.Currency = %q, want USD", got.Credit.Currency)
	}
	// Six dollars over the four-hour span.
	if got.Credit.SpendPerHour != 1.5 {
		t.Errorf("Credit.SpendPerHour = %v, want 1.5", got.Credit.SpendPerHour)
	}
	// Fifty dollars of room at a dollar fifty an hour. Read from the series'
	// newest sample instead, the room would be eighty and the date sixteen
	// hours later.
	// Computed rather than written out: the same expression the arithmetic
	// under test uses, so the assertion pins the figure and not the rounding of
	// a repeating decimal.
	hours := 50.0 / 1.5
	want := now.Add(time.Duration(hours * float64(time.Hour)))
	if !got.Credit.DryAt.Equal(want) {
		t.Errorf("Credit.DryAt = %v, want %v", got.Credit.DryAt, want)
	}
}

// TestTheCreditRunwayIsMeasuredForAFleetWithNoPlanWindows is the same join as
// the test above, on the fleet that is made entirely of money.
//
// The credit axis was fed the slice the WINDOW simulation had already filtered:
// an account with no simulatable plan window is counted unreadable and dropped
// before it, so a seat metered only in money could never reach the credit
// runway at all. Adding one irrelevant plan window to the same account made the
// figure appear, which is the tell.
//
// The existing end-to-end test cannot see this because its snapshot carries a
// weekly window.
func TestTheCreditRunwayIsMeasuredForAFleetWithNoPlanWindows(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	// A $100 cap against $50 spent, in the minor units the wire reports.
	snap := &usage.Snapshot{ExtraUsage: usage.ExtraUsageFor(usage.ExtraUsageInput{
		State: usage.ExtraUsageEnabled, Currency: "USD",
		MonthlyLimit: pf(10000), UsedCredits: pf(5000),
	})}
	// Major units, as the recorder writes them: six dollars over four hours.
	var series []history.Sample
	for i, u := range []float64{14, 17, 20} {
		series = append(series, history.Sample{
			At:     now.Add(-4*time.Hour + time.Duration(i)*2*time.Hour),
			Credit: &history.Credit{Used: u, Limit: pf(100), Currency: "USD"},
		})
	}

	got := Of([]Input{{UUID: "uuid-a", Idx: 1, Eligible: true, Snapshot: snap, Series: series}}, now)

	if !got.Credit.Known {
		t.Fatal("Credit.Known = false — the balance is readable and the spend rate is measured; only the window filter stood between them")
	}
	if got.Credit.SpendPerHour != 1.5 {
		t.Errorf("Credit.SpendPerHour = %v, want 1.5 — six dollars over the four-hour span", got.Credit.SpendPerHour)
	}
}

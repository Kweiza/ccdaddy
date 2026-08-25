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

// The weekly axis is a SET of windows that all meter the same consumption: one
// prompt raises seven_day, seven_day_opus and -- Claude Code being an OAuth app
// -- seven_day_oauth_apps together. Summing them counts the same tokens three to
// five times over, and the replenishment figure the reader is told to compare
// against is counted one window per account, so the two columns would not be on
// one basis.
func TestAnAccountContributesOneWeeklyFigureHoweverManyWeeklyWindowsItHas(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	reset := now.Add(72 * time.Hour)
	weekly := []usage.WindowName{
		usage.WindowSevenDay, usage.WindowSevenDayOpus,
		usage.WindowSevenDayOAuthApps, usage.WindowSevenDaySonnet,
	}
	// Four weekly windows rising together: two points over the four-hour
	// measured span is half a point an hour, on each of them and on the account.
	series := weeklySeries(now, 4*time.Hour, []float64{10, 11, 12}, reset, weekly...)
	snap := &usage.Snapshot{
		SevenDay:          usage.NewWindow(pf(12), pt(reset)),
		SevenDayOpus:      usage.NewWindow(pf(12), pt(reset)),
		SevenDayOAuthApps: usage.NewWindow(pf(12), pt(reset)),
		SevenDaySonnet:    usage.NewWindow(pf(12), pt(reset)),
	}
	got := Of([]Input{{UUID: "uuid-a", Idx: 1, Snapshot: snap, Series: series}}, now)

	if !got.Weekly.Burn.Known {
		t.Fatal("the weekly burn is unmeasured; three readings over four hours clear both contribution gates")
	}
	if got.Weekly.Burn.Low != 0.5 {
		t.Fatalf("Weekly.Burn.Low = %v, want 0.5 -- four windows metering one consumption were summed into four times the burn", got.Weekly.Burn.Low)
	}
	// The account's one weekly figure is also what its room is counted from,
	// and the two have to be the same window or the burn and the points left
	// would describe different quotas.
	if got.PointsLeft != 88 {
		t.Fatalf("PointsLeft = %v, want 88 -- one window per account, not four", got.PointsLeft)
	}
	if got.PointsTotal != 100 {
		t.Fatalf("PointsTotal = %v, want 100 -- one readable account", got.PointsTotal)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("len(Rows) = %d, want 1 -- one row per account, not one per window", len(got.Rows))
	}
	if got.Rows[0].Burn.Low != 0.5 {
		t.Errorf("Rows[0].Burn.Low = %v, want 0.5 -- the row's rate must be the same figure the axis sums", got.Rows[0].Burn.Low)
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
	got := Of([]Input{{UUID: "uuid-a", Idx: 1, Snapshot: snap, Series: series}}, now)

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
		{UUID: "uuid-a", Idx: 1, Snapshot: snapshotWithWeekly(10, reset)},
		{UUID: "uuid-b", Idx: 2, Snapshot: snapshotWithWeekly(20, reset)},
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
		UUID: "uuid-a", Idx: 1,
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

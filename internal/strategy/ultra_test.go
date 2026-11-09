package strategy

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// weekOnly is a verified Ultra seat: five_hour reads null, so the week is all it
// has.
func weekOnly(uuid string, pct float64, resetsIn time.Duration) Candidate {
	return polled(sub(uuid, &usage.Snapshot{SevenDay: win(pct, resetsIn)}), 180*time.Second)
}

// The case that stops a session: the live seat's week ends first, so the order
// would keep it first right up to the wall. Its last point is what moves the
// session, and the reason says so.
func TestALastWeeklyPointHandsOffToAWeeklyOnlySeatWithRoom(t *testing.T) {
	pool := []Candidate{weekOnly("a-live", 99, 31*time.Hour), weekOnly("b-other", 80, 120*time.Hour)}
	p := Decide(pool, hoverPreemptOpts(), Config{}, NewState(), "a-live")
	want(t, p, ActionSwitch, ReasonLastPoint, "b-other")
	if p.Departure != (Departure{From: "a-live", Closing: usage.WindowSevenDay}) {
		t.Errorf("Departure = %+v, want a-live left at its week", p.Departure)
	}
}

// And it does not come back, which is what a rate-based rule could not promise:
// nothing is spending the account it left, so it would read as idle and first.
func TestALastWeeklyPointDoesNotTakeTheSessionBack(t *testing.T) {
	pool := []Candidate{weekOnly("a-left", 99, 31*time.Hour), weekOnly("b-live", 80, 120*time.Hour)}
	p := Decide(pool, hoverPreemptOpts(), Config{}, NewState(), "b-live")
	if p.Action == ActionSwitch {
		t.Fatalf("moved back onto the last point: %s to %q", p.Reason, p.Target.UUID)
	}
}

// Two points left is not a limit. This is the line that keeps the rule from
// buying its one point of safety with quota the session could still spend.
func TestTwoPointsLeftIsNotALastPoint(t *testing.T) {
	pool := []Candidate{weekOnly("a-live", 98, 31*time.Hour), weekOnly("b-other", 80, 120*time.Hour)}
	p := Decide(pool, hoverPreemptOpts(), Config{}, NewState(), "a-live")
	if p.Action == ActionSwitch {
		t.Fatalf("left an account holding two points: %s to %q", p.Reason, p.Target.UUID)
	}
}

// A threshold a user typed is where they said to stop, and it is theirs to set to
// 100. The rule belongs to hover's hand-off line, which nobody typed.
func TestALastWeeklyPointIsHoversRuleAlone(t *testing.T) {
	r := Rank([]Candidate{weekOnly("a", 99, 31*time.Hour)}, opts())
	if r.Order[0].LastPoint {
		t.Error("flagged a last point outside hover")
	}
	h := Rank([]Candidate{weekOnly("a", 99, 31*time.Hour)}, hoverOpts())
	if !h.Order[0].LastPoint {
		t.Error("did not flag a last point under hover")
	}
}

// A week averaged over quiet days says hours; the rate just measured says minutes.
func TestAMeasuredBurnSeesWhatTheWeekAverageCannot(t *testing.T) {
	live := weekOnly("a-live", 97, 31*time.Hour)
	live.BurnPerMin, live.HasBurn, live.BurnWindow = 1.0, true, usage.WindowSevenDay
	o := hoverPreemptOpts()
	th := o.thresholdsFor(live)
	if w := projectedExhaustion(live.Usage, o.Model, o.Now, 3*time.Minute, th); w != "" {
		t.Fatalf("the average already saw %s, so this fixture proves nothing about the rate", w)
	}
	if got := runsOutWithin(live, o, 3*time.Minute); got != usage.WindowSevenDay {
		t.Errorf("three points at one a minute projected %q inside three minutes, want seven_day", got)
	}
	if got := runsOutWithin(live, o, 2*time.Minute); got != "" {
		t.Errorf("three points at one a minute projected %q inside two minutes", got)
	}
}

// A rate names the window it was measured on, and only that window. A five-hour
// session's rate says nothing about how fast a week is being run out.
func TestAMeasuredBurnIsReadOnlyOnItsOwnWindow(t *testing.T) {
	c := polled(sub("a", &usage.Snapshot{FiveHour: win(10, time.Hour), SevenDay: win(97, 31*time.Hour)}), 180*time.Second)
	c.BurnPerMin, c.HasBurn, c.BurnWindow = 5.4, true, usage.WindowFiveHour
	o := hoverPreemptOpts()
	if measuredExhaustion(c, o.Model, 3*time.Minute, o.thresholdsFor(c)) != "" {
		t.Error("read a five-hour rate against the week it was not measured on")
	}
}

// A rate nobody can attribute to a window projects nothing, which is how a reading
// cached before the name existed has to be read here.
func TestAnUnnamedBurnProjectsNothing(t *testing.T) {
	c := weekOnly("a", 99, 31*time.Hour)
	c.BurnPerMin, c.HasBurn = 50, true
	o := hoverPreemptOpts()
	if measuredExhaustion(c, o.Model, time.Hour, o.thresholdsFor(c)) != "" {
		t.Error("projected from a rate nobody can attribute to a window")
	}
}

// The unit error a mixed fleet makes possible: a Max seat's five-hour session
// measures five points a minute, and an Ultra seat holding thirty points of a WEEK
// has hours of that work in it, not six minutes.
func TestAFiveHourRateDoesNotRefuseAWeekOnlyTarget(t *testing.T) {
	five := polled(sub("a-live", &usage.Snapshot{FiveHour: win(99, 2*time.Hour), SevenDay: win(50, 100*time.Hour)}), 1800*time.Second)
	five.BurnPerMin, five.HasBurn, five.BurnWindow = 5.4, true, usage.WindowFiveHour
	week := polled(sub("b-week", &usage.Snapshot{SevenDay: win(70, 100*time.Hour)}), 1800*time.Second)
	week.HasBurn, week.BurnWindow = true, usage.WindowSevenDay
	p := Decide([]Candidate{five, week}, hoverPreemptOpts(), Config{}, NewState(), "a-live")
	if p.Action != ActionSwitch || p.Target.UUID != "b-week" {
		t.Fatalf("%s (%s) to %q, want a switch to the week-only seat", p.Action, p.Reason, p.Target.UUID)
	}
}

// Same-length windows do share a unit, so the rule keeps its bite there, and a
// rate cached before its window was recorded reads as it always did.
func TestASessionRateAppliesOnlyToWindowsOfItsOwnLength(t *testing.T) {
	b := sessionBurn{perMin: 1, has: true, window: usage.WindowSevenDay}
	if perMin, ok := b.onWindow(usage.WindowSevenDayOpus); !ok || perMin != 1 {
		t.Errorf("another week: %v %v", perMin, ok)
	}
	if _, ok := b.onWindow(usage.WindowFiveHour); ok {
		t.Error("a weekly rate applied to a five-hour window")
	}
	if perMin, ok := (sessionBurn{perMin: 2, has: true}).onWindow(usage.WindowFiveHour); !ok || perMin != 2 {
		t.Errorf("an unnamed rate: %v %v", perMin, ok)
	}
}

// Past the number a user typed is not out of quota. Under hover, with no credit
// pool to open, the live seat with the most room is simply staying put: "blocked"
// is the exit `ccdad auto --once` alerts on.
func TestHoverDoesNotCallAFleetPastEightyBlocked(t *testing.T) {
	pool := []Candidate{weekOnly("a-live", 90, 3*time.Hour), weekOnly("b-other", 85, 100*time.Hour)}
	p := Decide(pool, hoverPreemptOpts(), Config{}, NewState(), "a-live")
	if p.Action != ActionStay || p.Reason != ReasonAlreadyBest {
		t.Fatalf("%s (%s), want a plain stay", p.Action, p.Reason)
	}
}

// The other side of that line: an empty fleet really is blocked.
func TestHoverStillCallsAnEmptyFleetBlocked(t *testing.T) {
	pool := []Candidate{weekOnly("a-live", 100, 3*time.Hour), weekOnly("b-other", 100, 100*time.Hour)}
	p := Decide(pool, hoverPreemptOpts(), Config{}, NewState(), "a-live")
	if p.Action != ActionBlocked {
		t.Fatalf("%s (%s), want blocked", p.Action, p.Reason)
	}
}

// A null five_hour starts no warm-up: there is no clock to start.
func TestAnAbsentFiveHourWindowIsNotCold(t *testing.T) {
	s := &usage.Snapshot{SevenDay: win(60, 31*time.Hour)}
	if w, _, ok := ColdWindow(s, "", hoverOpts().Thresholds(), now); ok {
		t.Errorf("cold on %s", w)
	}
}

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

// The case that stops a session: the live seat is a day from its reset, so hover
// reads it as generously on pace, and the only other seat is ahead of its own
// week. Slack prefers the live one right up to the wall.
func TestALastWeeklyPointHandsOffToASeatAheadOfItsPace(t *testing.T) {
	pool := []Candidate{weekOnly("a-live", 99, 31*time.Hour), weekOnly("b-other", 80, 120*time.Hour)}
	p := Decide(pool, hoverPreemptOpts(), Config{}, NewState(), "a-live")
	if p.Action != ActionSwitch || p.Target.UUID != "b-other" {
		t.Fatalf("at 99%% of its week: %s (%s) to %q, want a switch to b-other", p.Action, p.Reason, p.Target.UUID)
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

// Two points left is still pace's to judge. This is the line that keeps the rule
// from buying its one point of safety with quota the other purpose wants spent.
func TestTwoPointsLeftIsNotALastPoint(t *testing.T) {
	pool := []Candidate{weekOnly("a-live", 98, 31*time.Hour), weekOnly("b-other", 80, 120*time.Hour)}
	p := Decide(pool, hoverPreemptOpts(), Config{}, NewState(), "a-live")
	if p.Action == ActionSwitch {
		t.Fatalf("left an account holding two points: %s to %q", p.Reason, p.Target.UUID)
	}
}

// A threshold a user typed is where they said to stop, and it is theirs to set to
// 100. The rule belongs to hover's pace target, which nobody typed.
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

// The recovery order is the other ordering hover can land in: every seat past its
// pace. It sorts on who comes back soonest, so without its own tier the seat on its
// last point would lead for having the nearer reset.
func TestALastWeeklyPointGoesBehindARoomierSeatInTheRecoveryOrderToo(t *testing.T) {
	pool := []Candidate{weekOnly("a-live", 99, 120*time.Hour), weekOnly("b-other", 90, 130*time.Hour)}
	r := Rank(pool, hoverPreemptOpts())
	if r.Mode != ModeRecovery {
		t.Fatalf("mode = %s; the fixture needs every seat past its pace", r.Mode)
	}
	if r.Order[0].UUID != "b-other" {
		t.Errorf("order = %v, want the seat with room first", order(r))
	}
}

// A week averaged over quiet days says hours; the rate just measured says minutes.
func TestAMeasuredBurnSeesWhatTheWeekAverageCannot(t *testing.T) {
	live := weekOnly("a-live", 97, 31*time.Hour)
	live.BurnPerMin, live.HasBurn, live.BurnWindow = 1.0, true, usage.WindowSevenDay
	o := hoverPreemptOpts()
	th := o.thresholdsFor(live)
	if projectedExhaustion(live.Usage, o.Model, o.Now, 3*time.Minute, th) {
		t.Fatal("the average already saw it, so this fixture proves nothing about the rate")
	}
	if !runsOutWithin(live, o, 3*time.Minute) {
		t.Error("three points at one a minute did not project inside three minutes")
	}
	if runsOutWithin(live, o, 2*time.Minute) {
		t.Error("three points at one a minute projected inside two minutes")
	}
}

// A rate names the window it was measured on, and only that window. A five-hour
// session's rate says nothing about how fast a week is being run out.
func TestAMeasuredBurnIsReadOnlyOnItsOwnWindow(t *testing.T) {
	c := polled(sub("a", &usage.Snapshot{FiveHour: win(10, time.Hour), SevenDay: win(97, 31*time.Hour)}), 180*time.Second)
	c.BurnPerMin, c.HasBurn, c.BurnWindow = 5.4, true, usage.WindowFiveHour
	o := hoverPreemptOpts()
	if measuredExhaustion(c, o.Model, 3*time.Minute, o.thresholdsFor(c)) {
		t.Error("read a five-hour rate against the week it was not measured on")
	}
}

// A rate nobody can attribute to a window projects nothing, which is how a reading
// cached before the name existed has to be read here.
func TestAnUnnamedBurnProjectsNothing(t *testing.T) {
	c := weekOnly("a", 99, 31*time.Hour)
	c.BurnPerMin, c.HasBurn = 50, true
	o := hoverPreemptOpts()
	if measuredExhaustion(c, o.Model, time.Hour, o.thresholdsFor(c)) {
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

// The licence floor is the other reader of the session's rate. Five points a
// minute of a five-hour window must not read as a week being emptied in ten
// minutes, or that week's perishable quota loses its widening.
func TestTheLicenceFloorDoesNotPriceAWeekInFiveHourPoints(t *testing.T) {
	s := &usage.Snapshot{SevenDay: win(95, 31*time.Hour)}
	th := opts().Thresholds()
	five := sessionBurn{perMin: 5.4, has: true, window: usage.WindowFiveHour}
	if !absorbsACooldown(s, "", th, five) {
		t.Error("five points of a week refused on a five-hour rate")
	}
	week := sessionBurn{perMin: 5.4, has: true, window: usage.WindowSevenDay}
	if absorbsACooldown(s, "", th, week) {
		t.Error("a weekly rate that really would empty the week was waved through")
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

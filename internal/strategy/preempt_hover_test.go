package strategy

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/pollpolicy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// The live account here is three hours into a five-hour window at 88%, which
// extrapolates to its limit 1472 s out, and the blind interval is the 1800 s
// ceiling a 429 earns -- so the projection reaches it.
func hoverBurnPool() []Candidate {
	pool := []Candidate{
		// 88% three hours into a five-hour window: its limit is 1472 s out, and
		// the 1800 s blind interval a 429 earns reaches it.
		polled(burning("a-live", 88), 1800*time.Second),
		// 79% of the same window: 21 points from its limit, which is 48 minutes
		// away and well outside the horizon.
		polled(burning("b-room", 79), 1800*time.Second),
	}
	// Four weekly-only seats behind it, so the five-hour account is chosen by the
	// order's first rule and not by being the only roomy account in the pool.
	for i := 0; i < 4; i++ {
		c := sub(string(rune('c'+i))+"-spent", &usage.Snapshot{
			SevenDay: elapsedWindow(7*24*time.Hour, 0.20, 85),
		})
		pool = append(pool, polled(c, 1800*time.Second))
	}
	return pool
}

func hoverPreemptOpts() Options {
	o := preemptOpts()
	o.Hover = true
	return o
}

// The live five-hour account would be FIRST in hover's order on its reading
// alone, and the pre-emptive walk ends at the live account. What lets the walk
// reach the account behind it is that a closing account is on the limit rung.
func TestPreemptionFiresWhenTheLiveAccountIsClosingAndAnotherHasRoom(t *testing.T) {
	pool := hoverBurnPool()
	o := hoverPreemptOpts()

	res := Rank(pool, o)
	if live, _ := find(res, "a-live"); live.Closing != usage.WindowFiveHour || live.Rung() != RungAtLimit {
		t.Fatalf("a-live is closing on %q at rung %d; the fixture no longer projects the live account out",
			live.Closing, live.Rung())
	}

	p := Decide(pool, o, Config{}, NewState(), "a-live")
	want(t, p, ActionSwitch, ReasonProjectedExhaustion, "b-room")
	if p.Departure != (Departure{From: "a-live", Closing: usage.WindowFiveHour}) {
		t.Errorf("Departure = %+v, want a-live left on its five-hour window", p.Departure)
	}
}

// It still refuses the two moves that would waste the switch.
func TestPreemptionWillNotRunToAnEmptyAccount(t *testing.T) {
	pool := []Candidate{
		polled(burning("a-live", 88), 1800*time.Second),
		polled(sub("b-empty", &usage.Snapshot{FiveHour: win(100, 2*time.Hour)}), 1800*time.Second),
	}
	p := Decide(pool, hoverPreemptOpts(), Config{}, NewState(), "a-live")
	if p.Action == ActionSwitch && p.Target.UUID == "b-empty" {
		t.Fatal("pre-empted onto an account with nothing left")
	}
}

func TestPreemptionWillNotRunToAnAccountAboutToRunOutItself(t *testing.T) {
	// b burns harder than a: 95% three hours into the same five-hour window.
	pool := []Candidate{
		polled(burning("a-live", 88), 1800*time.Second),
		polled(burning("b-worse", 95), 1800*time.Second),
	}
	p := Decide(pool, hoverPreemptOpts(), Config{}, NewState(), "a-live")
	if p.Action == ActionSwitch && p.Target.UUID == "b-worse" {
		t.Fatal("pre-empted onto an account projected to run out inside the same horizon")
	}
}

// A throttled poller is a preference, not a filter: it means the reading cannot
// be refreshed, never that the account is spent.
func TestPreemptionPrefersACandidateItCanStillSee(t *testing.T) {
	mk := func(uuid string, throttled bool) Candidate {
		c := polled(burning(uuid, 20), 1800*time.Second)
		if throttled {
			c.LastRateLimited = now.Add(-pollpolicy.Recent429Window / 2)
		}
		return c
	}
	// The throttled one ranks FIRST -- more room -- so only the preference can
	// move the answer.
	pool := []Candidate{
		polled(burning("a-live", 88), 1800*time.Second),
		mk("b-throttled", true),
		polled(burning("c-visible", 30), 1800*time.Second),
	}
	p := Decide(pool, hoverPreemptOpts(), Config{}, NewState(), "a-live")
	want(t, p, ActionSwitch, ReasonProjectedExhaustion, "c-visible")

	// And with nothing cleaner available it takes the throttled one rather than
	// leaving the session on an account that is about to stop working.
	only := []Candidate{
		polled(burning("a-live", 88), 1800*time.Second),
		mk("b-throttled", true),
	}
	p = Decide(only, hoverPreemptOpts(), Config{}, NewState(), "a-live")
	want(t, p, ActionSwitch, ReasonProjectedExhaustion, "b-throttled")
}

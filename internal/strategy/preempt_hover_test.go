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

	p := Decide(pool, o, Config{}, NewState(), "a-live")
	if live, _ := find(p.Result, "a-live"); live.Closing != usage.WindowFiveHour || live.Rung() != RungAtLimit {
		t.Fatalf("a-live is closing on %q at rung %d; the fixture no longer projects the live account out",
			live.Closing, live.Rung())
	}
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

// A candidate the work running now would run out before its first live reading
// is no place to pre-empt to: the session is cut off there instead, a switch
// later. The question is asked at the WORK's rate -- the one measured live --
// because the candidate's own reading says nothing about what the session would
// do to it. The live account is stopping (twenty-four minutes against a
// thirty-minute blind), and the candidate's ten minutes do not outlast it by a
// cooldown either, so nothing can serve yet; a last resort comes at the live
// account's projected stop.
func TestPreemptionWillNotRunToAnAccountTheWorkWouldRunOut(t *testing.T) {
	live := polled(burning("a-live", 88), 1800*time.Second)
	live.BurnPerMin, live.HasBurn, live.BurnWindow = 0.5, true, usage.WindowFiveHour
	live.LiveBurns = []usage.LiveBurn{{PerMin: 0.5, Window: usage.WindowFiveHour, From: now.Add(-30 * time.Minute), At: now}}
	// Five points of the same window: ten minutes at the work's rate, against a
	// horizon of half an hour and more.
	pool := []Candidate{live, polled(burning("b-worse", 95), 1800*time.Second)}

	p := Decide(pool, hoverPreemptOpts(), Config{}, NewState(), "a-live")
	if r, _ := find(p.Result, "b-worse"); r.Carries || r.Closing != "" {
		t.Fatalf("b-worse carries %v, closing on %q; the work's rate runs it out before its first live reading, "+
			"and an account the session is not on is never closing", r.Carries, r.Closing)
	}
	if p.Action == ActionSwitch {
		t.Fatalf("pre-empted onto %s, which the work would run out inside the same horizon", p.Target.UUID)
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

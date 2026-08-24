package strategy

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/pollpolicy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// Pre-emption used to require a candidate with POSITIVE slack, which under hover
// is a condition an ordinary pool never meets: hover's threshold is a pace
// target, so every account ahead of its pace is negative and the rule that
// exists to move a session before its account hits a hard limit could not fire
// at all.
//
// The live account here is three hours into a five-hour window at 88%, which
// extrapolates to its limit 1472 s out, and the blind interval is the 1800 s
// ceiling a 429 earns -- so the projection reaches it. Every account is negative
// on the hover axis.
func hoverBurnPool() []Candidate {
	pool := []Candidate{
		// 88% three hours into a five-hour window: its limit is 1472 s out, and
		// the 1800 s blind interval a 429 earns reaches it.
		polled(burning("a-live", 88), 1800*time.Second),
		// 79% of the same window. Two points past the pace hover derives for it,
		// so it is NEGATIVE like everything else here -- and 21 points from its
		// limit, which is 48 minutes away and well outside the horizon.
		polled(burning("b-room", 79), 1800*time.Second),
	}
	// Four more, so the share is 100/6 rather than a figure a two-account pool
	// flatters. Weekly windows only: a five-hour window this early would give
	// them positive slack and stop the pool reproducing the dead case.
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

func TestPreemptionFiresWhenEveryAccountIsOverItsPace(t *testing.T) {
	pool := hoverBurnPool()
	o := hoverPreemptOpts()

	// Precondition: nothing has positive slack, which is what used to kill it.
	oh := o.withHover(pool)
	for _, c := range pool {
		if h := measure(c, oh).Headroom; h.Slack > 0 {
			t.Fatalf("%s has slack %+.1f; the fixture no longer reproduces the dead case", c.UUID, h.Slack)
		}
	}

	p := Decide(pool, o, Config{}, NewState(), "a-live")
	want(t, p, ActionSwitch, ReasonProjectedExhaustion, "b-room")
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

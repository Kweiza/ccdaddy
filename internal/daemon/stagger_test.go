package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// The phase is a fraction of the window and never more than half of it, so the
// clock an account loses is bounded by something a fleet can price.
func TestEveryPhaseIsInsideHalfTheWindow(t *testing.T) {
	s := rolledOver(time.Minute)
	for _, uuid := range []string{"a", "b", "c", "0d9e4e6a-1f1a-4b5e-9c3a-2f7b6a1d8e40", "", "u-1"} {
		got := warmPhase(uuid, s, usage.WindowFiveHour)
		if got < 0 || got > 150*time.Minute {
			t.Errorf("warmPhase(%q) = %v, want inside [0, 2h30m]", uuid, got)
		}
	}
}

// It is the same answer on every process. A per-process seed would re-phase the
// whole fleet on every daemon restart, which is the batch re-forming by another
// route -- and the log shows four restarts in one afternoon.
func TestThePhaseIsStableAcrossCalls(t *testing.T) {
	s := rolledOver(time.Minute)
	first := warmPhase("u-1", s, usage.WindowFiveHour)
	for i := 0; i < 100; i++ {
		if got := warmPhase("u-1", s, usage.WindowFiveHour); got != first {
			t.Fatalf("warmPhase moved from %v to %v on call %d", first, got, i)
		}
	}
}

// Different accounts land in different places. Identical phases would be no
// stagger at all, which is the state this replaces.
func TestDifferentAccountsGetDifferentPhases(t *testing.T) {
	s := rolledOver(time.Minute)
	seen := map[time.Duration]string{}
	for _, uuid := range []string{"u-1", "u-2", "u-3", "u-4", "u-5", "u-6"} {
		got := warmPhase(uuid, s, usage.WindowFiveHour)
		if other, dup := seen[got]; dup {
			t.Errorf("%s and %s share a phase of %v", uuid, other, got)
		}
		seen[got] = uuid
	}
	// And they are actually spread rather than clustered in one corner: with six
	// accounts over a 150-minute range, at least one has to land in each half.
	var low, high int
	for d := range seen {
		if d < 75*time.Minute {
			low++
		} else {
			high++
		}
	}
	if low == 0 || high == 0 {
		t.Errorf("phases = %v; every account landed in one half of the range", seen)
	}
}

// A window whose length this build cannot name gets no phase, rather than a
// guess. It is the same silence the licence floor answers a nameless scale with.
func TestAWindowWithNoNameableLengthGetsNoPhase(t *testing.T) {
	if got := warmPhase("u-1", rolledOver(time.Minute), usage.WindowName("not_a_window")); got != 0 {
		t.Errorf("warmPhase = %v for a window this build cannot measure, want 0", got)
	}
	if got := warmPhase("u-1", nil, usage.WindowFiveHour); got != 0 {
		t.Errorf("warmPhase = %v with no reading at all, want 0", got)
	}
}

// End to end: an account whose clock ran down a minute ago is NOT warmed until
// its own phase has elapsed. Without this every account in a fleet re-anchors
// within a minute of its rollover, so a fleet warmed together stays warmed
// together for as long as it exists -- which is the forty-minute band measured
// on 2026-09-05.
func TestTheWarmUpWaitsOutItsPhaseAfterTheRollover(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedLiveHolder(t, "u-live")
	seedEntry(t, "u-1", usage.Entry{
		Snapshot:  rolledOver(time.Minute),
		FetchedAt: tickEpoch.Add(-10 * time.Minute),
	})

	phase := warmPhase("u-1", rolledOver(time.Minute), usage.WindowFiveHour)
	if phase <= time.Minute {
		t.Fatalf("this fixture's phase is %v; it cannot exercise the wait", phase)
	}

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return rolledOver(time.Minute), nil
	})
	probes := stubProbe(t, e)
	tick(t, e)

	if len(*probes) != 0 {
		t.Fatalf("probes = %+v a minute after the rollover; this account's phase is %v", *probes, phase)
	}
}

// And it is a WAIT and not a refusal: once the phase has passed, the same
// account is warmed exactly as it always was.
func TestTheWarmUpRunsOnceThePhaseHasPassed(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedLiveHolder(t, "u-live")

	phase := warmPhase("u-1", rolledOver(time.Minute), usage.WindowFiveHour)
	ago := phase + time.Minute
	seedEntry(t, "u-1", usage.Entry{
		Snapshot:  rolledOver(ago),
		FetchedAt: tickEpoch.Add(-10 * time.Minute),
	})

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return rolledOver(ago), nil
	})
	probes := stubProbe(t, e)
	tick(t, e)

	if len(*probes) != 1 || (*probes)[0].uuid != "u-1" {
		t.Fatalf("probes = %+v, want one for u-1: its phase of %v elapsed %v ago",
			*probes, phase, ago-phase)
	}
}

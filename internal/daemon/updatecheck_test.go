package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// The interval is a RANGE, not a deadline. NewEngine wires math/rand's Float64
// as the jitter source, so every cadence in this process is spread, and a test
// that wants an exact instant pins the sample -- which is what makes the
// un-jittered implementation a mutation this can catch rather than one it
// cannot see.
func TestTheReleaseCheckDeadlineIsADayPlusOrMinusTenPercent(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		rnd  float64
		want time.Duration
	}{
		{"the low end of the range", 0, 24*time.Hour - 144*time.Minute},
		{"the midpoint is the interval itself", 0.5, 24 * time.Hour},
		{"the high end of the range", 1, 24*time.Hour + 144*time.Minute},
		{"a sample below the range is clamped rather than inverted", -1, 24*time.Hour - 144*time.Minute},
		{"a sample above the range is clamped", 2, 24*time.Hour + 144*time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nextUpdateCheck(now, tc.rnd)
			if want := now.Add(tc.want); !got.Equal(want) {
				t.Errorf("nextUpdateCheck(%v) = %v, want %v", tc.rnd, got, want)
			}
		})
	}
}

// A status.json restored from a backup, a machine whose clock jumped, or one
// whose time was wrong when the last daemon ran can carry a deadline years out.
// now.Before(nextAt) would then switch the feature off permanently and in
// silence, which is the worst of the three outcomes because nothing anywhere
// reports it.
func TestAnImpossibleDeadlineIsResampledRatherThanDiscarded(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	got := usableDeadline(now.Add(72*time.Hour), now, 0.5)
	if want := now.Add(24 * time.Hour); !got.Equal(want) {
		t.Errorf("usableDeadline(three days out) = %v, want a fresh %v", got, want)
	}
	// NOT the zero value. Zeroing it would make every machine with a bad clock
	// dispatch on its very next tick, which turns one machine's clock problem
	// into a fleet arriving at the origin together.
	if got.IsZero() {
		t.Error("an impossible deadline was discarded rather than replaced; every machine with a wrong clock would then check immediately")
	}

	for _, tc := range []struct {
		name      string
		published time.Time
	}{
		{"a deadline inside the window is left exactly as it was", now.Add(time.Hour)},
		{"a deadline already past is left alone so the check is due", now.Add(-time.Hour)},
		{"no deadline at all is left alone, which is what makes a fresh store check on its first tick", time.Time{}},
		{"the far edge of legitimate is kept", now.Add(updateCheckSlack)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := usableDeadline(tc.published, now, 0.5); !got.Equal(tc.published) {
				t.Errorf("usableDeadline(%v) = %v, want it untouched", tc.published, got)
			}
		})
	}
}

// stubRelease points the engine's resolver at a canned answer and counts what
// reached it. The counter is read only after e.Wait(), which the tick helper
// calls, so the race detector has a happens-before edge to work with.
func stubRelease(e *Engine, tag string, err error) *int {
	n := 0
	e.LatestRelease = func(context.Context) (string, error) {
		n++
		return tag, err
	}
	return &n
}

// releaseEngine is engineFor with a poll that always answers, so the tick has
// something ordinary to do around the release check.
func releaseEngine(t *testing.T) *Engine {
	t.Helper()
	return engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return snapshotWith(10), nil
	})
}

// A store with no recorded check dispatches on its first tick. That is one
// request shortly after a fresh install and it is deliberate: the alternative
// delays the feature's first useful answer by a day to avoid a burst that does
// not exist, because installs are not synchronised the way an outage recovery
// is.
func TestAStoreThatHasNeverCheckedAsksOnItsFirstTick(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	liveAs(t, "u-1")

	e := releaseEngine(t)
	n := stubRelease(e, "v0.7.0", nil)
	tick(t, e)

	if *n != 1 {
		t.Fatalf("the release origin was asked %d times, want 1", *n)
	}
	s := e.Snapshot()
	if !s.UpdateCheckedAt.Equal(tickEpoch) {
		t.Errorf("UpdateCheckedAt = %v, want the dispatch stamp %v", s.UpdateCheckedAt, tickEpoch)
	}
	if want := tickEpoch.Add(24 * time.Hour); !s.NextUpdateCheckAt.Equal(want) {
		t.Errorf("NextUpdateCheckAt = %v, want %v at the pinned midpoint sample", s.NextUpdateCheckAt, want)
	}
}

// The tick runs about once a second. A second one inside the window must not
// produce a second request, or the daily check is a per-second one.
func TestASecondTickInsideTheWindowDoesNotAskAgain(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	liveAs(t, "u-1")

	e := releaseEngine(t)
	n := stubRelease(e, "v0.7.0", nil)
	tick(t, e)
	tick(t, e)

	if *n != 1 {
		t.Fatalf("the release origin was asked %d times across two ticks, want 1", *n)
	}
}

// Nil is the refusing default, and it is what keeps every engine that is not a
// daemon off this request: NewEngine leaves the resolver unset, and internal/cli
// builds a real engine of its own for a refresh.
func TestAnEngineWithNoResolverNeverChecks(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	liveAs(t, "u-1")

	e := releaseEngine(t)
	if e.LatestRelease != nil {
		t.Fatal("NewEngine wired a release resolver; the only engine allowed to reach the origin is the daemon's")
	}
	tick(t, e)

	if s := e.Snapshot(); !s.NextUpdateCheckAt.IsZero() || !s.UpdateCheckedAt.IsZero() {
		t.Errorf("an engine with no resolver scheduled a check anyway: %+v", s)
	}
}

// The key is read from the config the tick already has in hand, so switching it
// takes effect on the NEXT TICK rather than at the next daemon start. A machine
// that has just been told it may not call out should stop calling out.
func TestTheUpdateCheckKeyStopsTheRequestAndFlippingItTakesEffectNextTick(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	liveAs(t, "u-1")
	writeConfig(t, "update_check = false\n")

	e := releaseEngine(t)
	n := stubRelease(e, "v0.7.0", nil)
	tick(t, e)
	if *n != 0 {
		t.Fatalf("the release origin was asked %d times with update_check off, want 0", *n)
	}
	if s := e.Snapshot(); !s.NextUpdateCheckAt.IsZero() {
		t.Errorf("a check was scheduled with the key off: %v", s.NextUpdateCheckAt)
	}

	writeConfig(t, "update_check = true\n")
	tick(t, e)
	if *n != 1 {
		t.Fatalf("the release origin was asked %d times after the key was turned back on, want 1", *n)
	}
}

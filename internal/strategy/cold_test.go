package strategy

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// noReset is a window that named a percentage and no rollover at all.
func noReset(pct float64) usage.Window { return usage.NewWindow(&pct, nil) }

// The two ways a clock can be stopped, and the one shape that looks like a third
// and is not.
//
// The rollover arm is the one this file exists for. Before it, the daemon could
// only see a stopped clock once a poll had written {0, null} over the expired
// reading — so it waited for one poll interval to DISCOVER the state and another
// for a tick to act on it, and the window stayed cold across both.
func TestColdWindowNamesBothWaysAClockCanBeStopped(t *testing.T) {
	t.Run("never spent: nothing used and no rollover", func(t *testing.T) {
		w, at, ok := ColdWindow(snap(noReset(0), win(40, 3*24*time.Hour)), "", Thresholds{Default: 80}, now)
		if !ok || w != usage.WindowFiveHour {
			t.Fatalf("ColdWindow = %q, %v; want the five-hour window", w, ok)
		}
		if !at.IsZero() {
			t.Errorf("rollover = %s for a window that never had one; MayProbe reads that as an "+
				"instant to compare against", at)
		}
	})

	t.Run("rolled over: the reading's own rollover has passed", func(t *testing.T) {
		w, at, ok := ColdWindow(snap(win(38, -time.Minute), win(40, 3*24*time.Hour)), "", Thresholds{Default: 80}, now)
		if !ok || w != usage.WindowFiveHour {
			t.Fatalf("ColdWindow = %q, %v; want the five-hour window", w, ok)
		}
		if !at.Equal(now.Add(-time.Minute)) {
			t.Errorf("rollover = %s, want the instant the reading named — one warm-up per rollover "+
				"is spelled against it", at)
		}
	})

	t.Run("a rollover still in the future is a running clock", func(t *testing.T) {
		if w, _, ok := ColdWindow(snap(win(38, time.Minute), win(40, 3*24*time.Hour)), "", Thresholds{Default: 80}, now); ok {
			t.Errorf("ColdWindow = %q for a clock with a minute left on it", w)
		}
	})

	t.Run("spent with no rollover is an unreadable reset, not a stopped clock", func(t *testing.T) {
		// 40% of the window is gone and the endpoint still named no time, so the
		// time is one this build could not parse. Another turn buys the same
		// unreadable field back, once per rung, forever.
		if w, _, ok := ColdWindow(snap(noReset(40), win(40, 3*24*time.Hour)), "", Thresholds{Default: 80}, now); ok {
			t.Errorf("ColdWindow = %q for a window 40%% spent with no reset", w)
		}
	})

	t.Run("an unreadable window is nothing to warm", func(t *testing.T) {
		if w, _, ok := ColdWindow(snap(unread(), unread()), "", Thresholds{Default: 80}, now); ok {
			t.Errorf("ColdWindow = %q for a reading that carries no percentages", w)
		}
	})

	t.Run("no reading at all", func(t *testing.T) {
		if _, _, ok := ColdWindow(nil, "", Thresholds{Default: 80}, now); ok {
			t.Error("a nil reading named a window to warm")
		}
	})
}

// Wire order decides, and the five-hour window comes first. It is also the one
// worth having: a turn spent against it is the one that shortens the next
// lockout, where a weekly cap's clock is days long and rolls over on its own.
func TestColdWindowPrefersTheFiveHourWindowWhenBothAreStopped(t *testing.T) {
	s := snap(win(38, -time.Minute), noReset(0))
	w, _, ok := ColdWindow(s, "", Thresholds{Default: 80}, now)
	if !ok || w != usage.WindowFiveHour {
		t.Fatalf("ColdWindow = %q, %v; want five_hour ahead of the weekly", w, ok)
	}
}

// The clamp aims at the EARLIEST FUTURE rollover across the candidate set, and
// both halves of that are load-bearing. Taking the binding window would aim days
// out — the weekly binds on every account of a busy fleet — and taking a
// rollover already past would aim into the past.
func TestNextResetAmongTakesTheEarliestFutureRollover(t *testing.T) {
	t.Run("the nearer of two futures, not the binding one", func(t *testing.T) {
		// The weekly is 40% used against a default threshold of 80, so it binds
		// nothing here; what matters is that its reset is three days out while
		// the five-hour window rolls over in twenty minutes.
		at, ok := NextResetAmong(snap(win(90, 20*time.Minute), win(40, 3*24*time.Hour)), "", Thresholds{Default: 80}, now)
		if !ok || !at.Equal(now.Add(20*time.Minute)) {
			t.Errorf("NextResetAmong = %s, %v; want the twenty-minute one", at, ok)
		}
	})

	t.Run("a rollover already past is not a schedule", func(t *testing.T) {
		at, ok := NextResetAmong(snap(win(38, -time.Minute), win(40, 3*24*time.Hour)), "", Thresholds{Default: 80}, now)
		if !ok || !at.Equal(now.Add(3*24*time.Hour)) {
			t.Errorf("NextResetAmong = %s, %v; want the weekly, since the five-hour one has "+
				"already happened", at, ok)
		}
	})

	t.Run("nothing to aim at", func(t *testing.T) {
		if at, ok := NextResetAmong(snap(noReset(0), noReset(0)), "", Thresholds{Default: 80}, now); ok {
			t.Errorf("NextResetAmong = %s, true; both clocks are stopped and neither has a rollover", at)
		}
	})
}

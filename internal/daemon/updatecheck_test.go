package daemon

import (
	"testing"
	"time"
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

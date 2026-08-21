package strategy

import (
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// Headroom is how much of an account's BINDING window is left, as a percent.
//
// It is deliberately not one window's number. An account carries a five-hour
// window and several weekly ones, and the one that binds is whichever has least
// left: ranking on five_hour alone hands work to an account whose weekly Opus
// quota is gone, and it hits a hard limit one prompt later.
type Headroom struct {
	// Pct is 100 minus the binding window's utilization. It can go negative,
	// and is kept that way: how far past its limit an account already is still
	// orders it against other spent accounts.
	Pct float64
	// Known is false when no window reported a utilization. An account that
	// could not be read is NOT an empty one — spec §7.2, and the exact bug that
	// parked cswap's engine permanently.
	Known bool
	// Binding names the window Pct came from.
	Binding usage.WindowName
}

// HeadroomOf finds the binding window.
//
// It ranges over Snapshot.RateLimitWindows, which excludes cinder_cove: that is
// a one-time credit grant whose resets_at is an expiry, so a spent one would
// otherwise read as a permanently exhausted account. seven_day_oauth_apps IS
// included even though the brief's list of four leaves it out — Claude Code is
// itself an OAuth app, nothing in the bundle says the window means anything
// else, and the two mistakes are not symmetric: counting a window that does not
// bind costs one conservative switch, while missing one that does bind hands
// work to an account that is already out of quota.
//
// Windows tie on the first in the schema's own order, so the answer does not
// depend on map iteration.
func HeadroomOf(s *usage.Snapshot) Headroom {
	if s == nil {
		return Headroom{}
	}
	out := Headroom{}
	for _, w := range s.RateLimitWindows() {
		pct, ok := w.Percent()
		if !ok {
			continue
		}
		left := 100 - pct
		if !out.Known || left < out.Pct {
			out = Headroom{Pct: left, Known: true, Binding: w.Name}
		}
	}
	return out
}

// recoveryOf is when the binding window rolls over: the moment the account stops
// being the one that is spent. A window that reported no reset has no recovery,
// which is not the same as recovering now.
func recoveryOf(s *usage.Snapshot, binding usage.WindowName) (t timeValue) {
	if s == nil {
		return t
	}
	for _, w := range s.RateLimitWindows() {
		if w.Name != binding {
			continue
		}
		if at, ok := w.Reset(); ok {
			return timeValue{at: at, ok: true}
		}
	}
	return t
}

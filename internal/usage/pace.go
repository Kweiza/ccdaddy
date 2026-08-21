package usage

import "time"

// Spec §7.5. Weekly windows carry an expected-versus-actual reading once the
// week is about a day old, and a linear projection that stays --json-only.
//
// The suppression is the load-bearing part. In the first hours after a reset
// elapsed time is tiny, so almost any usage divides out as "far ahead" and a
// dashboard built on it cries wolf every Monday.

// paceMinElapsed is how much of a window must have run before its pace means
// anything.
const paceMinElapsed = 24 * time.Hour

// weeklyWindow is how long a seven-day window runs, from the bundle's own table
// (`windowSeconds: 604800`; five_hour is 18000 there, and cinder_cove has no
// length at all because its resets_at is an expiry).
const weeklyWindow = 7 * 24 * time.Hour

// isWeekly reports whether a window is one of the seven-day family, which is the
// only family §7.5 paces. A five-hour window is shorter than the suppression
// period, so pacing it would produce nothing but suppressed readings.
func isWeekly(n WindowName) bool {
	switch n {
	case WindowSevenDay, WindowSevenDayOAuthApps, WindowSevenDayOpus, WindowSevenDaySonnet:
		return true
	}
	return false
}

// PaceReason says why a pace reading is or is not available. Every non-OK value
// means "say nothing", never "say zero".
type PaceReason uint8

const (
	PaceOK PaceReason = iota
	// PaceNoUtilization: the window reported no utilization to compare.
	PaceNoUtilization
	// PaceNoReset: no resets_at, so there is no window start to measure from.
	// Taking the zero time here would put the start in 1970 and report
	// effectively infinite overage on every account.
	PaceNoReset
	// PaceNotWeekly: pace is a weekly-window measure.
	PaceNotWeekly
	// PaceTooEarly: less than a day since the reset.
	PaceTooEarly
	// PaceWindowNotStarted: the reset is further out than the window is long,
	// so either the local clock or the endpoint's is wrong. There is no elapsed
	// time to divide by.
	PaceWindowNotStarted
)

func (r PaceReason) String() string {
	switch r {
	case PaceOK:
		return "ok"
	case PaceNoUtilization:
		return "no utilization"
	case PaceNoReset:
		return "no reset timestamp"
	case PaceNotWeekly:
		return "not a weekly window"
	case PaceTooEarly:
		return "less than a day since the reset"
	case PaceWindowNotStarted:
		return "the window has not started"
	}
	return "unknown"
}

// Pace is how one weekly window's consumption compares with the time elapsed in
// it.
//
// It deliberately carries no projection fields. §7.5 keeps projectedExhaustionAt
// and willLastToReset out of every human-facing view, and the way to make that
// stick is to keep them off the struct a renderer ranges over — see Projection.
type Pace struct {
	Reason PaceReason
	// ExpectedPct is the share of the window that has elapsed, as a percent.
	ExpectedPct float64
	// ActualPct is the window's reported utilization, as a percent.
	ActualPct float64
	// AheadOfPace is ActualPct > ExpectedPct.
	AheadOfPace bool

	exhaustionAt    time.Time
	willLastToReset bool
	hasProjection   bool
}

// OK reports whether this reading says anything at all.
func (p Pace) OK() bool { return p.Reason == PaceOK }

// Projection is a linear extrapolation of the current burn.
//
// It is --json-only by spec §7.5: real usage is bursty, and a straight line
// through it is too rough to state as fact in a table a human reads. It is
// reachable only through Pace.Projection, so surfacing it in `list` or `status`
// has to be a deliberate act.
type Projection struct {
	// ExhaustionAt is when the window hits 100% at the current rate.
	ExhaustionAt time.Time
	// WillLastToReset is whether that lands at or after the reset.
	WillLastToReset bool
}

// Projection is the --json-only extrapolation, and whether there was one to
// make. It reports nothing when pace itself is suppressed: the first day's
// numbers are exactly the ones too noisy to extrapolate from.
func (p Pace) Projection() (Projection, bool) {
	if !p.hasProjection {
		return Projection{}, false
	}
	return Projection{ExhaustionAt: p.exhaustionAt, WillLastToReset: p.willLastToReset}, true
}

// PaceOf measures one window against the clock.
func PaceOf(name WindowName, w Window, now time.Time) Pace {
	if !isWeekly(name) {
		return Pace{Reason: PaceNotWeekly}
	}
	actual, ok := w.Percent()
	if !ok {
		return Pace{Reason: PaceNoUtilization}
	}
	reset, ok := w.Reset()
	if !ok {
		return Pace{Reason: PaceNoReset}
	}

	start := reset.Add(-weeklyWindow)
	elapsed := now.Sub(start)
	if elapsed < 0 {
		return Pace{Reason: PaceWindowNotStarted}
	}
	if elapsed < paceMinElapsed {
		return Pace{Reason: PaceTooEarly}
	}
	// A reset already in the past is a window the endpoint has not rolled over
	// yet. Capping keeps expected at a full quota rather than running past it.
	if elapsed > weeklyWindow {
		elapsed = weeklyWindow
	}

	expected := float64(elapsed) / float64(weeklyWindow) * 100
	p := Pace{
		Reason:      PaceOK,
		ExpectedPct: expected,
		ActualPct:   actual,
		AheadOfPace: actual > expected,
	}

	// The projection needs a rate, and a rate needs something spent. An account
	// that has used nothing has no burn to extend.
	if actual > 0 {
		perSecond := actual / elapsed.Seconds()
		remaining := 100 - actual
		if remaining < 0 {
			remaining = 0
		}
		p.exhaustionAt = now.Add(time.Duration(remaining/perSecond) * time.Second)
		p.willLastToReset = !p.exhaustionAt.Before(reset)
		p.hasProjection = true
	}
	return p
}

// Pace measures every weekly window the response actually carried. Windows that
// were absent, or that have nothing to say, are left out rather than reported as
// a zero reading.
func (s *Snapshot) Pace(now time.Time) map[WindowName]Pace {
	out := map[WindowName]Pace{}
	for _, nw := range s.RateLimitWindows() {
		// A window the response never carried has no utilization, so PaceOf
		// reports PaceNoUtilization and this filter drops it — no separate
		// presence check is needed, and one would be unreachable.
		if p := PaceOf(nw.Name, nw.Window, now); p.OK() {
			out[nw.Name] = p
		}
	}
	return out
}

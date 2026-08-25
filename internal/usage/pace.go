package usage

import (
	"math"
	"time"
)

// Every window ccdad knows a length for carries an expected-versus-actual
// reading once enough of that window has run, plus a linear projection that
// stays out of the human views.
//
// The suppression is the load-bearing part. In the first hours after a reset
// elapsed time is tiny, so almost any usage divides out as "far ahead" and a
// dashboard built on it cries wolf every Monday.

// paceSuppressionDivisor is the share of a window that has to run before its
// pace means anything: one seventh of the window's own length.
//
// A seventh is not a new number. It reproduces the 24 hours a seven-day window
// was held quiet for exactly (604800/7 = 86400), and applied to the five-hour
// window it gives 2571 s — 42 minutes and 51 seconds.
//
// Those 43 minutes cannot hide the case this measure exists for. Reaching 95%
// of a five-hour quota inside the first seventh of it means burning at more than
// six times the rate the window allows, and an account doing that is already
// past every threshold ccdad ships with: the ranking has moved off it on the
// ordinary spent-account path long before an extrapolation would have had
// anything left to add.
const paceSuppressionDivisor = 7

// maxDurationSeconds is the largest whole number of seconds a time.Duration can
// carry, which is what a projection has to be held under before it is converted.
// It is written as the division the conversion inverts, so the bound cannot
// drift from the arithmetic it bounds.
const maxDurationSeconds = int64(math.MaxInt64) / int64(time.Second)

// windowLength is how long a window runs, from the bundle's own windowSeconds
// table: 18000 for five_hour and 604800 for every seven-day window. A scoped
// window is weekly by construction, because ScopedWindows admits a limits[]
// entry only when its kind is weekly_scoped.
//
// cinder_cove is absent, and that is why this is a lookup rather than a
// subtraction: it has no length in that table because its resets_at is an EXPIRY
// rather than a rollover, so measuring elapsed time against it would pace a
// countdown to deletion.
//
// This is deliberately not IsWeekly with a length bolted onto it. IsWeekly
// answers a different question — does this quota expire weekly — and the ranking
// asks it of the same names to find the perishable quota consume-first spends
// first. Folding five_hour into that would send consume-first chasing a
// five-hour rollover as though it were a week's quota about to be lost.
func windowLength(n WindowName) (time.Duration, bool) {
	switch n {
	case WindowFiveHour:
		return 5 * time.Hour, true
	case WindowSevenDay, WindowSevenDayOAuthApps, WindowSevenDayOpus, WindowSevenDaySonnet:
		return 7 * 24 * time.Hour, true
	}
	if n.Scoped() {
		return 7 * 24 * time.Hour, true
	}
	return 0, false
}

// WindowLength is how long a window runs before it rolls over, and whether this
// release knows. It is the ONLY source of that figure outside this package: a
// caller that re-declares 18000 and 604800 owns a second copy of a rule that
// lives here, and the two copies drift the first time a window's length moves.
//
// A wrapper rather than a rename, so the export is purely additive: windowLength
// keeps both of its in-package call sites -- PaceOf here and ExpectedPct in
// expected.go -- and the long note on it, which is about why cinder_cove is
// absent from the table and why this is not IsWeekly with a length bolted on,
// stays where it explains something rather than becoming the public contract.
//
// cinder_cove still answers false, and that is the load-bearing half for an
// outside caller. Its resets_at is an expiry rather than a rollover, so a caller
// handed a plausible length for it would invent an endless series of grants that
// never arrive.
func WindowLength(n WindowName) (time.Duration, bool) { return windowLength(n) }

// IsWeekly reports whether a window's quota is the seven-day kind. It is a
// question about PERISHABILITY, not about length: the ranking asks it to find
// the quota consume-first should spend before it expires, and a five-hour
// rollover is not quota anyone can lose. Pace is no longer scoped to it —
// windowLength is the lookup that decides what can be paced.
//
// Every SCOPED window is weekly by construction: ScopedWindows admits a limits[]
// entry only when its kind is weekly_scoped. It is exported because the ranking
// asks the same question of the same names, and two copies of this list would
// drift.
func IsWeekly(n WindowName) bool {
	switch n {
	case WindowSevenDay, WindowSevenDayOAuthApps, WindowSevenDayOpus, WindowSevenDaySonnet:
		return true
	}
	return n.Scoped()
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
	// PaceNoWindowLength: ccdad has no length for this window, so there is no
	// elapsed share to compute. cinder_cove is the case — its resets_at is an
	// expiry — along with any window name a later release adds.
	PaceNoWindowLength
	// PaceTooEarly: less than a seventh of the window has run.
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
	case PaceNoWindowLength:
		return "no known length for this window"
	case PaceTooEarly:
		return "less than a seventh of the window has run"
	case PaceWindowNotStarted:
		return "the window has not started"
	}
	return "unknown"
}

// Pace is how one window's consumption compares with the time elapsed in it.
//
// It deliberately carries no projection fields. projectedExhaustionAt and
// willLastToReset stay out of every human-facing view, and the way to make that
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
// It stays out of every HUMAN-FACING view: real usage is bursty, and a straight
// line through it is too rough to state as fact in a table a person reads. It is
// reachable only through Pace.Projection, so putting it in front of a person has
// to be a deliberate act.
type Projection struct {
	// ExhaustionAt is when the window hits 100% at the current rate.
	ExhaustionAt time.Time
	// WillLastToReset is whether that lands at or after the reset.
	WillLastToReset bool
}

// Projection is the extrapolation, and whether there was one to make. It reports
// nothing when pace itself is suppressed: the numbers from a window's first
// seventh are exactly the ones too noisy to extrapolate from.
func (p Pace) Projection() (Projection, bool) {
	if !p.hasProjection {
		return Projection{}, false
	}
	return Projection{ExhaustionAt: p.exhaustionAt, WillLastToReset: p.willLastToReset}, true
}

// PaceOf measures one window against the clock.
func PaceOf(name WindowName, w Window, now time.Time) Pace {
	length, ok := windowLength(name)
	if !ok {
		return Pace{Reason: PaceNoWindowLength}
	}
	actual, ok := w.Percent()
	if !ok {
		return Pace{Reason: PaceNoUtilization}
	}
	reset, ok := w.Reset()
	if !ok {
		return Pace{Reason: PaceNoReset}
	}

	start := reset.Add(-length)
	elapsed := now.Sub(start)
	if elapsed < 0 {
		return Pace{Reason: PaceWindowNotStarted}
	}
	if elapsed < length/paceSuppressionDivisor {
		return Pace{Reason: PaceTooEarly}
	}
	// A reset already in the past is a window the endpoint has not rolled over
	// yet. Capping keeps expected at a full quota rather than running past it.
	if elapsed > length {
		elapsed = length
	}

	expected := float64(elapsed) / float64(length) * 100
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
		// Clamped BEFORE the conversion. time.Duration(secs) * time.Second
		// overflows int64 once secs passes about 9.22e9, and an overflow here
		// does not merely lose precision -- it wraps the instant into the PAST
		// and flips willLastToReset to false, which is the opposite of the
		// truth and reaches a switch decision through projectedExhaustion. A
		// seven-day window 65% elapsed at 0.003% used reported an exhaustion in
		// 1857 before this clamp. Saturating instead says "further out than this
		// type can express", which every reader of the field already treats as
		// "not soon".
		secs := remaining / perSecond
		if max := float64(maxDurationSeconds); secs > max {
			secs = max
		}
		p.exhaustionAt = now.Add(time.Duration(secs) * time.Second)
		p.willLastToReset = !p.exhaustionAt.Before(reset)
		p.hasProjection = true
	}
	return p
}

// Pace measures every window the response actually carried and ccdad knows a
// length for, the scoped ones included: an account whose binding cap is a
// per-model weekly one would otherwise get no pace reading at all, which is the
// account the reading is most useful for. Windows that were absent, or that have
// nothing to say, are left out rather than reported as a zero reading.
func (s *Snapshot) Pace(now time.Time) map[WindowName]Pace {
	out := map[WindowName]Pace{}
	for _, nw := range s.AllWindows() {
		// A window the response never carried has no utilization, so PaceOf
		// reports PaceNoUtilization and this filter drops it — no separate
		// presence check is needed, and one would be unreachable.
		if p := PaceOf(nw.Name, nw.Window, now); p.OK() {
			out[nw.Name] = p
		}
	}
	return out
}

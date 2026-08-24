package usage

import "time"

// ExpectedPct is the share of a window that has elapsed, as a percent: given how
// long is left before this window resets, what utilization is on pace.
//
// It carries NO suppression, and that is the whole difference between it and
// Pace. Pace is a verdict a person reads, and "ahead of pace" in the first hours
// after a reset is noise -- elapsed time is tiny there, so almost any usage
// divides out as far ahead, and a dashboard built on it cries wolf every Monday.
// This is a number a comparator holds a utilization against, and there the early
// reading is the useful one: an account that has spent a quarter of a window in
// its first twentieth should hand the next session to a peer, which costs
// nothing while a peer still has room.
//
// It answers false for a window with no length on record, one that reported no
// reset, and one whose reset is further out than the window is long. A zero
// would read as "this window has just reset", which is the most generous answer
// there is, and it would be handed to a threshold.
//
// PaceOf computes the same share for the windows it does answer for, and
// TestExpectedPctAgreesWithTheShareThatPaceReports pins the two together so the
// day either the cap or the window table moves, both move.
func ExpectedPct(name WindowName, w Window, now time.Time) (float64, bool) {
	length, ok := windowLength(name)
	if !ok {
		return 0, false
	}
	reset, ok := w.Reset()
	if !ok {
		return 0, false
	}
	elapsed := now.Sub(reset.Add(-length))
	if elapsed < 0 {
		return 0, false
	}
	// A reset already in the past is a window the endpoint has not rolled over
	// yet. Capping keeps this at a full quota rather than running past one.
	if elapsed > length {
		elapsed = length
	}
	return float64(elapsed) / float64(length) * 100, true
}

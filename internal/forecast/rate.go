// Package forecast measures how fast a fleet of accounts is spending its quota
// and decides what that rate implies.
//
// It is pure arithmetic. Nothing here opens a clock, a filesystem, an
// environment variable or a store: every input arrives as an argument, in the
// manner of internal/view. Nothing here polls either -- the readings it works
// from are the ones the daemon was already taking.
//
// No strategy.Thresholds value may enter this package. Hover rewrites
// thresholds every tick, and a forecast that moved because a ranking moved
// would report a change in the fleet that never happened.
package forecast

import (
	"time"

	"github.com/Kweiza/ccdaddy/internal/history"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

const (
	// rateWindow is how far back the burn rate is measured.
	//
	// It makes the rate a speedometer rather than an odometer, and that is the
	// trade being made knowingly: four idle hours read as "holds" and four hard
	// hours read as "dry in two days", and both are honest reports of the last
	// four hours rather than of the week. Every surface that prints the figure
	// prints the span and the reading count beside it for exactly that reason.
	//
	// Four hours is where an ordinary cadence clears the contribution gates
	// below with room. The poller's sustained floor is 180 s, which offers
	// eighty readings inside the window; at the other end AIMD parks a
	// repeatedly rate-limited account near 1800 s before jitter, and the daemon
	// multiplies that by the number of accounts sharing one identity, which can
	// push a single account's interval past an hour. No span guarantees three
	// readings -- minSamples is what carries that guarantee -- so this figure is
	// chosen for the ordinary case and the gates refuse the rest.
	// internal/history sizes its retention against the same four hours and keeps
	// six, so that the far end of this window still has a reading bracketing it
	// after the longest silence the poller can leave.
	rateWindow = 4 * time.Hour

	// minSamples is three because two samples hold one difference, and one
	// difference of a whole-percent field cannot be told from a single
	// quantisation step: an account that burned nothing and an account that
	// burned nine tenths of a point both report a rise of 0 or 1.
	minSamples = 3

	// minCover is a floor under minSamples, so that three readings arriving in
	// one burst -- a --refresh next to a scheduled poll next to a retry -- do
	// not qualify as evidence about four hours.
	//
	// It is deliberately NOT derived from a poll interval, and the derivation
	// that would do it does not exist: the ceiling AIMD parks a rate-limited
	// account at is 1800 s before jitter, and the daemon multiplies an ordinary
	// cadence by the number of accounts on one identity, so no span can be
	// chosen that guarantees a second difference. minSamples carries that
	// guarantee. Twenty minutes is a floor under it and nothing more.
	minCover = 20 * time.Minute
)

// Band is a measured rate and the upper bound the measurement cannot rule out,
// both in percentage points per hour.
//
// It is an interval rather than a number because the endpoint reports whole
// percents -- thirty live values read across six accounts on 2026-08-25 were
// integers without exception -- so a measured rise of three points is anything
// from just over two to just under four. Low is what gets printed. High is what
// a claim that an axis holds has to survive, and that is the whole defence
// against a thin basis: a coarse measurement produces a wide band, and a wide
// band declines to promise anything.
//
// Known false is not a rate of zero. A fleet nobody has enough readings for is
// not a fleet burning nothing, and the two must never render the same.
type Band struct {
	Low, High float64
	Known     bool
}

// rolledOver reports whether the window rolled over between two consecutive
// readings of it.
//
// It is a TEST rather than a comparison of the recorded reset times, and that is
// the whole reason the function exists. The endpoint derives the sub-second part
// of resets_at from its own clock at request time rather than from the window's
// anchor: two readings of one account's five-hour window 72 minutes apart on
// 2026-08-25 carried 01:50:00.308482Z and 01:50:00.320288Z, and three windows
// read out of one response cluster inside a few hundred microseconds of each
// other even though their anchors are days apart, which is what identifies the
// source. An equality test would therefore call every consecutive pair a
// rollover, leave every segment one sample long, sum over an empty set of pairs
// and report 0.0 points per hour for every account and every window forever --
// with "holds" on the screen and every test green. history.Reading stores the
// reset truncated to the minute against the same failure; the two defences are
// deliberately independent.
//
// Half a window's length is the threshold because a rollover advances the reset
// by exactly one length, and nothing else moves it that far. A reset that was
// never reported is the zero time; it cannot be differenced against anything, so
// on those readings the percentage arm decides alone.
//
// That percentage arm is what makes this a rollover predicate rather than a
// "the reset moved" predicate, and it is the only arm that can fire when no
// reset was reported at all. Its effect on the sum in windowRate is masked
// there, because the clamp in that loop discards a negative difference anyway;
// the direct test on this function is what pins it, and removing the arm is a
// mutation windowRate's own tests cannot see.
func rolledOver(a, b history.Reading, length time.Duration) bool {
	if b.Pct < a.Pct {
		return true
	}
	if a.Reset.IsZero() || b.Reset.IsZero() {
		return false
	}
	return b.Reset.Sub(a.Reset) >= length/2
}

// windowRate measures one account's consumption of one window name over the
// samples in [from, to].
//
// filled is that consumption in percentage points. cover is the span the
// measurement actually reaches across -- this account's own first and last
// reading of this window, which is not the requested range and not any other
// account's span. samples is how many readings carried the window.
//
// ok false means there is NO rate, which is not a rate of zero. An account that
// fails a gate is unmeasured: absent from every sum, counted as unmeasured, and
// never counted as burning nothing. filled is meaningful only when ok is true.
//
// The series must be oldest first, which is what history.Series returns. A
// sample that does not carry the window is skipped rather than treated as a
// break in the chain: a window that drops out of one response and returns in the
// next was still being consumed across the gap, and pairing across it counts
// that consumption once. What is never done is carrying the previous reading
// forward into the gap -- nothing read is not nothing used.
func windowRate(series []history.Sample, name usage.WindowName, from, to time.Time) (filled float64, cover time.Duration, samples int, ok bool) {
	// A window this release knows no length for cannot be rated. cinder_cove is
	// the case that matters: its resets_at is an expiry rather than a rollover,
	// so there is no length to halve, no rollover to detect, and a rate measured
	// over it would read a countdown to deletion as consumption.
	length, known := usage.WindowLength(name)
	if !known {
		return 0, 0, 0, false
	}

	var (
		first, last   time.Time
		prev          history.Reading
		havePrev      bool
		haveNewest    bool
		newestCarries bool
	)
	for _, s := range series {
		if s.At.Before(from) || s.At.After(to) {
			continue
		}
		r, carries := s.Windows[name]
		// The staleness gate asks about the newest SAMPLE, not the newest one
		// carrying this window, so it is recorded on every pass through the
		// range and read after the loop.
		haveNewest, newestCarries = true, carries
		if !carries {
			continue
		}
		if havePrev && !rolledOver(prev, r, length) {
			// The clamp is inside a segment, not around the sum: a small
			// backwards correction from the endpoint is not negative
			// consumption, and letting it subtract would have one account's
			// rounding pay for another account's burn.
			if d := r.Pct - prev.Pct; d > 0 {
				filled += d
			}
		}
		prev, havePrev = r, true
		if samples == 0 {
			first = s.At
		}
		last = s.At
		samples++
	}

	cover = last.Sub(first)
	switch {
	case !haveNewest || !newestCarries:
		// A stale account is unmeasured, not slow. Rating an account whose
		// newest reading no longer carries the window would freeze a slope from
		// evidence that stopped arriving and report the fleet's past as its
		// present.
		return 0, cover, samples, false
	case samples < minSamples, cover < minCover:
		return 0, cover, samples, false
	}
	return filled, cover, samples, true
}

// fleetBand turns summed consumption into the fleet's rate and the upper bound
// the measurement cannot rule out.
//
// Consumption is summed and time is SHARED. Summing per-account rates would
// project one account's forty-minute burst as though the whole fleet had
// sustained it for four hours; dividing one sum of points by one span does not.
//
// span is an argument rather than rateWindow because the fleet has usually not
// been observed for the whole window -- a daemon started twenty minutes ago has
// twenty minutes of evidence -- and dividing that consumption by four hours
// would report a sixth of the rate that was actually measured.
//
// High carries one quantisation step for each contributing account-window,
// because the endpoint reports whole percents and every contributor's own figure
// is therefore short of the truth by up to one point.
//
// No span or no contributors is not a rate of zero: Known stays false and the
// caller has to say "unmeasured" rather than "idle".
func fleetBand(filled float64, contributors int, span time.Duration) Band {
	if span <= 0 || contributors <= 0 {
		return Band{}
	}
	h := span.Hours()
	return Band{Low: filled / h, High: (filled + float64(contributors)) / h, Known: true}
}

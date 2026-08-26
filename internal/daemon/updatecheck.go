package daemon

import (
	"context"
	"time"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/pollpolicy"
)

// updateCheckInterval is how often the daemon asks the release origin what the
// newest release is.
//
// A day, and the number is chosen against the failure on either side of it.
// Shorter buys nothing a user can act on -- a release learned about six hours
// sooner is one they install at the same moment either way -- and every halving
// doubles the traffic to a host that has no idea ccdad exists. Longer would let
// a fix sit unseen for most of a week on a machine whose user never runs an
// update command by hand.
//
// The guarantee it carries is one request per interval per STORE, not per
// machine: the daemon singleton is keyed on the store home, and a machine with
// two ccdad stores on it runs two daemons on purpose.
const updateCheckInterval = 24 * time.Hour

// updateCheckSlack is how far out a published deadline may sit before it is
// treated as impossible rather than merely distant.
//
// Two intervals, because that is the smallest bound a live scheduler can never
// reach: the furthest ahead nextUpdateCheck ever writes is one interval plus
// ten percent. Anything past it was written by a machine whose clock was wrong,
// or restored from a backup taken on one.
const updateCheckSlack = 2 * updateCheckInterval

// updateCheckJitter spreads a duration by plus or minus pollpolicy.JitterFrac.
//
// The FRACTION is pollpolicy's rather than a second literal, so the spread on
// this cadence and the spread on every poll cadence cannot drift apart. The
// FUNCTION is not: pollpolicy's own is unexported, and exporting it would widen
// that package's surface to put a release check inside the package that decides
// how often an account is polled. So the fraction is shared and these six lines
// are deliberately duplicated.
//
// It is applied to the INTERVAL and never to the resulting instant. Multiplying
// a unix timestamp by 1.1 produces a nonsense date decades away, which is a
// mistake that looks like arithmetic and reads like a corrupted file.
func updateCheckJitter(d time.Duration, rnd float64) time.Duration {
	// Clamped rather than trusted. The sample comes from Engine.Rand, which a
	// caller supplies, and a source that returned 2 would double the interval
	// instead of nudging it.
	if rnd < 0 {
		rnd = 0
	}
	if rnd > 1 {
		rnd = 1
	}
	return time.Duration(float64(d) * (1 + pollpolicy.JitterFrac*(2*rnd-1)))
}

// nextUpdateCheck is when the check after the one dispatched at now falls due.
//
// The jitter is redrawn on every reschedule, not once at startup. A fleet that
// failed together against one outage would otherwise come back together, which
// is the burst the spread exists to prevent.
func nextUpdateCheck(now time.Time, rnd float64) time.Time {
	return now.Add(updateCheckJitter(updateCheckInterval, rnd))
}

// usableDeadline is the deadline to run on, given the one a previous daemon
// published.
//
// A deadline more than two intervals out is REPLACED with a fresh one rather
// than discarded. The zero value would be the obvious repair and it is the
// wrong one: zero means "never checked", which dispatches on the very next
// tick, so every machine with a wrong clock would arrive at the origin at once
// and a clock problem would become a stampede.
func usableDeadline(published, now time.Time, rnd float64) time.Time {
	if published.After(now.Add(updateCheckSlack)) {
		return nextUpdateCheck(now, rnd)
	}
	return published
}

// checkForRelease asks the origin what the newest release is, at most once per
// interval per store.
//
// It rides the tick rather than a ticker of its own: a second timer would be a
// second thing to shut down cleanly, for a decision that is one comparison
// against a clock the tick has already read.
//
// The stamp is written at DISPATCH and not when the answer arrives. The day's
// slot is then durable in the same iteration's publish, so a daemon that dies
// mid-check does not spend a second request when it comes back -- which is why
// the published field is called "checked" and means "asked", not "answered".
//
// There is no retry ladder. Failure and success schedule identically, which is
// what makes "one request per slot" true without qualification, and the jitter
// is redrawn on every reschedule so a fleet that failed against one outage does
// not come back in a single burst.
func (e *Engine) checkForRelease(ctx context.Context, cfg config.Config, now time.Time) {
	if !cfg.UpdateCheck || e.LatestRelease == nil {
		return
	}

	e.mu.Lock()
	if e.updateInFlight || now.Before(e.nextUpdateCheckAt) {
		e.mu.Unlock()
		return
	}
	e.updateCheckedAt = now
	e.nextUpdateCheckAt = nextUpdateCheck(now, e.rand())
	e.updateInFlight = true
	e.mu.Unlock()

	// Dispatched, never awaited. The tick never waits on the network -- Loop
	// waits for the body to return, so a tick that awaited this would stop
	// publishing status and stop executing switches for as long as the origin
	// took to answer.
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		// The TICK's context, so a shutdown cancels a check in flight, bounded
		// by the same clock one poll gets: there is one answer in this process
		// to "how long will the daemon wait on a network call", and a second
		// number here would be a second thing to keep in step.
		ctx, cancel := context.WithTimeout(ctx, e.pollTimeout())
		defer cancel()
		tag, err := e.LatestRelease(ctx)
		e.recordRelease(tag, err)
	}()
}

// recordRelease releases the slot the dispatch took. It records nothing yet:
// the slot must be released unconditionally or no later check ever runs, and
// that alone is what this commit needs to be correct.
func (e *Engine) recordRelease(tag string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.updateInFlight = false
	_, _ = tag, err
}

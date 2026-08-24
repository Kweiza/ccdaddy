package usage

import "time"

// The probe's half of the cache: a `claude -p` run spends the account's own
// quota to make a window that has never been used report a reset time, and this
// is where the attempt is recorded and rate-limited.

const (
	// ProbeRetryAfter is the minimum gap between two probe attempts for one
	// account. A probe spends the account's own quota, so a probe that fails —
	// no Claude Code on PATH, a login prompt nobody answers, a model outage —
	// must not be retried at the daemon's roughly 1 Hz tick. Six hours is a
	// little over one five-hour window, so an account that a probe cannot wake
	// is tried again about once per window rather than about once per second.
	ProbeRetryAfter = 6 * time.Hour

	// ProbePollDelay is how long after a probe the poll that reads what it woke
	// is scheduled for.
	//
	// Deliberately NOT in internal/pollpolicy: every number there is the usage
	// endpoint's own budget or the congestion response to it, and this is a wait
	// for a DIFFERENT service to have finished processing a turn. Polling at
	// once instead would spend the usage budget on top of the inference budget
	// the probe just spent, for a reading that is not there yet.
	ProbePollDelay = 60 * time.Second
)

// ProbeState is what the last probe of an account did.
//
// It lives in the usage cache rather than beside the quarantine in the engine
// state, and the placement is a decision rather than convenience. Three things
// settle it. This is the schedule of an attempt to take a READING, which is what
// NextPollAt and PollState in this same entry already are. Cache.Prune drops an
// entry dated before its account's AddedAt, so an account removed and added
// again at the same uuid gets a fresh probe budget — where strategy.State.Prune
// only drops uuids that are gone, and would hand a new login its predecessor's
// backoff. And the engine state is written only when the engine actually moves
// or quarantines, on purpose, so that an engine-rate writer is not put behind a
// poller-rate lock; a probe stamp is a poller-rate event — it is followed a
// minute later by a poll writing this very entry — and putting it there would be
// that same mistake in the other direction.
type ProbeState struct {
	// LastAttemptAt is when a probe was last ATTEMPTED, whatever came of it.
	// The zero time means never.
	LastAttemptAt time.Time `json:"last_attempt_at,omitempty"`
	// LastError is why the last attempt failed, kept because it is the only
	// record of a probe that ran detached and reported to nobody.
	LastError string `json:"last_error,omitempty"`
}

// MayProbe reports whether a probe may be attempted for this account at now.
//
// The gate counts every ATTEMPT and not only the failures. A probe that
// "succeeded" and still left the window without a reset time — claude answered
// out of a cache, the model spent against a different window than the one asked
// for — is exactly the case a failure-only gate would spin on, once per cadence,
// spending quota every time.
func (e Entry) MayProbe(now time.Time) bool {
	if e.Probe.LastAttemptAt.IsZero() {
		return true
	}
	// A stamp in the future is a clock that moved backwards rather than a probe
	// that has not happened yet, and the conservative direction here is to WAIT:
	// the cost of waiting is a late reading, and the cost of not waiting is the
	// user's quota.
	if now.Before(e.Probe.LastAttemptAt) {
		return false
	}
	return now.Sub(e.Probe.LastAttemptAt) >= ProbeRetryAfter
}

// RecordProbe stamps a probe attempt against an account and schedules the poll
// that will read what the probe woke.
//
// It is written twice for one probe, and the duplication is deliberate: an
// unattended caller stamps it BEFORE it starts a detached probe, because a probe
// that never starts must still consume the six-hour budget or an unstartable one
// is attempted on every cadence forever; and the probe itself stamps it again
// when it knows the outcome. Both go through the cache's own lock, and the
// second — the only writer that knows whether it worked — lands last.
//
// NextPollAt is NOT divided by the identity's size the way a cadence is. This is
// one poll rather than a rate, and the ordinary divided cadence resumes with the
// reading it takes.
func RecordProbe(timeout time.Duration, uuid string, at time.Time, probeErr error) error {
	return WithCache(timeout, func(c *Cache) error {
		e, had := c.Get(uuid)
		if !had {
			// There is no reading to keep, and an entry the cache prunes is a
			// backoff that does not survive the next evaluation: Prune drops
			// anything dated before the account was added. Stamping the attempt
			// keeps the record alive. The price is stated rather than hidden —
			// this also makes the entry FRESH, so the poll below is held at the
			// serve TTL rather than at ProbePollDelay. It costs nothing in the
			// case that matters: an account is only worth probing once a reading
			// has shown a window with no reset, and then there is an entry.
			e.FetchedAt = at
		}
		e.Probe.LastAttemptAt = at
		e.Probe.LastError = ""
		if probeErr != nil {
			e.Probe.LastError = probeErr.Error()
		}
		e.NextPollAt = at.Add(ProbePollDelay)
		c.Put(uuid, e)
		return nil
	})
}

// ResetFor is when one named window rolls over, and whether this reading named a
// time at all.
//
// A window that reported no resets_at has never been spent against, which is not
// the same as one that resets now: the endpoint answers null until something is
// actually spent, and there is no reset, no pace and no projection until then. A
// window the response did not carry at all answers the same way, which is the
// honest reading — an absent window has no rollover either. A nil reading is the
// same answer again, and AllWindows already returns nothing for one.
func (s *Snapshot) ResetFor(name WindowName) (time.Time, bool) {
	for _, w := range s.AllWindows() {
		if w.Name == name {
			return w.Reset()
		}
	}
	return time.Time{}, false
}

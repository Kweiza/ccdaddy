package usage

import "time"

// The probe's half of the cache: a `claude -p` run spends the account's own
// quota to start a window's clock, and this is where the attempt is recorded,
// judged by the reading that follows it, and rate-limited.
//
// The point of spending that turn is NOT to learn the reset time for its own
// sake. A five-hour window is anchored at FIRST USE and does not stretch when
// more is spent against it — measured on a live account, resets_at held at one
// instant (±0.9 s of server jitter) while utilization climbed from 5% to 24%
// over half an hour. So a clock started early is elapsed time the account gets
// for free: exhausting a window four hours into it costs one hour of lockout
// where exhausting a window started on first use costs five. Keeping every idle
// account's clock RUNNING is the whole errand, and every number below is sized
// for that.

const (
	// ProbeRetryAfter is the CEILING of the backoff ladder: the rate an account
	// that warm-ups demonstrably cannot wake settles at.
	//
	// It is no longer a retry interval. It used to be one — the only gate there
	// was — and that is what made the mechanism defeat itself: a five-hour
	// window goes cold five hours after it was started, and a flat six-hour gate
	// then refuses to restart it for another hour. The clock was cold for an
	// hour of every cycle, and it was cold in the hour right after the rollover,
	// which is the hour where starting it is worth the most. Measured on a live
	// fleet before this changed: cycles of 21805 s and 22213 s carrying 3805 s
	// and 4213 s of cold clock, about 4.2-4.6 hours per account per day.
	//
	// Six hours survives as the ceiling because it is what an account nothing
	// can wake used to cost, and the ladder must never be worse than what it
	// replaced: an account stuck at the cap is attempted about four times a day,
	// the same as before.
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

	// ProbeWakeMargin is how far AFTER a window's own rollover the poll that
	// finds it cold is aimed.
	//
	// A margin rather than the instant itself, because resets_at is not exact:
	// consecutive readings of one unchanged window disagreed by up to 0.9 s, and
	// drifts of -0.58 s and +0.87 s were both observed. Aiming at R itself would
	// land before the rollover on about half of those and buy a reading that
	// still says warm, which costs a whole poll interval.
	//
	// Sixty seconds and not more: this is dead clock by construction — the
	// window is cold for every second of it — so the margin is the smallest one
	// the jitter cannot beat, not a comfortable one.
	ProbeWakeMargin = 60 * time.Second

	// ProbeConfirmAfter is how long a warm-up is given to show up in a reading
	// before the reading is allowed to call it ineffective.
	//
	// It is ten minutes against a measured turn-to-reset lag of 61-62 s, and the
	// order of magnitude is the point. That lag sits directly ON the 60 s
	// ProbePollDelay: the poll a warm-up schedules for itself arrives at the
	// moment the endpoint is still deciding, so judging on THAT reading would
	// score working warm-ups as failures, walk the ladder to its six-hour cap,
	// and re-create by hand the very cold hour this design exists to remove. A
	// strike costs hours; the measurement it rests on is one minute; the
	// deadline sits far above both.
	//
	// The consequence is a rule, not a suggestion: the +60 s confirm poll may
	// CLEAR a streak and may never add one.
	ProbeConfirmAfter = 10 * time.Minute

	// ProbeSettleGap is the floor the ladder starts at — the interval in which
	// no verdict can exist yet, so no verdict can pace anything.
	//
	// It is a backstop against a 1 Hz loop and NOT the pacer. The pacer is the
	// window's own rollover: an account whose warm-up worked is not eligible
	// again until its clock actually runs down, which no timer here expresses.
	ProbeSettleGap = 15 * time.Minute
)

// probeBackoff is how long after an attempt the next one may be made, given how
// many consecutive attempts at that window have been judged to have woken
// nothing.
//
// The rungs are 15m / 1h / 2h / 4h / 6h. The first is ProbeSettleGap, which is
// not a punishment but the span in which the verdict has not run. The second is
// one hour rather than the old six because the common failure is TRANSIENT — a
// rotated credential, a model outage, a machine that was asleep — and making a
// transient failure cost six hours of cold clock is the hole this whole change
// closes. The last is ProbeRetryAfter, so an account that is genuinely
// unwakeable is never attempted more often than the flat gate attempted it.
func probeBackoff(strikes int) time.Duration {
	switch {
	case strikes <= 0:
		return ProbeSettleGap
	case strikes == 1:
		return time.Hour
	case strikes == 2:
		return 2 * time.Hour
	case strikes == 3:
		return 4 * time.Hour
	}
	return ProbeRetryAfter
}

// ProbeState is what the last warm-up of an account did.
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
	//
	// It is a REPORT and never a gate. The exit code cannot tell a turn that was
	// billed and then failed from one that never authenticated — both are
	// "claude exited 1" — so nothing schedules on it. Judged is what schedules,
	// and it reads the window instead.
	LastError string `json:"last_error,omitempty"`
	// Window is the window the last attempt aimed at. Judged needs it: the
	// verdict is "did THAT window start its clock", and a probe of a model
	// scoped weekly is not answered by the five-hour window waking.
	//
	// Empty means an entry written before this field existed. Judged treats that
	// as inconclusive rather than as a failure — a strike costs hours, and an
	// upgrade is not evidence about an account.
	Window WindowName `json:"window,omitempty"`
	// ColdStreaks counts, per window, how many consecutive attempts aimed at
	// that window a later reading judged to have woken nothing.
	//
	// Per WINDOW and not one counter, and this is load-bearing rather than
	// tidy. probeModel cannot express a model VERSION, so a weekly cap scoped to
	// a build `--model opus` no longer resolves to is a window warm-ups can
	// never wake; with a single counter, every five-hour rollover would retarget
	// it and reset that hopeless window's ladder to the bottom rung, and the
	// account would spend roughly three turns per five-hour cycle on it forever.
	// Held per window, the hopeless one climbs to the cap and stays there while
	// the five-hour window keeps its own clean record.
	ColdStreaks map[WindowName]int `json:"cold_streaks,omitempty"`
}

// Strikes is how many consecutive attempts at one window have woken nothing.
func (p ProbeState) Strikes(w WindowName) int { return p.ColdStreaks[w] }

// NextAttemptAt is the earliest instant a warm-up of w may be attempted, on the
// backoff arm alone. It is the reporting half of MayProbe's second arm, and it
// is what `ccdad probe` and daemon status print rather than deriving a
// second copy of the ladder.
func (p ProbeState) NextAttemptAt(w WindowName) time.Time {
	return p.LastAttemptAt.Add(probeBackoff(p.Strikes(w)))
}

// withStrikes returns a copy carrying a changed streak for one window.
//
// The map is cloned rather than written through. usage.Entry is handed around by
// value and its Snapshot is documented as shared-and-read-only; a map written in
// place would be the one field of a "copy" that is not one, and the reader
// holding the older copy would see a number nobody wrote for it.
func (p ProbeState) withStrikes(w WindowName, n int) ProbeState {
	if n == p.ColdStreaks[w] {
		return p
	}
	next := make(map[WindowName]int, len(p.ColdStreaks)+1)
	for k, v := range p.ColdStreaks {
		next[k] = v
	}
	if n <= 0 {
		delete(next, w)
	} else {
		next[w] = n
	}
	if len(next) == 0 {
		next = nil
	}
	p.ColdStreaks = next
	return p
}

// Judged is this probe state after a reading taken at now, given the reading
// FetchedAt of the entry the reading is replacing.
//
// This is the verdict, and it is taken from the WINDOW rather than from the
// child's exit status. The exit code is not the question: a probe can exit 1
// having already spent its turn, and one can exit 0 having spent it against a
// different window than the one asked for — the second case is why the gate this
// replaces counted every attempt rather than only the failures. What can be
// observed is whether the window's clock is running, and a reading is the only
// thing that can say so.
//
// Three outcomes:
//
//   - the aimed window reports a reset in the FUTURE — its clock is running, so
//     the streak is cleared. This runs whether or not an attempt is outstanding,
//     which is what lets warmth from ANY source clear a standing streak: a
//     human using the account is as good an answer as a warm-up, and an account
//     nobody warmed for a day must not carry yesterday's ladder into tonight.
//   - an attempt IS outstanding, ProbeConfirmAfter has passed, and the window
//     still reports no future reset — the attempt woke nothing, so the streak
//     advances one rung.
//   - anything else — no reading, no aimed window, or too soon to say — is
//     inconclusive and changes nothing.
//
// "Outstanding" is derived from the two timestamps rather than from a flag: an
// attempt is outstanding while the reading being replaced is not NEWER than the
// attempt. A persisted boolean would be set by the child process and cleared by
// the daemon, which is exactly the shape that goes stale when one of the two
// dies; two timestamps written by the same lock cannot disagree. It also makes
// the verdict fire at most once per attempt for free, because the very commit
// that judges an attempt writes the FetchedAt that ends it — so the daemon's
// pre-spawn stamp and the child's outcome stamp cannot advance one streak twice.
func (p ProbeState) Judged(prevFetchedAt time.Time, snap *Snapshot, now time.Time) ProbeState {
	if snap == nil || p.Window == "" {
		return p
	}
	if at, has := snap.ResetFor(p.Window); has && at.After(now) {
		return p.withStrikes(p.Window, 0)
	}
	if p.LastAttemptAt.IsZero() || prevFetchedAt.After(p.LastAttemptAt) {
		return p
	}
	if now.Before(p.LastAttemptAt.Add(ProbeConfirmAfter)) {
		return p
	}
	return p.withStrikes(p.Window, p.Strikes(p.Window)+1)
}

// MayProbe reports whether a warm-up of w may be attempted for this account at
// now. rollover is the reset instant the last reading reported for w, and
// hasRollover whether it reported one at all — strategy.ColdWindow answers both.
//
// There are two arms because there are two ways for a clock to be stopped, and
// they want opposite gates.
//
// A window whose reading carries a rollover that has PASSED is a clock that ran
// down. The gate for it is not an interval at all: one attempt per rollover,
// spelled as "the last attempt predates this rollover". That is a structural
// bound rather than a tuned one — however wrong every schedule around it goes,
// this arm cannot spend more than one turn per five-hour window — and it is what
// lets the warm-up land on the same tick as the poll that discovered the window
// cold instead of waiting a further poll interval for a timer to agree.
//
// Everything else — a window nothing has ever spent against, and any window
// whose attempts are being judged ineffective — is on the ladder. The ladder is
// a backstop against retrying a broken errand at the tick loop's 1 Hz, and it is
// keyed on this window's own streak so that one unwakeable window cannot pace
// another.
//
// The streak arm outranks the rollover arm deliberately. An account whose
// warm-ups wake nothing would otherwise get a free attempt at every rollover
// forever, which is the flat gate's failure mode with extra steps.
//
// LastAttemptAt is per ACCOUNT while the streak is per window, and that
// asymmetry is right rather than an oversight. A turn is a turn: whatever
// --model it was spent against, it starts the five-hour clock, so an attempt
// aimed at a weekly cap has already bought this rollover's warm-up and the
// account must not be charged a second one. What is per window is the JUDGEMENT
// — whether the window that turn aimed at actually woke — because that is a
// property of the window and not of the account.
// It lives on ProbeState rather than on Entry so that the ranking package, which
// carries the probe state on a Candidate and never the whole cache row, asks the
// same question through the same code. Entry.MayProbe is the delegate.
func (p ProbeState) MayProbe(now time.Time, w WindowName, rollover time.Time, hasRollover bool) bool {
	if p.LastAttemptAt.IsZero() {
		return true
	}
	// A stamp in the future is a clock that moved backwards rather than a probe
	// that has not happened yet, and the conservative direction here is to WAIT:
	// the cost of waiting is a late reading, and the cost of not waiting is the
	// user's quota.
	if now.Before(p.LastAttemptAt) {
		return false
	}
	if hasRollover && p.Strikes(w) == 0 {
		return p.LastAttemptAt.Before(rollover)
	}
	return !now.Before(p.NextAttemptAt(w))
}

// MayProbe answers for this entry's own probe state.
func (e Entry) MayProbe(now time.Time, w WindowName, rollover time.Time, hasRollover bool) bool {
	return e.Probe.MayProbe(now, w, rollover, hasRollover)
}

// RecordProbe stamps a probe attempt against an account and schedules the poll
// that will read what the probe woke.
//
// It is written twice for one probe, and the duplication is deliberate: an
// unattended caller stamps it BEFORE it starts a detached probe, because a probe
// that never starts must still consume the budget or an unstartable one is
// attempted on every cadence forever; and the probe itself stamps it again when
// it knows the outcome. Both go through the cache's own lock, and the second —
// the only writer that knows whether it worked — lands last.
//
// Neither writer touches ColdStreaks. The verdict belongs to Judged and to the
// reading it runs on, which is what keeps the double stamp from advancing one
// streak twice: this function records that an attempt happened and nothing about
// whether it worked.
//
// NextPollAt is NOT divided by the identity's size the way a cadence is. This is
// one poll rather than a rate, and the ordinary divided cadence resumes with the
// reading it takes.
func RecordProbe(timeout time.Duration, uuid string, at time.Time, w WindowName, probeErr error) error {
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
		e.Probe.Window = w
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

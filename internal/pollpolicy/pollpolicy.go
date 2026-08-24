// Package pollpolicy decides when to poll /api/oauth/usage next.
//
// cswap's measured cadences are taken over verbatim rather than reinvented,
// and the reason is the shape of the budget. The endpoint allows roughly
// 28-30 requests per identity per rolling hour and it is a SLIDING WINDOW, not
// a bucket: capacity returns only as old requests age out, so a burst saturates
// the identity for up to a full hour and pausing does not give any of it back
// early. A "sleep it off" recovery path is wrong by construction.
//
// Which makes an over-eager poller worse than no poller at all. The
// unknown-is-never-zero rule means a blind engine cannot switch, so a fleet
// that spent the hour's budget in a minute has not merely lost freshness — it
// has parked the auto-switch engine until the window rolls.
//
// The whole package is a pure function over its own input struct. It imports
// nothing but the standard library — no internal/usage, no cache, no clock —
// so every rule below is table-testable and the randomness is an argument
// rather than a global.
//
// What is NOT here: the budget is per IDENTITY, not per account, so two
// accounts in the same organization share one. Grouping them is the
// scheduler's, because it is the thing that knows the org of each account;
// this package answers about one account at a time.
package pollpolicy

import (
	"strconv"
	"strings"
	"time"
)

// The measured cadence table, verbatim. Every behavioural test in this package
// is phrased in terms of these, so a changed value moves the assertions with
// it — which is why one test pins the numbers themselves.
const (
	// ServeTTL is the age under which a reading is served from the cache with
	// no fetch, `--refresh` included.
	ServeTTL = 180 * time.Second
	// MinInterval is the normal floor. The urgent cadence is the one documented
	// exception to it.
	MinInterval = 180 * time.Second
	// UrgentInterval is for the active account when it is close to the
	// threshold AND moving. Both halves are required.
	UrgentInterval = 60 * time.Second
	// ActiveMaxInterval is the active account when it is not moving.
	ActiveMaxInterval = 300 * time.Second
	// CandidateMaxInterval is an idle alternate.
	CandidateMaxInterval = 600 * time.Second
	// ExhaustedInterval keeps spent accounts polling: quota can be granted or
	// reset before the advertised timestamp, and decision-grade status must not
	// age into "unavailable" while the scheduler waits.
	ExhaustedInterval = 600 * time.Second
	// Post429MinInterval is the floor while any 429 was seen inside
	// Recent429Window.
	Post429MinInterval = 360 * time.Second
	// Post429BackoffMult is AIMD's multiplicative increase, applied once per
	// 429.
	Post429BackoffMult = 1.5
	// Post429MaxInterval is the congestion ceiling the increase clamps at.
	Post429MaxInterval = 1800 * time.Second
	// Recent429Window matches the saturation horizon: an hour, the same span
	// the endpoint's sliding window covers.
	Recent429Window = 3600 * time.Second
	// MovementDeltaPct is how far the binding percentage must move to count as
	// moving.
	MovementDeltaPct = 1.0
	// JitterFrac keeps independent processes out of lockstep.
	JitterFrac = 0.1
	// UrgentBandPct is how close to the threshold the active account must be
	// for the urgent cadence. It is not a cadence of its own — it is half of the
	// urgent rule, within 15 pp of threshold AND moving — so cswap's measured
	// set carries it as prose rather than as a number. It is a constant here
	// for the same reason every other measured number in this block is.
	//
	// spendArmFraction, from the same measured set, is not here: it belongs to
	// the credit gate and already exists in internal/strategy. Re-spelling it
	// would put one number in two places that must agree.
	UrgentBandPct = 15.0
	// DangerBandPct is where the live account stops sharing its identity's
	// budget. Five points of a five-hour window is under three minutes of a busy
	// session, which is less than the 180 s an alternate is already allowed to go
	// unread — so past this line the fleet's freshness is worth less than the one
	// account a session can be cut off on.
	//
	// It is a distance from 100 and NOT from Threshold, and that is the whole
	// distinction it exists to draw: Threshold is where ccdad decided an account
	// should stop being used, and 100 is where the endpoint refuses. UrgentBandPct
	// is a band around the first, which is why it moves when a window's threshold
	// moves. This is a band around the second, which is why it is a constant
	// rather than a second knob.
	DangerBandPct = 95.0
	// DangerServeTTL is how long a reading taken inside the danger band may be
	// served before another poll is worth a request.
	//
	// ServeTTL is 180 s and the band asks for a poll every 60 s. Leaving the TTL
	// alone would have the scheduler's own freshness gate refuse two of every
	// three polls the band just asked for: the cadence would exist in the schedule
	// and nowhere else, and every consumer would still read a three-minute-old
	// number.
	DangerServeTTL = 30 * time.Second
)

// State is one account's polling history. The caller carries it across calls
// and PERSISTS it: a backoff a 429 earned must survive a restart, or a
// crash-looping daemon resets its own congestion estimate every time.
type State struct {
	// Interval is the AIMD cadence currently in force. Zero means none has been
	// earned.
	Interval time.Duration
	// LastRateLimited is when a 429 was last seen. The zero time means NEVER,
	// which is not the same as an hour ago.
	LastRateLimited time.Time
	// LastBindingPct is the previous sample, and HasLastBinding whether there
	// was one at all. The first poll of an account has no predecessor, and
	// calling that "moving" drops every account to UrgentInterval on every
	// daemon start.
	LastBindingPct float64
	HasLastBinding bool
}

// Reading is the sample just taken.
type Reading struct {
	// BindingPct is percent USED on the binding window, and Known whether it
	// could be read at all.
	//
	// Unknown is never zero. As zero, an unreadable account looks maximally
	// far from its threshold, and against a real previous sample it looks like
	// an enormous move — so it would be polled on the wrong cadence in
	// whichever direction happens to be worse.
	BindingPct float64
	Known      bool
	// Exhausted means every window is spent.
	Exhausted bool
}

// Input is everything the cadence depends on besides the history.
type Input struct {
	Now time.Time
	// Active marks the account Claude Code is currently logged in as.
	Active bool
	// Reading is the sample just taken.
	Reading Reading
	// Threshold is the spent line this reading is measured against, in percent,
	// for the BINDING window the reading came from.
	//
	// One number, resolved by the caller. With a weekly floor at 60 and a
	// five-hour line at 85, what belongs here is whichever of the two the binding
	// window carries — not the global fallback, which at 80 would put every rule
	// phrased around this field twenty points out of place on that weekly floor.
	// The resolution is deliberately not here: this package imports nothing but
	// the standard library, which is what makes every rule in it table-testable
	// with no clock, no cache and no config.
	//
	// It is not what the danger band is measured against. DangerBandPct is a
	// distance from 100 because 100 is where the endpoint refuses; this is only
	// where ccdad decided to stop.
	Threshold float64
}

// Next reports when this account should be polled again, and the state to carry
// forward.
//
// rnd is a uniform sample in [0,1). It is an argument rather than a call into
// math/rand so the whole policy stays a pure function; the caller passes
// rand.Float64().
func Next(s State, in Input, rnd float64) (time.Time, State) {
	next := s
	moving := movement(s, in.Reading)
	if in.Reading.Known {
		next.LastBindingPct, next.HasLastBinding = in.Reading.BindingPct, true
	}

	d := base(in, moving)

	// The two post-429 rules are separate and BOTH apply: a floor while any 429
	// is inside the window, and the AIMD estimate on top of it. Either one
	// alone leaves a gap — without the floor a single 429 on an urgent account
	// still polls at 60 s, and without the estimate repeated 429s never slow
	// anything down.
	if recent429(s, in.Now) {
		d = longest(d, Post429MinInterval, s.Interval)
	} else if !s.LastRateLimited.IsZero() {
		// The window has passed with no further 429. The congestion estimate
		// lapses with it, so a later 429 starts the increase over rather than
		// resuming a stale one — and an account is never parked permanently by
		// one bad hour.
		next.Interval = 0
	}

	return in.Now.Add(jitter(d, rnd)), next
}

// base is the cadence before any rate-limit rule.
func base(in Input, moving bool) time.Duration {
	if InDangerBand(in) {
		// Ahead of BOTH rules below, and each override is deliberate.
		//
		// Ahead of Exhausted, because Exhausted is measured against the spent
		// line the caller chose: at a threshold of 80 every account inside the
		// band is also exhausted, so an exhausted-first order would make the band
		// unreachable in exactly the case it exists for. "Nothing left to watch
		// tick down" is a true statement about the threshold and a false one
		// about the five points left before the endpoint refuses.
		//
		// Ahead of the movement AND, because "merely close to its limit and
		// idle" stops being a fair description of an account five points out.
		// Idle is a claim about the last interval, not the next one, and a paused
		// session that resumes can spend the whole distance between two polls.
		return UrgentInterval
	}
	if in.Reading.Exhausted {
		// Deliberately ahead of the urgent rule. An exhausted active account
		// has nothing left to watch tick down, so polling it every minute buys
		// nothing and spends the identity's budget.
		return ExhaustedInterval
	}
	if in.Active {
		if moving && nearThreshold(in) {
			// AND, never OR. As an OR this fires for every account that is
			// merely close to its limit and idle, which halves the effective
			// interval across the whole fleet.
			return UrgentInterval
		}
		if moving {
			return MinInterval
		}
		return ActiveMaxInterval
	}
	if moving {
		return MinInterval
	}
	return CandidateMaxInterval
}

// nearThreshold is the "within 15 pp of threshold" half of the urgent rule. An
// account already past the threshold is inside the band too: it is the closest
// thing there is to the limit.
func nearThreshold(in Input) bool {
	if !in.Reading.Known {
		return false
	}
	return in.Reading.BindingPct >= in.Threshold-UrgentBandPct
}

// InDangerBand reports whether this is the LIVE account at or above
// DangerBandPct on its binding window.
//
// Only the live account, because the band spends most of an identity's budget
// on one account and the only account worth that is the one a session is
// running against; an alternate at 97% is a candidate nobody will pick, not an
// emergency. An unreadable sample is never in the band, for the same reason it
// is never near the threshold: unknown is not a percentage, and reading it as
// one would silence a whole identity on no evidence at all.
func InDangerBand(in Input) bool {
	if !in.Active || !in.Reading.Known {
		return false
	}
	return in.Reading.BindingPct >= DangerBandPct
}

// movement compares the sample against its predecessor. No predecessor is not
// movement, and neither is an unreadable sample.
func movement(s State, r Reading) bool {
	if !r.Known || !s.HasLastBinding {
		return false
	}
	delta := r.BindingPct - s.LastBindingPct
	if delta < 0 {
		delta = -delta
	}
	return delta >= MovementDeltaPct
}

// recent429 reports whether a 429 was seen inside the saturation horizon. The
// zero time is never, not an instant an hour before the epoch.
func recent429(s State, now time.Time) bool {
	if s.LastRateLimited.IsZero() {
		return false
	}
	return now.Sub(s.LastRateLimited) < Recent429Window
}

// PerIdentity divides a cadence among the accounts that share one identity's
// budget.
//
// The endpoint's allowance is per IDENTITY, not per account, so three accounts
// in one organization polling at MinInterval each is 60 requests an hour
// against a ~30-an-hour allowance. Multiplying by the group size holds the
// identity's aggregate rate where a single account's would have been.
//
// The urgent cadence is scaled like everything else, and it can still exceed
// the allowance transiently on a group of one — 60 s is 60 requests an hour.
// That overshoot is the urgent cadence's own arithmetic, and it is deliberate:
// urgency is a burst, not a steady state, and AIMD is the backstop when it
// turns out to be one.
//
// The one cadence that is NOT divided is the live account's inside the danger
// band, and that exemption lives in Share rather than here: this function is
// the division and nothing else, so a caller that wants the division does not
// also buy a policy decision it did not ask about.
//
// accounts below 1 means the caller has nothing to share among, which is the
// unshared interval rather than a licence to poll as fast as possible.
func PerIdentity(d time.Duration, accounts int) time.Duration {
	if accounts < 1 {
		return d
	}
	return d * time.Duration(accounts)
}

// Share is PerIdentity with the danger band's one exemption applied, and it is
// what a scheduler should call. The exemption and the division are two halves
// of one rule; a caller reaching for PerIdentity directly would divide the
// single cadence that must not be divided.
//
// The arithmetic, stated because it is the reason the band is shaped this way
// and not a more generous way. The endpoint allows roughly 28-30 requests per
// identity per rolling hour over a SLIDING window, so capacity comes back only
// as old requests age out. UrgentInterval is already 60 s — 60 requests an
// hour, a deliberate transient overshoot even on an identity of one. The
// exemption gives an identity of three that same 60 s on the ONE account that
// matters: a threefold freshness gain there, against the 180 s three accounts
// sharing the urgent cadence would each get, and exactly nothing on an identity
// of one, where 60 s was already the answer.
//
// It must never go below 60 s. That is twice the allowance already; a 429 then
// imposes the 360 s Post429MinInterval floor on top of an AIMD estimate that
// climbs to the 1800 s ceiling, so an account that earns one at 97% is blind
// for six minutes at the moment it can least afford to be. What actually keeps
// a session alive is switching before the projection lands; this only narrows
// that projection's error bars.
func Share(d time.Duration, accounts int, in Input) time.Duration {
	if InDangerBand(in) {
		return d
	}
	return PerIdentity(d, accounts)
}

// ServeTTLFor is how long the reading just taken may be served before another
// poll is worth a request. A scheduler records it WITH the reading, so the rule
// lives here once instead of being re-derived by every gate that has to honour
// it — and so it survives a restart, which is what stops the first tick after
// one from re-clamping the band to the flat 180 s.
func ServeTTLFor(in Input) time.Duration {
	if InDangerBand(in) {
		return DangerServeTTL
	}
	return ServeTTL
}

// StandDownUntil is when an account that yielded its share of the identity's
// budget may be polled again. rnd is a uniform sample in [0,1), as in Next.
//
// It is Post429MaxInterval because that is the longest cadence this package
// already trusts an account to survive on, and a stand-down is the same
// congestion decision a 429 forces, made one request early instead of one 429
// late. It is not "stop": an account nobody polls is an account nobody can
// rank, and the accounts standing down are exactly the ones the engine will
// want to switch TO — two requests an hour each is what keeps them rankable.
//
// The caller takes the LATER of this and whatever schedule the account had
// already earned, so a stand-down can hold an account back and can never let
// one out early — in particular it can never shorten a 429's floor.
func StandDownUntil(now time.Time, rnd float64) time.Time {
	return now.Add(jitter(Post429MaxInterval, rnd))
}

// RateLimited records a 429 and applies AIMD's multiplicative increase.
//
// retryAfter is what the endpoint asked for, and hasRetryAfter whether it asked
// at all — an absent header is not a zero wait. A longer request than our own
// estimate is honoured, because arguing with the endpoint about how saturated
// it is earns another 429; a shorter one is not, because its advice is a floor
// on our patience rather than a licence to poll sooner than our own backoff.
//
// Everything is bound by Post429MaxInterval, the endpoint's own request
// included: without that bound a mistaken header parks an account for as long
// as it says, and exhausted and rate-limited accounts are still polled because
// quota can come back early.
func RateLimited(s State, now time.Time, retryAfter time.Duration, hasRetryAfter bool) State {
	next := s
	next.LastRateLimited = now

	grown := s.Interval
	if grown <= 0 {
		grown = Post429MinInterval
	}
	grown = time.Duration(float64(grown) * Post429BackoffMult)
	if hasRetryAfter {
		grown = longest(grown, retryAfter)
	}
	if grown > Post429MaxInterval {
		grown = Post429MaxInterval
	}
	next.Interval = grown
	return next
}

// RateLimitedUntil is the post-429 floor as an instant, for the caller that has
// no cadence to ask Next() about: `ccdad list --refresh` is a button a hand
// presses, not a scheduler, and it is still held to the post-429 backoff.
//
// It is the LONGER of the flat floor and whatever AIMD has earned, measured
// from the 429 itself rather than from the last reading. A failed poll leaves
// the previous reading's timestamp alone — deliberately, since a failure must
// not overwrite real evidence — so an age-based hold would expire the instant
// a 429 arrived for an account whose last good reading was already stale.
//
// Recent429Window is not consulted, and that is not an omission: the hold can
// never outlast it, because Post429MaxInterval clamps the earned backoff at
// 1800 s against a 3600 s window. A branch that can never be taken is worse
// than no branch, so there is none.
//
// The IsZero guard below is the opposite case, and it is kept on purpose even
// though a mutation deleting it SURVIVES. Without it the answer is epoch plus
// the interval, and a time.Duration tops out at about 292 years — so from the
// zero time it can never reach a real clock, and the function returns false
// either way. That equivalence is an accident of an unrelated type's range,
// which is not what "never rate-limited is not rate-limited" should rest on:
// the guard says the thing the arithmetic only happens to agree with.
func RateLimitedUntil(s State, now time.Time) (time.Time, bool) {
	if s.LastRateLimited.IsZero() {
		return time.Time{}, false
	}
	at := s.LastRateLimited.Add(longest(Post429MinInterval, s.Interval))
	if !now.Before(at) {
		return time.Time{}, false
	}
	return at, true
}

// ParseRetryAfter reads either legal form of the header.
//
// RFC 9110 allows delta-seconds or an HTTP-date, and accepting only the integer
// form throws away a wait the endpoint explicitly asked for — after which the
// next request earns another 429. The date form is evaluated against the
// caller's clock, so a date already in the past reports absent rather than a
// negative or zero wait: a skewed clock must not turn "wait" into "go now".
func ParseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	v := strings.TrimSpace(value)
	if v == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 {
			return 0, false
		}
		return time.Duration(n) * time.Second, true
	}
	// http.ParseTime covers the three date formats RFC 9110 permits.
	at, err := parseHTTPDate(v)
	if err != nil {
		return 0, false
	}
	d := at.Sub(now)
	if d <= 0 {
		return 0, false
	}
	return d, true
}

// jitter spreads the INTERVAL by ±JitterFrac and the result is added to now.
//
// Applied to the resulting deadline instead, the multiplication lands on a unix
// timestamp and produces a nonsense date decades away. Applied nowhere at all,
// a fleet that paused together — a laptop waking, a daemon restarting across
// machines — comes back in lockstep and empties the shared per-identity budget
// in one burst.
func jitter(d time.Duration, rnd float64) time.Duration {
	if rnd < 0 {
		rnd = 0
	}
	if rnd > 1 {
		rnd = 1
	}
	return time.Duration(float64(d) * (1 + JitterFrac*(2*rnd-1)))
}

// longest is max over durations, spelled out because the zero value is a
// meaningful input here: an unearned backoff must not win.
func longest(ds ...time.Duration) time.Duration {
	var out time.Duration
	for _, d := range ds {
		if d > out {
			out = d
		}
	}
	return out
}

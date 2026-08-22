// Package pollpolicy is spec §7.4: when to poll /api/oauth/usage next.
//
// §12 says to port cswap's measured poll policy verbatim rather than reinvent
// it, and the reason is the shape of the budget. The endpoint allows roughly
// 28-30 requests per identity per rolling hour and it is a SLIDING WINDOW, not
// a bucket: capacity returns only as old requests age out, so a burst saturates
// the identity for up to a full hour and pausing does not give any of it back
// early. A "sleep it off" recovery path is wrong by construction.
//
// Which makes an over-eager poller worse than no poller at all. §7.2's
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

// §7.4's table, verbatim. Every behavioural test in this package is phrased in
// terms of these, so a changed value moves the assertions with it — which is
// why one test pins the numbers themselves.
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
	// ExhaustedInterval keeps spent accounts polling. §7.4: quota can be
	// granted or reset before the advertised timestamp, and decision-grade
	// status must not age into "unavailable" while the scheduler waits.
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
	// for the urgent cadence. §7.4's table names it only in the urgentInterval
	// row's prose ("within 15 pp of threshold"); it is a constant here for the
	// same reason every other number in that table is one.
	//
	// The thirteenth row of that table, spendArmFraction, is not here: it
	// belongs to §7.3's credit gate and already exists in internal/strategy.
	// Re-spelling it would put one number in two places that must agree.
	UrgentBandPct = 15.0
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
	// §7.2: unknown is never zero. As zero, an unreadable account looks
	// maximally far from its threshold, and against a real previous sample it
	// looks like an enormous move — so it would be polled on the wrong cadence
	// in whichever direction happens to be worse.
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
	// Threshold is §7.6's spent line, in percent.
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
// as it says, and §7.4 wants exhausted and rate-limited accounts still polled
// because quota can come back early.
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

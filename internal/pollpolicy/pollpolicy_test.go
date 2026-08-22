package pollpolicy

import (
	"math"
	"net/http"
	"testing"
	"time"
)

var epoch = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

// The numbers are the whole point of this package: §12 says to port cswap's
// measured poll policy verbatim, and every behavioural test below is phrased in
// terms of these, so any assertion would shrink along with a changed constant.
// This is the one test that cannot.
func TestTheConstantsAreSpecSevenPointFours(t *testing.T) {
	for _, c := range []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"ServeTTL", ServeTTL, 180 * time.Second},
		{"MinInterval", MinInterval, 180 * time.Second},
		{"UrgentInterval", UrgentInterval, 60 * time.Second},
		{"ActiveMaxInterval", ActiveMaxInterval, 300 * time.Second},
		{"CandidateMaxInterval", CandidateMaxInterval, 600 * time.Second},
		{"ExhaustedInterval", ExhaustedInterval, 600 * time.Second},
		{"Post429MinInterval", Post429MinInterval, 360 * time.Second},
		{"Post429MaxInterval", Post429MaxInterval, 1800 * time.Second},
		{"Recent429Window", Recent429Window, 3600 * time.Second},
	} {
		if c.got != c.want {
			t.Errorf("%s = %s, want %s", c.name, c.got, c.want)
		}
	}
	if Post429BackoffMult != 1.5 {
		t.Errorf("Post429BackoffMult = %v, want 1.5", Post429BackoffMult)
	}
	if MovementDeltaPct != 1.0 {
		t.Errorf("MovementDeltaPct = %v, want 1.0", MovementDeltaPct)
	}
	if JitterFrac != 0.1 {
		t.Errorf("JitterFrac = %v, want 0.1", JitterFrac)
	}
	if UrgentBandPct != 15.0 {
		t.Errorf("UrgentBandPct = %v, want 15.0", UrgentBandPct)
	}
}

// interval is Next with the jitter neutralized, so a table can assert the
// cadence rule rather than the randomness.
func interval(t *testing.T, s State, in Input) time.Duration {
	t.Helper()
	at, _ := Next(s, in, 0.5)
	return at.Sub(in.Now)
}

func sample(pct float64) Reading { return Reading{BindingPct: pct, Known: true} }
func unknown() Reading           { return Reading{} }
func seen(pct float64) State     { return State{LastBindingPct: pct, HasLastBinding: true} }

// §7.4's four cadences, plus the two the table names only in prose.
func TestTheCadenceMatchesTheSituation(t *testing.T) {
	const threshold = 80

	cases := []struct {
		name string
		s    State
		in   Input
		want time.Duration
	}{
		{
			// Active, inside the 15 pp band, and moving: both halves of the
			// urgent rule.
			name: "active near threshold and moving",
			s:    seen(66),
			in:   Input{Now: epoch, Active: true, Reading: sample(70), Threshold: threshold},
			want: UrgentInterval,
		},
		{
			// The AND is load-bearing. As an OR this would also be 60 s, which
			// halves the effective interval for every account that is merely
			// close to its limit and idle.
			name: "active near threshold but not moving",
			s:    seen(70),
			in:   Input{Now: epoch, Active: true, Reading: sample(70), Threshold: threshold},
			want: ActiveMaxInterval,
		},
		{
			name: "active moving but far from threshold",
			s:    seen(10),
			in:   Input{Now: epoch, Active: true, Reading: sample(20), Threshold: threshold},
			want: MinInterval,
		},
		{
			name: "active and idle",
			s:    seen(10),
			in:   Input{Now: epoch, Active: true, Reading: sample(10), Threshold: threshold},
			want: ActiveMaxInterval,
		},
		{
			name: "an idle alternate",
			s:    seen(10),
			in:   Input{Now: epoch, Reading: sample(10), Threshold: threshold},
			want: CandidateMaxInterval,
		},
		{
			name: "an alternate that is moving",
			s:    seen(10),
			in:   Input{Now: epoch, Reading: sample(30), Threshold: threshold},
			want: MinInterval,
		},
		{
			// §7.4: exhausted accounts keep polling, because quota can be
			// granted or reset before the advertised timestamp and
			// decision-grade status must not age into "unavailable" while the
			// scheduler waits.
			name: "exhausted, and still polled",
			s:    seen(100),
			in:   Input{Now: epoch, Reading: Reading{BindingPct: 100, Known: true, Exhausted: true}, Threshold: threshold},
			want: ExhaustedInterval,
		},
		{
			// The active account being exhausted does not make it urgent: there
			// is nothing left to watch tick down.
			name: "exhausted and active",
			s:    seen(99),
			in:   Input{Now: epoch, Active: true, Reading: Reading{BindingPct: 100, Known: true, Exhausted: true}, Threshold: threshold},
			want: ExhaustedInterval,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := interval(t, c.s, c.in); got != c.want {
				t.Fatalf("interval = %s, want %s", got, c.want)
			}
		})
	}
}

// The first poll of an account has no predecessor to compare against. Treating
// "no previous sample" as movement drops every account to UrgentInterval on
// every daemon start, which is a burst against a budget that recovers only by
// ageing out.
func TestTheFirstPollIsNotMovement(t *testing.T) {
	in := Input{Now: epoch, Active: true, Reading: sample(75), Threshold: 80}
	if got := interval(t, State{}, in); got != ActiveMaxInterval {
		t.Fatalf("interval = %s, want %s — a first sample counted as movement", got, ActiveMaxInterval)
	}
}

// §7.2's unknown-is-never-zero rule, applied to the cadence. A reading that
// could not be taken is not 0% used: as zero it looks maximally far from the
// threshold, and against a previous real sample it looks like a huge move.
func TestAnUnreadableSampleIsNeitherZeroNorMovement(t *testing.T) {
	in := Input{Now: epoch, Active: true, Reading: unknown(), Threshold: 80}
	if got := interval(t, seen(79), in); got != ActiveMaxInterval {
		t.Fatalf("interval = %s, want %s", got, ActiveMaxInterval)
	}
	// And it must not overwrite the last real sample, or the reading after it
	// would be compared against nothing.
	_, next := Next(seen(79), in, 0.5)
	if !next.HasLastBinding || next.LastBindingPct != 79 {
		t.Fatalf("last sample = (%v, %v), want the previous real one kept", next.LastBindingPct, next.HasLastBinding)
	}
}

// movementDeltaPct is a threshold, not a strict inequality dressed up as one:
// exactly one point of movement is movement.
func TestMovementIsAtLeastOnePoint(t *testing.T) {
	base := Input{Now: epoch, Reading: sample(11), Threshold: 80}
	if got := interval(t, seen(10), base); got != MinInterval {
		t.Fatalf("a 1.0 pp move gave %s, want %s", got, MinInterval)
	}
	base.Reading = sample(10.5)
	if got := interval(t, seen(10), base); got != CandidateMaxInterval {
		t.Fatalf("a 0.5 pp move gave %s, want %s", got, CandidateMaxInterval)
	}
	// Downwards counts too: a window that reset is a change worth noticing.
	base.Reading = sample(1)
	if got := interval(t, seen(40), base); got != MinInterval {
		t.Fatalf("a large drop gave %s, want %s", got, MinInterval)
	}
}

// Jitter multiplies the INTERVAL and the result is added to now. Applied to the
// deadline instead it would scale a unix timestamp — a nonsense date — and
// applied nowhere at all, a fleet that paused together re-synchronizes and
// hammers the shared per-identity budget in lockstep.
func TestJitterScalesTheIntervalAndNotTheDeadline(t *testing.T) {
	in := Input{Now: epoch, Reading: sample(10), Threshold: 80}
	s := seen(10)

	low, _ := Next(s, in, 0)
	mid, _ := Next(s, in, 0.5)
	high, _ := Next(s, in, 1)

	if got, want := low.Sub(in.Now), time.Duration(float64(CandidateMaxInterval)*(1-JitterFrac)); got != want {
		t.Fatalf("rnd=0 gave %s, want %s", got, want)
	}
	if got := mid.Sub(in.Now); got != CandidateMaxInterval {
		t.Fatalf("rnd=0.5 gave %s, want %s", got, CandidateMaxInterval)
	}
	if got, want := high.Sub(in.Now), time.Duration(float64(CandidateMaxInterval)*(1+JitterFrac)); got != want {
		t.Fatalf("rnd=1 gave %s, want %s", got, want)
	}
	// The spread is a fraction of the interval, which is what keeps it from
	// being a nonsense date: an hour's worth of drift on a ten-minute poll.
	if spread, cap := high.Sub(low), time.Duration(2*JitterFrac*float64(CandidateMaxInterval)); spread > cap {
		t.Fatalf("spread = %s, want at most %s", spread, cap)
	}
}

// AIMD: multiplicative increase per 429, clamped. Without the clamp the
// interval runs away and the account is never polled again; the endpoint can
// grant quota back before its own advertised timestamp, so an account parked
// forever is a decision the engine can never revisit.
func TestTheBackoffMultipliesAndClampsAtTheCeiling(t *testing.T) {
	s := State{}
	want := []time.Duration{
		time.Duration(float64(Post429MinInterval) * 1.5),
		time.Duration(float64(Post429MinInterval) * 1.5 * 1.5),
		time.Duration(float64(Post429MinInterval) * 1.5 * 1.5 * 1.5),
	}
	for i, w := range want {
		s = RateLimited(s, epoch, 0, false)
		if s.Interval != w {
			t.Fatalf("after %d rate limits interval = %s, want %s", i+1, s.Interval, w)
		}
	}
	for i := 0; i < 20; i++ {
		s = RateLimited(s, epoch, 0, false)
	}
	if s.Interval != Post429MaxInterval {
		t.Fatalf("interval = %s, want it clamped at %s", s.Interval, Post429MaxInterval)
	}
}

// Two rules, and BOTH apply: a floor of Post429MinInterval while any 429 is
// inside the window, and the AIMD interval on top of it.
func TestBothPost429RulesApply(t *testing.T) {
	idle := Input{Now: epoch, Reading: sample(10), Threshold: 80}

	// A single 429 whose AIMD interval (540 s) is under the idle cadence
	// (600 s): the base still wins, so the AIMD rule is not a replacement.
	s := RateLimited(State{LastBindingPct: 10, HasLastBinding: true}, epoch, 0, false)
	if got := interval(t, s, idle); got != CandidateMaxInterval {
		t.Fatalf("interval = %s, want the base cadence %s", got, CandidateMaxInterval)
	}

	// The urgent cadence is 60 s and the floor is 360 s: the floor wins, which
	// is the rule that stops a rate-limited account from being polled fastest
	// of all.
	urgent := Input{Now: epoch, Active: true, Reading: sample(70), Threshold: 80}
	if got := interval(t, State{LastBindingPct: 66, HasLastBinding: true, LastRateLimited: epoch}, urgent); got != Post429MinInterval {
		t.Fatalf("interval = %s, want the post-429 floor %s", got, Post429MinInterval)
	}

	// And once the AIMD interval exceeds both, it is what applies.
	grown := State{LastBindingPct: 66, HasLastBinding: true, LastRateLimited: epoch, Interval: 900 * time.Second}
	if got := interval(t, grown, urgent); got != 900*time.Second {
		t.Fatalf("interval = %s, want the grown interval 900s", got)
	}
}

// The congestion estimate is not permanent. recent429Window matches the
// saturation horizon, so once it has passed with no further 429 the account
// returns to its ordinary cadence — and a later 429 starts the increase again
// rather than resuming a stale one.
func TestTheBackoffLapsesOnceTheWindowHasPassed(t *testing.T) {
	s := State{LastBindingPct: 10, HasLastBinding: true, LastRateLimited: epoch, Interval: 900 * time.Second}
	in := Input{Now: epoch.Add(Recent429Window + time.Second), Reading: sample(10), Threshold: 80}

	if got := interval(t, s, in); got != CandidateMaxInterval {
		t.Fatalf("interval = %s, want the ordinary cadence %s", got, CandidateMaxInterval)
	}
	_, next := Next(s, in, 0.5)
	if next.Interval != 0 {
		t.Fatalf("Interval = %s, want it cleared so the next 429 starts over", next.Interval)
	}
	// One second earlier it is still inside the window.
	inside := Input{Now: epoch.Add(Recent429Window - time.Second), Reading: sample(10), Threshold: 80}
	if got := interval(t, s, inside); got != 900*time.Second {
		t.Fatalf("interval = %s, want the backoff still in force", got)
	}
}

// A zero LastRateLimited means never, not "at the epoch", and never must not
// read as inside the window on a machine whose clock is near it.
func TestNeverRateLimitedIsNotRecentlyRateLimited(t *testing.T) {
	in := Input{Now: time.Time{}.Add(time.Minute), Reading: sample(10), Threshold: 80}
	if got := interval(t, seen(10), in); got != CandidateMaxInterval {
		t.Fatalf("interval = %s, want %s", got, CandidateMaxInterval)
	}
}

// Retry-After is legally delta-seconds OR an HTTP-date. Accepting only the
// integer form throws away a wait the endpoint explicitly asked for, and the
// next request earns another 429.
func TestParseRetryAfterAcceptsBothLegalForms(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Duration
		ok    bool
	}{
		{"delta seconds", "120", 2 * time.Minute, true},
		{"delta seconds with spaces", "  120  ", 2 * time.Minute, true},
		{"zero seconds", "0", 0, true},
		// IMF-fixdate, which is the form RFC 9110 tells senders to use.
		{"http date", epoch.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second, true},
		// RFC 850, which it tells recipients to accept anyway.
		{"rfc 850 date", epoch.Add(90 * time.Second).Format("Monday, 02-Jan-06 15:04:05 GMT"), 90 * time.Second, true},
		// A date already gone is not a zero wait: a skewed clock must not turn
		// "wait" into "go now".
		{"http date in the past", epoch.Add(-time.Hour).Format(http.TimeFormat), 0, false},
		{"absent", "", 0, false},
		{"negative", "-5", 0, false},
		{"garbage", "soon", 0, false},
		{"float", "1.5", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseRetryAfter(c.value, epoch)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if ok && got != c.want {
				t.Fatalf("got %s, want %s", got, c.want)
			}
		})
	}
}

// An absent Retry-After is not a zero wait: the AIMD increase still applies, so
// the account is not re-fired immediately.
func TestAnAbsentRetryAfterStillBacksOff(t *testing.T) {
	s := RateLimited(State{}, epoch, 0, false)
	if s.Interval < Post429MinInterval {
		t.Fatalf("interval = %s, want at least %s", s.Interval, Post429MinInterval)
	}
}

// A Retry-After longer than the AIMD estimate is honoured — arguing with the
// endpoint about how saturated it is earns another 429 — but it is still bound
// by the congestion ceiling, so a mistaken header cannot park an account for a
// week.
func TestRetryAfterWinsWhenItIsLongerAndIsStillBounded(t *testing.T) {
	s := RateLimited(State{}, epoch, 20*time.Minute, true)
	if s.Interval != 20*time.Minute {
		t.Fatalf("interval = %s, want the 20m the endpoint asked for", s.Interval)
	}
	s = RateLimited(State{}, epoch, 7*24*time.Hour, true)
	if s.Interval != Post429MaxInterval {
		t.Fatalf("interval = %s, want it bounded at %s", s.Interval, Post429MaxInterval)
	}
	// Shorter than the AIMD estimate loses: the endpoint's advice is a floor on
	// our patience, not a licence to poll sooner than our own backoff.
	grown := State{Interval: 20 * time.Minute}
	s = RateLimited(grown, epoch, time.Second, true)
	if s.Interval < 20*time.Minute {
		t.Fatalf("interval = %s, want at least the previous estimate", s.Interval)
	}
}

// A deadline must never land in the past, whatever the inputs.
func TestTheDeadlineIsAlwaysInTheFuture(t *testing.T) {
	for _, rnd := range []float64{0, 0.25, 0.5, 0.75, 1, math.Nextafter(1, 0)} {
		at, _ := Next(State{}, Input{Now: epoch, Reading: sample(10), Threshold: 80}, rnd)
		if !at.After(epoch) {
			t.Fatalf("rnd=%v gave a deadline at or before now: %s", rnd, at)
		}
	}
}

// The budget belongs to the IDENTITY, not the account. Three accounts in one
// organization polling at MinInterval each is 60 requests an hour against a
// ~30-an-hour allowance, so the cadence has to be divided among them — and the
// division is here, with the arithmetic, rather than in whatever happens to
// know the organization.
func TestTheBudgetIsSharedAcrossAnIdentity(t *testing.T) {
	if got := PerIdentity(MinInterval, 1); got != MinInterval {
		t.Errorf("one account = %s, want %s", got, MinInterval)
	}
	if got, want := PerIdentity(MinInterval, 3), 3*MinInterval; got != want {
		t.Errorf("three accounts = %s, want %s", got, want)
	}
	// A count of zero is the caller having nothing to share among, not a
	// licence to poll as fast as possible.
	for _, n := range []int{0, -1} {
		if got := PerIdentity(MinInterval, n); got != MinInterval {
			t.Errorf("%d accounts = %s, want the unshared interval %s", n, got, MinInterval)
		}
	}
}

package pollpolicy

import (
	"math"
	"net/http"
	"testing"
	"time"
)

var epoch = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

// The numbers are the whole point of this package: they are cswap's measured
// poll policy taken over verbatim, and every behavioural test below is phrased
// in terms of these, so any assertion would shrink along with a changed
// constant. This is the one test that cannot.
func TestTheConstantsAreThePollPolicyTable(t *testing.T) {
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
		{"DangerInterval", DangerInterval, 180 * time.Second},
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
	if DangerBandPct != 95.0 {
		t.Errorf("DangerBandPct = %v, want 95.0", DangerBandPct)
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

// The four cadences the measured table names outright, plus the two it names
// only in prose.
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
			// Exhausted accounts keep polling, because quota can be granted
			// or reset before the advertised timestamp and decision-grade
			// status must not age into "unavailable" while the scheduler
			// waits.
			name: "exhausted, and still polled",
			s:    seen(100),
			in:   Input{Now: epoch, Reading: Reading{BindingPct: 100, Known: true, Exhausted: true}, Threshold: threshold},
			want: ExhaustedInterval,
		},
		{
			// The active account being exhausted does not make it urgent: there
			// is nothing left to watch tick down BELOW the danger band, where
			// "exhausted" means past the number the user chose rather than past
			// the one the endpoint enforces.
			name: "exhausted and active",
			s:    seen(84),
			in:   Input{Now: epoch, Active: true, Reading: Reading{BindingPct: 85, Known: true, Exhausted: true}, Threshold: threshold},
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

// The unknown-is-never-zero rule, applied to the cadence. A reading that
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

// The post-429 floor as a hand-held command has to read it. `ccdad list
// --refresh` does not run on a cadence, so it cannot ask Next() when it may
// call the endpoint again — it has to ask directly.
func TestTheHandHeldFloorIsTheLongerOfTheFloorAndTheEarnedBackoff(t *testing.T) {
	for _, c := range []struct {
		name  string
		state State
		now   time.Time
		want  time.Time
		held  bool
	}{
		{
			name:  "never rate limited",
			state: State{},
			now:   epoch,
		},
		{
			// The zero Interval is an unearned backoff, so the floor is what
			// stands. A max() that let it win would clear the hold entirely.
			name:  "one 429, no backoff earned yet",
			state: State{LastRateLimited: epoch},
			now:   epoch.Add(time.Minute),
			want:  epoch.Add(Post429MinInterval),
			held:  true,
		},
		{
			name:  "the earned backoff is longer than the floor",
			state: State{LastRateLimited: epoch, Interval: 900 * time.Second},
			now:   epoch.Add(7 * time.Minute),
			want:  epoch.Add(900 * time.Second),
			held:  true,
		},
		{
			// A backoff SHORTER than the floor must not shorten the hold.
			name:  "the earned backoff is shorter than the floor",
			state: State{LastRateLimited: epoch, Interval: 60 * time.Second},
			now:   epoch.Add(2 * time.Minute),
			want:  epoch.Add(Post429MinInterval),
			held:  true,
		},
		{
			name:  "the floor has elapsed",
			state: State{LastRateLimited: epoch},
			now:   epoch.Add(Post429MinInterval),
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			at, held := RateLimitedUntil(c.state, c.now)
			if held != c.held {
				t.Fatalf("held = %v, want %v", held, c.held)
			}
			if held && !at.Equal(c.want) {
				t.Errorf("until = %s, want %s", at, c.want)
			}
		})
	}
}

// The danger band. At 95% of the binding window there are five points between
// the session and the endpoint's refusal — less than one busy turn — so the
// account a session is running against stops sharing its identity's budget and
// polls at the floor.
func TestTheDangerBandPollsTheLiveAccountAtTheSustainedFloor(t *testing.T) {
	const threshold = 80

	for _, c := range []struct {
		name string
		s    State
		in   Input
		want time.Duration
	}{
		{
			// Idle, and urgent anyway. The AND that keeps an idle account off the
			// urgent cadence is a statement about an account with room, and five
			// points is not room.
			name: "active at the band and idle",
			s:    seen(97),
			in:   Input{Now: epoch, Active: true, Reading: sample(97), Threshold: threshold},
			want: DangerInterval,
		},
		{
			name: "active a tenth of a point below the band",
			s:    seen(94.9),
			in:   Input{Now: epoch, Active: true, Reading: sample(94.9), Threshold: threshold},
			want: ActiveMaxInterval,
		},
		{
			// The band is a threshold, not a strict inequality dressed up as
			// one: the line itself is inside it. Five points left is the case
			// the number was chosen for, so the account that has exactly five
			// cannot be the one it excludes.
			name: "active exactly on the line",
			s:    seen(95),
			in:   Input{Now: epoch, Active: true, Reading: sample(95), Threshold: threshold},
			want: DangerInterval,
		},
		{
			// The band is ahead of the exhausted rule deliberately. Exhausted is
			// measured against the caller's spent line, so at a threshold of 80
			// every account inside the band is also exhausted and the other order
			// would make the band unreachable in its own case.
			name: "active, at the band, and over the spent line",
			s:    seen(97),
			in:   Input{Now: epoch, Active: true, Reading: Reading{BindingPct: 97, Known: true, Exhausted: true}, Threshold: threshold},
			want: DangerInterval,
		},
		{
			// The band is a distance from 100 and not from Threshold, so a tight
			// weekly floor does not drag it down with it. At a threshold of 40,
			// 92% is fifty-two points past the spent line and still eight points
			// of real room — the exhausted cadence, not the emergency one.
			name: "a tight window threshold does not move the band",
			s:    seen(92),
			in:   Input{Now: epoch, Active: true, Reading: Reading{BindingPct: 92, Known: true, Exhausted: true}, Threshold: 40},
			want: ExhaustedInterval,
		},
		{
			name: "the same tight threshold, inside the band",
			s:    seen(96),
			in:   Input{Now: epoch, Active: true, Reading: Reading{BindingPct: 96, Known: true, Exhausted: true}, Threshold: 40},
			want: DangerInterval,
		},
		{
			// Nobody is spending an alternate, so its freshness is worth no more
			// than any other candidate's — and the band costs the whole identity's
			// budget.
			name: "a candidate at the band",
			s:    seen(97),
			in:   Input{Now: epoch, Reading: sample(97), Threshold: threshold},
			want: CandidateMaxInterval,
		},
		{
			// Unknown is never 97, for the same reason it is never 0.
			name: "active and unreadable",
			s:    seen(97),
			in:   Input{Now: epoch, Active: true, Reading: unknown(), Threshold: threshold},
			want: ActiveMaxInterval,
		},
		{
			// And the rule is that the sample was not TAKEN, not that it came
			// back as zero. A Reading carrying a percentage it could not
			// confirm is the shape the zero value happens to hide: with the
			// Known guard gone this reads as 97 and puts a whole identity on
			// the emergency cadence on no evidence at all.
			name: "active, unreadable, and carrying a stale percentage",
			s:    seen(97),
			in:   Input{Now: epoch, Active: true, Reading: Reading{BindingPct: 97}, Threshold: threshold},
			want: ActiveMaxInterval,
		},
		{
			// The one rule the band must never beat. Buying a shorter interval
			// with a 429 buys Post429MinInterval — six minutes blind at 97%, which
			// is the opposite of what the band is for.
			name: "at the band with a 429 inside the window",
			s:    State{LastBindingPct: 97, HasLastBinding: true, LastRateLimited: epoch},
			in:   Input{Now: epoch, Active: true, Reading: sample(97), Threshold: threshold},
			want: Post429MinInterval,
		},
		{
			name: "at the band with an earned backoff longer than the floor",
			s:    State{LastBindingPct: 97, HasLastBinding: true, LastRateLimited: epoch, Interval: 900 * time.Second},
			in:   Input{Now: epoch, Active: true, Reading: sample(97), Threshold: threshold},
			want: 900 * time.Second,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := interval(t, c.s, c.in); got != c.want {
				t.Fatalf("interval = %s, want %s", got, c.want)
			}
		})
	}
}

// The exemption, and its bound. The band hands the identity's urgent cadence to
// the one account a session is running against; on an identity of one it
// changes nothing, because 60 s was already the answer there.
func TestTheDangerBandIsExemptFromTheIdentityShare(t *testing.T) {
	band := Input{Now: epoch, Active: true, Reading: sample(97), Threshold: 80}
	idle := Input{Now: epoch, Reading: sample(10), Threshold: 80}

	if got := Share(DangerInterval, 3, band); got != DangerInterval {
		t.Errorf("the live account of three, in the band = %s, want %s", got, DangerInterval)
	}
	if got := Share(DangerInterval, 1, band); got != DangerInterval {
		t.Errorf("a sole account in the band = %s, want %s", got, DangerInterval)
	}
	if got, want := Share(CandidateMaxInterval, 3, idle), 3*CandidateMaxInterval; got != want {
		t.Errorf("an alternate on a three-account identity = %s, want %s", got, want)
	}
	// The exemption belongs to the BAND and not to the live account. Being live
	// and near the threshold is the ordinary urgent case, and it is still shared.
	near := Input{Now: epoch, Active: true, Reading: sample(90), Threshold: 80}
	if got, want := Share(UrgentInterval, 3, near), 3*UrgentInterval; got != want {
		t.Errorf("a live account below the band = %s, want the shared %s", got, want)
	}
}

// The reading taken inside the band has to age out on the band's own clock. A
// 180 s serve TTL over a 60 s cadence refuses two of every three polls the band
// just asked for, so the cadence would exist in the schedule and nowhere else.
func TestTheBandShortensTheServeTTLAndNothingElseDoes(t *testing.T) {
	band := Input{Now: epoch, Active: true, Reading: sample(97), Threshold: 80}
	if got := ServeTTLFor(band); got != ServeTTL {
		t.Errorf("ServeTTLFor in the band = %s, want %s", got, ServeTTL)
	}
	if DangerInterval < ServeTTL {
		t.Errorf("DangerInterval = %s is under the %s freshness gate, so the band's own "+
			"polls would be refused by it", DangerInterval, ServeTTL)
	}
	for _, c := range []struct {
		name string
		in   Input
	}{
		{"a live account below the band", Input{Now: epoch, Active: true, Reading: sample(90), Threshold: 80}},
		{"an alternate at the band", Input{Now: epoch, Reading: sample(97), Threshold: 80}},
		{"a live account that could not be read", Input{Now: epoch, Active: true, Reading: unknown(), Threshold: 80}},
	} {
		if got := ServeTTLFor(c.in); got != ServeTTL {
			t.Errorf("%s: ServeTTLFor = %s, want the ordinary %s", c.name, got, ServeTTL)
		}
	}
}

// A stand-down is a congestion decision made one request early rather than one
// 429 late, so it is the congestion ceiling. It is not "stop": an account nobody
// polls is an account nobody can rank, and the accounts standing down are
// exactly the ones the engine will want to switch to.
func TestAStandDownIsTheCongestionCeilingWithJitter(t *testing.T) {
	if got, want := StandDownUntil(epoch, 0.5).Sub(epoch), Post429MaxInterval; got != want {
		t.Fatalf("stand-down = %s, want %s", got, want)
	}
	low, high := StandDownUntil(epoch, 0), StandDownUntil(epoch, 1)
	if !low.After(epoch) {
		t.Fatalf("a stand-down landed at or before now: %s", low)
	}
	// Jittered for the reason every other cadence here is: an identity stood down
	// by one poll would otherwise come back all in the same second. Both bounds
	// are asserted, because a ceiling on the spread is satisfied perfectly by
	// there being no spread — which is the one outcome this rule exists to stop.
	if !high.After(low) {
		t.Fatalf("stand-downs at rnd 0 and rnd 1 both landed on %s, want them spread", low)
	}
	if spread, ceiling := high.Sub(low), time.Duration(2*JitterFrac*float64(Post429MaxInterval)); spread > ceiling {
		t.Fatalf("spread = %s, want at most %s", spread, ceiling)
	}
}

// The measured allowance the whole package is sized against: roughly 28-30
// requests per identity per rolling hour, on a sliding window. 28 is the low end
// and therefore the one an assertion should use.
const measuredHourlyAllowance = 28

// The band must not exceed the rate the endpoint will actually sustain, and this
// is the test that was missing.
//
// The bug it exists for: the band returned UrgentInterval, carried no movement
// requirement, and was exempt from Share(), so a live account parked at 96% on a
// single-member identity polled every 60 s -- 60 requests an hour against an
// allowance of 28-30 -- for as long as it stayed parked, with nothing in the
// package to bring it back down. Every existing band test asserted the cadence in
// isolation, which is a number, and none asserted the RATE, which is the thing
// the endpoint actually refuses. So the whole suite passed.
//
// It is phrased as a full simulated hour, with the FASTEST jitter sample on every
// step, on the worst case the package allows: a solo identity, so Share() divides
// by one and its band exemption is moot either way; never rate-limited, so no
// post-429 floor is doing the work; and parked, so nothing moves the account out
// of the band.
func TestTheBandNeverExceedsTheSustainedAllowanceOnASoloIdentity(t *testing.T) {
	in := Input{Now: epoch, Active: true, Reading: sample(97), Threshold: 80}
	st := seen(97)

	polls := 0
	for now := epoch; now.Before(epoch.Add(time.Hour)); {
		in.Now = now
		// rnd 0 is the fastest end of the jitter, so this counts the most polls
		// the policy can produce in an hour rather than an average one.
		at, next := Next(st, in, 0)
		st = next
		if !at.After(now) {
			t.Fatalf("the schedule did not advance past %s", now)
		}
		now = at
		polls++
	}
	if polls > measuredHourlyAllowance {
		t.Errorf("a parked live account in the band makes %d requests an hour, want at most %d "+
			"-- the endpoint's own allowance", polls, measuredHourlyAllowance)
	}
	// Share() cannot be credited with any of that: on a group of one the cadence
	// itself has to fit.
	if got := Share(DangerInterval, 1, in); got != DangerInterval {
		t.Errorf("Share on a solo identity = %s, want the undivided %s", got, DangerInterval)
	}
}

// The band REALLOCATES an identity's budget; it does not multiply it. The one
// account that gets faster is the one a session is running against, and every
// other account on the identity gets slower.
//
// What this no longer asserts is that the live account reaches UrgentInterval.
// That WAS the overshoot, and the old version of this test said so itself -- "the
// overshoot UrgentInterval already commits to on an identity of ONE" -- while
// asserting around it rather than against it.
func TestTheDangerBandReallocatesTheIdentityBudget(t *testing.T) {
	const accounts = 3
	perHour := func(d time.Duration) float64 { return float64(time.Hour) / float64(d) }

	in := Input{Now: epoch, Active: true, Reading: sample(97), Threshold: 80}
	liveIn := Share(interval(t, seen(97), in), accounts, in)

	if liveIn != DangerInterval {
		t.Fatalf("in the band the live account polls every %s, want exactly %s", liveIn, DangerInterval)
	}
	// The band's remaining value is that it beats the 600 s cadence an account
	// inside it would otherwise take. Against the urgent path it is deliberately
	// SLOWER, so the only comparison that means anything is against the cadence
	// the band actually replaces.
	if liveIn >= ExhaustedInterval {
		t.Fatalf("the band bought nothing over the exhausted cadence: %s in, %s out",
			liveIn, ExhaustedInterval)
	}
	// The alternates pay for it. A stand-down is never faster than the cadence it
	// replaces, which is what makes this a reallocation.
	idle := Input{Now: epoch, Reading: sample(10), Threshold: 80}
	siblingOut := Share(interval(t, seen(10), idle), accounts, idle)
	sibling := StandDownUntil(epoch, 0.5).Sub(epoch)
	if sibling < siblingOut {
		t.Fatalf("a stand-down of %s is faster than the %s cadence it replaces", sibling, siblingOut)
	}
	// The identity's total now fits the measured allowance outright, which is the
	// property the old arithmetic could only approach from above.
	total := perHour(liveIn) + (accounts-1)*perHour(sibling)
	if total > measuredHourlyAllowance {
		t.Fatalf("the identity makes %.1f requests an hour inside the band, want at most %d",
			total, measuredHourlyAllowance)
	}
}

// The sustained floor holds whatever the policy asks for, and this is the test
// whose failure requires the clamp to exist.
//
// It calls sustained() directly and not through Next(), on purpose. base() no
// longer produces a sub-floor cadence off the exempt path, so there is no Input
// that reaches the clamp from outside — and a floor that is only proved by the
// rules that happen to respect it today is proved by nothing. The `d` values here
// are what a future rule would hand it.
func TestTheSustainedFloorHoldsWhateverThePolicyAsksFor(t *testing.T) {
	// Live, moving, and inside the 15 pp band: the one exemption.
	exempt := Input{Now: epoch, Active: true, Reading: sample(70), Threshold: 80}
	// Live and parked in the danger band: fast, and NOT exempt. This is the
	// combination that spent twice the allowance.
	band := Input{Now: epoch, Active: true, Reading: sample(97), Threshold: 80}
	idle := Input{Now: epoch, Reading: sample(10), Threshold: 80}

	for _, c := range []struct {
		name   string
		d      time.Duration
		in     Input
		moving bool
		want   time.Duration
	}{
		{"a rule asking for a second, on an idle candidate", time.Second, idle, false, MinInterval},
		{"a rule asking for the old band cadence", UrgentInterval, band, false, MinInterval},
		{"the same, with the account also moving", UrgentInterval, band, true, MinInterval},
		{"the exempt urgent path keeps its cadence", UrgentInterval, exempt, true, UrgentInterval},
		{"the floor itself is not raised", MinInterval, idle, false, MinInterval},
		{"a slower rule is left alone", ExhaustedInterval, idle, false, ExhaustedInterval},
	} {
		if got := sustained(c.d, c.in, c.moving); got != c.want {
			t.Errorf("%s: sustained(%s) = %s, want %s", c.name, c.d, got, c.want)
		}
	}
}

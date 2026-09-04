package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// The interval is a RANGE, not a deadline. NewEngine wires math/rand's Float64
// as the jitter source, so every cadence in this process is spread, and a test
// that wants an exact instant pins the sample -- which is what makes the
// un-jittered implementation a mutation this can catch rather than one it
// cannot see.
func TestTheReleaseCheckDeadlineIsADayPlusOrMinusTenPercent(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		rnd  float64
		want time.Duration
	}{
		{"the low end of the range", 0, 24*time.Hour - 144*time.Minute},
		{"the midpoint is the interval itself", 0.5, 24 * time.Hour},
		{"the high end of the range", 1, 24*time.Hour + 144*time.Minute},
		{"a sample below the range is clamped rather than inverted", -1, 24*time.Hour - 144*time.Minute},
		{"a sample above the range is clamped", 2, 24*time.Hour + 144*time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nextUpdateCheck(now, tc.rnd)
			if want := now.Add(tc.want); !got.Equal(want) {
				t.Errorf("nextUpdateCheck(%v) = %v, want %v", tc.rnd, got, want)
			}
		})
	}
}

// A status.json restored from a backup, a machine whose clock jumped, or one
// whose time was wrong when the last daemon ran can carry a deadline years out.
// now.Before(nextAt) would then switch the feature off permanently and in
// silence, which is the worst of the three outcomes because nothing anywhere
// reports it.
func TestAnImpossibleDeadlineIsResampledRatherThanDiscarded(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	got := usableDeadline(now.Add(72*time.Hour), now, 0.5)
	if want := now.Add(24 * time.Hour); !got.Equal(want) {
		t.Errorf("usableDeadline(three days out) = %v, want a fresh %v", got, want)
	}
	// NOT the zero value. Zeroing it would make every machine with a bad clock
	// dispatch on its very next tick, which turns one machine's clock problem
	// into a fleet arriving at the origin together.
	if got.IsZero() {
		t.Error("an impossible deadline was discarded rather than replaced; every machine with a wrong clock would then check immediately")
	}

	for _, tc := range []struct {
		name      string
		published time.Time
	}{
		{"a deadline inside the window is left exactly as it was", now.Add(time.Hour)},
		{"a deadline already past is left alone so the check is due", now.Add(-time.Hour)},
		{"no deadline at all is left alone, which is what makes a fresh store check on its first tick", time.Time{}},
		{"the far edge of legitimate is kept", now.Add(updateCheckSlack)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := usableDeadline(tc.published, now, 0.5); !got.Equal(tc.published) {
				t.Errorf("usableDeadline(%v) = %v, want it untouched", tc.published, got)
			}
		})
	}
}

// stubRelease points the engine's resolver at a canned answer and counts what
// reached it. The counter is read only after e.Wait(), which the tick helper
// calls, so the race detector has a happens-before edge to work with.
func stubRelease(e *Engine, tag string, err error) *int {
	n := 0
	e.LatestRelease = func(context.Context) (string, error) {
		n++
		return tag, err
	}
	return &n
}

// releaseEngine is engineFor with a poll that always answers, so the tick has
// something ordinary to do around the release check.
func releaseEngine(t *testing.T) *Engine {
	t.Helper()
	return engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return snapshotWith(10), nil
	})
}

// A store with no recorded check dispatches on its first tick. That is one
// request shortly after a fresh install and it is deliberate: the alternative
// delays the feature's first useful answer by a day to avoid a burst that does
// not exist, because installs are not synchronised the way an outage recovery
// is.
func TestAStoreThatHasNeverCheckedAsksOnItsFirstTick(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	liveAs(t, "u-1")

	e := releaseEngine(t)
	n := stubRelease(e, "v0.7.0", nil)
	tick(t, e)

	if *n != 1 {
		t.Fatalf("the release origin was asked %d times, want 1", *n)
	}
	s := e.Snapshot()
	if !s.UpdateCheckedAt.Equal(tickEpoch) {
		t.Errorf("UpdateCheckedAt = %v, want the dispatch stamp %v", s.UpdateCheckedAt, tickEpoch)
	}
	if want := tickEpoch.Add(24 * time.Hour); !s.NextUpdateCheckAt.Equal(want) {
		t.Errorf("NextUpdateCheckAt = %v, want %v at the pinned midpoint sample", s.NextUpdateCheckAt, want)
	}
}

// The tick runs about once a second. A second one inside the window must not
// produce a second request, or the daily check is a per-second one.
func TestASecondTickInsideTheWindowDoesNotAskAgain(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	liveAs(t, "u-1")

	e := releaseEngine(t)
	n := stubRelease(e, "v0.7.0", nil)
	tick(t, e)
	tick(t, e)

	if *n != 1 {
		t.Fatalf("the release origin was asked %d times across two ticks, want 1", *n)
	}
}

// Nil is the refusing default, and it is what keeps every engine that is not a
// daemon off this request: NewEngine leaves the resolver unset, and internal/cli
// builds a real engine of its own for a refresh.
func TestAnEngineWithNoResolverNeverChecks(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	liveAs(t, "u-1")

	e := releaseEngine(t)
	if e.LatestRelease != nil {
		t.Fatal("NewEngine wired a release resolver; the only engine allowed to reach the origin is the daemon's")
	}
	tick(t, e)

	if s := e.Snapshot(); !s.NextUpdateCheckAt.IsZero() || !s.UpdateCheckedAt.IsZero() {
		t.Errorf("an engine with no resolver scheduled a check anyway: %+v", s)
	}
}

// The key is read from the config the tick already has in hand, so switching it
// takes effect on the NEXT TICK rather than at the next daemon start. A machine
// that has just been told it may not call out should stop calling out.
func TestTheUpdateCheckKeyStopsTheRequestAndFlippingItTakesEffectNextTick(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	liveAs(t, "u-1")
	writeConfig(t, "update_check = false\n")

	e := releaseEngine(t)
	n := stubRelease(e, "v0.7.0", nil)
	tick(t, e)
	if *n != 0 {
		t.Fatalf("the release origin was asked %d times with update_check off, want 0", *n)
	}
	if s := e.Snapshot(); !s.NextUpdateCheckAt.IsZero() {
		t.Errorf("a check was scheduled with the key off: %v", s.NextUpdateCheckAt)
	}

	writeConfig(t, "update_check = true\n")
	tick(t, e)
	if *n != 1 {
		t.Fatalf("the release origin was asked %d times after the key was turned back on, want 1", *n)
	}
}

// A movable clock, so a test can cross the deadline it just watched be set.
// Named apart from this package's existing movableEngine, which takes a fetch
// func and builds its engine straight from NewEngine -- leaving Freshen and
// ResolveOwner pointed at the real endpoints, which is exactly what a release
// test must not have.
func movableReleaseEngine(t *testing.T, now *time.Time) *Engine {
	t.Helper()
	e := releaseEngine(t)
	e.Now = func() time.Time { return *now }
	return e
}

func TestASuccessfulCheckRecordsTheReleaseWithoutItsLeadingV(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	liveAs(t, "u-1")

	e := releaseEngine(t)
	stubRelease(e, "v0.7.0", nil)
	tick(t, e)

	s := e.Snapshot()
	// Spelled the way buildinfo.Version is, because the reader compares the two
	// and a "v" on one side only is a comparison that fails on punctuation.
	if s.UpdateLatest != "0.7.0" {
		t.Errorf("UpdateLatest = %q, want %q", s.UpdateLatest, "0.7.0")
	}
	if s.UpdateCheckError != "" {
		t.Errorf("UpdateCheckError = %q, want it empty on a success", s.UpdateCheckError)
	}
}

// A failed check never erases the last good reading. A temporary outage must
// not un-tell the user about a release that is still out.
func TestAFailedCheckKeepsTheLastGoodReadingAndASuccessClearsTheError(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	liveAs(t, "u-1")

	now := tickEpoch
	e := movableReleaseEngine(t, &now)
	answer, failure := "v0.7.0", error(nil)
	e.LatestRelease = func(context.Context) (string, error) { return answer, failure }

	tick(t, e)
	if got := e.Snapshot().UpdateLatest; got != "0.7.0" {
		t.Fatalf("UpdateLatest = %q after the first check, want %q", got, "0.7.0")
	}

	answer, failure = "", errors.New("dial tcp: i/o timeout")
	now = now.Add(25 * time.Hour)
	tick(t, e)
	s := e.Snapshot()
	if s.UpdateLatest != "0.7.0" {
		t.Errorf("UpdateLatest = %q after a failure, want the last good reading %q", s.UpdateLatest, "0.7.0")
	}
	if s.UpdateCheckError == "" {
		t.Error("a failed check recorded no error; nothing else on the machine says the check is not working")
	}

	answer, failure = "v0.7.0", nil
	now = now.Add(25 * time.Hour)
	tick(t, e)
	// Without this, one failure leaves every reader warning about it forever,
	// beside a row that says the check is current.
	if got := e.Snapshot().UpdateCheckError; got != "" {
		t.Errorf("UpdateCheckError = %q after a successful check, want it cleared", got)
	}
}

// Cancellation is this process shutting down, which is a fact about the daemon
// rather than about the origin. Recording it would make "the release check
// failed" the last thing a clean stop publishes.
func TestACancelledCheckRecordsNothing(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	liveAs(t, "u-1")

	e := releaseEngine(t)
	n := stubRelease(e, "", context.Canceled)
	tick(t, e)

	// Asserted first, because every assertion below is also satisfied by a
	// check that was never dispatched at all -- the fields start empty, so
	// without this the test passes whether or not the gate ever fired.
	if *n != 1 {
		t.Fatalf("the release origin was asked %d times, want 1", *n)
	}
	s := e.Snapshot()
	if s.UpdateCheckError != "" {
		t.Errorf("UpdateCheckError = %q for a cancelled check, want nothing recorded", s.UpdateCheckError)
	}
	if s.UpdateLatest != "" {
		t.Errorf("UpdateLatest = %q for a cancelled check", s.UpdateLatest)
	}
	// The slot is still released, or every later check is blocked forever by a
	// flag nothing clears.
	if e.updateInFlight {
		t.Error("a cancelled check left the slot marked in flight; no check would ever run again")
	}
}

// An expired deadline is the opposite of a cancellation and IS recorded: the
// origin was asked and did not answer, which is a fact about the origin. The
// two arrive as sibling errors from the same context, so nothing but a test
// stops the cancellation arm being widened to swallow both.
func TestAnExpiredDeadlineIsRecordedUnlikeACancellation(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	liveAs(t, "u-1")

	e := releaseEngine(t)
	n := stubRelease(e, "", context.DeadlineExceeded)
	tick(t, e)

	if *n != 1 {
		t.Fatalf("the release origin was asked %d times, want 1", *n)
	}
	if got := e.Snapshot().UpdateCheckError; got == "" {
		t.Error("a check that timed out recorded nothing; the origin was asked and did not answer, and that is not the daemon shutting down")
	}
}

// The tag arrives over an unauthenticated channel and ends up in status.json
// and on a terminal. An answer that is not a release tag is a failed check, not
// a latest version nobody can parse.
func TestAnAnswerThatIsNotATagIsAFailedCheck(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	liveAs(t, "u-1")

	e := releaseEngine(t)
	stubRelease(e, "<html>404</html>", nil)
	tick(t, e)

	s := e.Snapshot()
	if s.UpdateLatest != "" {
		t.Errorf("UpdateLatest = %q, want nothing recorded for an answer that is not a tag", s.UpdateLatest)
	}
	if s.UpdateCheckError == "" {
		t.Error("an unparseable answer was recorded as a success")
	}
}

// The state is seeded where the DAEMON builds its engine, and it must happen
// before Run publishes its first document: that publish would otherwise
// overwrite the deadline with nothing, and a machine in a restart loop would
// spend one request per restart.
func TestTheDaemonsEngineCarriesTheLastDaemonsReleaseState(t *testing.T) {
	isolateEngine(t)
	now := time.Now()
	published := Status{
		UpdateCheckedAt:   now.Add(-2 * time.Hour),
		NextUpdateCheckAt: now.Add(22 * time.Hour),
		UpdateLatest:      "0.7.0",
		UpdateCheckError:  "dial tcp: i/o timeout",
	}
	if _, err := NewStatusWriter().Write(published, now); err != nil {
		t.Fatal(err)
	}

	e := engineForDaemon()
	// The one constructor that wires the resolver. NewEngine must not, or every
	// `ccdad status --refresh` acquires a release check.
	if e.LatestRelease == nil {
		t.Fatal("the daemon's engine has no release resolver, so the check can never run")
	}
	got := e.Snapshot()
	if !got.NextUpdateCheckAt.Equal(published.NextUpdateCheckAt) {
		t.Errorf("NextUpdateCheckAt = %v, want the published %v -- a restart that forgot it would ask again immediately",
			got.NextUpdateCheckAt, published.NextUpdateCheckAt)
	}
	if got.UpdateLatest != "0.7.0" {
		t.Errorf("UpdateLatest = %q, want the published %q", got.UpdateLatest, "0.7.0")
	}
	if got.UpdateCheckError != published.UpdateCheckError {
		t.Errorf("UpdateCheckError = %q, want the published %q", got.UpdateCheckError, published.UpdateCheckError)
	}
	if !got.UpdateCheckedAt.Equal(published.UpdateCheckedAt) {
		t.Errorf("UpdateCheckedAt = %v, want the published %v", got.UpdateCheckedAt, published.UpdateCheckedAt)
	}
	// NewEngine reads nothing. A status read inside the constructor would put
	// filesystem I/O on every CLI call site of it.
	if fresh := NewEngine().Snapshot(); !fresh.NextUpdateCheckAt.IsZero() || fresh.UpdateLatest != "" {
		t.Errorf("NewEngine read the published document: %+v", fresh)
	}
}

// A document from a backup or from a machine whose clock was wrong can carry a
// deadline years out. Honouring it would switch the feature off permanently and
// in silence, and zeroing it would have every such machine check immediately.
func TestASeededDeadlineFromTheFutureIsResampledRatherThanHonoured(t *testing.T) {
	isolateEngine(t)
	now := time.Now()
	if _, err := NewStatusWriter().Write(Status{NextUpdateCheckAt: now.Add(72 * time.Hour)}, now); err != nil {
		t.Fatal(err)
	}

	got := engineForDaemon().Snapshot().NextUpdateCheckAt
	if got.IsZero() {
		t.Fatal("an impossible deadline was discarded; the machine would ask on its very next tick")
	}
	if !got.After(now) || got.After(now.Add(updateCheckInterval+updateCheckInterval/10)) {
		t.Errorf("NextUpdateCheckAt = %v, want a fresh deadline inside one jittered interval of %v", got, now)
	}
}

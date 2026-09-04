package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/pollpolicy"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// movableEngine is engineFor with a clock the test advances, which is what
// every post-429 assertion needs: the floor is a span, so a frozen clock can
// never leave it.
func movableEngine(t *testing.T, now *time.Time,
	fetch func(context.Context, string) (*usage.Snapshot, error)) *Engine {
	t.Helper()
	e := NewEngine()
	e.Now = func() time.Time { return *now }
	e.Rand = func() float64 { return 0.5 }
	e.AccessToken = tokensAreFine
	e.FetchUsage = fetch
	return e
}

// refresh runs one hand-held pass over every managed account.
func refresh(t *testing.T, e *Engine, active string) []RefreshResult {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	return e.Refresh(context.Background(), s, s.Accounts(), config.Defaults(), active)
}

func onlyResult(t *testing.T, results []RefreshResult) RefreshResult {
	t.Helper()
	if len(results) != 1 {
		t.Fatalf("results = %+v, want exactly one", results)
	}
	return results[0]
}

// The reading has to land in the SAME cache the daemon reads, and the schedule
// has to come from the poll policy — a hand-held fetch that wrote a reading
// without a cadence would leave the next daemon tick free to spend the budget
// again.
func TestRefreshFetchesAndSchedulesThroughThePollPolicy(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	now := tickEpoch

	var polls int
	e := movableEngine(t, &now, func(context.Context, string) (*usage.Snapshot, error) {
		polls++
		return snapshotWith(42), nil
	})
	res := onlyResult(t, refresh(t, e, ""))

	if polls != 1 {
		t.Fatalf("polls = %d, want 1", polls)
	}
	if res.State != RefreshFetched {
		t.Fatalf("state = %v, want RefreshFetched", res.State)
	}
	entry, ok := cacheEntry(t, "u-1")
	if !ok {
		t.Fatal("the refresh cached no reading")
	}
	if pct, ok := entry.Snapshot.FiveHour.Percent(); !ok || pct != 42 {
		t.Fatalf("cached utilization = %v (%v), want 42", pct, ok)
	}
	if !entry.FetchedAt.Equal(tickEpoch) {
		t.Fatalf("FetchedAt = %s, want %s", entry.FetchedAt, tickEpoch)
	}
	// An idle alternate with no previous sample: candidateMaxInterval, which is
	// deliberately NOT the interval the active account would have earned.
	if want := tickEpoch.Add(pollpolicy.CandidateMaxInterval); !entry.NextPollAt.Equal(want) {
		t.Fatalf("NextPollAt = %s, want %s", entry.NextPollAt, want)
	}
}

// The poll policy's serveTTL, and the whole reason `--refresh` is safe to put
// under a human finger: a reading younger than 180 s is served from the cache.
func TestRefreshServesAReadingInsideTheServeTTLFromTheCache(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	now := tickEpoch

	var polls int
	e := movableEngine(t, &now, func(context.Context, string) (*usage.Snapshot, error) {
		polls++
		return snapshotWith(42), nil
	})
	refresh(t, e, "")

	now = tickEpoch.Add(pollpolicy.ServeTTL - time.Second)
	res := onlyResult(t, refresh(t, e, ""))
	if polls != 1 {
		t.Fatalf("polls = %d, want 1 — the second refresh re-fetched inside the TTL", polls)
	}
	if res.State != RefreshCached {
		t.Fatalf("state = %v, want RefreshCached", res.State)
	}
	if want := tickEpoch.Add(pollpolicy.ServeTTL); !res.At.Equal(want) {
		t.Fatalf("At = %s, want the instant the TTL expires %s", res.At, want)
	}

	// And it lapses: one second later the same call reaches the endpoint.
	now = tickEpoch.Add(pollpolicy.ServeTTL)
	if res := onlyResult(t, refresh(t, e, "")); res.State != RefreshFetched {
		t.Fatalf("state at the TTL boundary = %v, want RefreshFetched", res.State)
	}
	if polls != 2 {
		t.Fatalf("polls = %d, want 2 once the TTL has elapsed", polls)
	}
}

// The floor a 429 earns outlives the serveTTL, and it is measured from the 429
// rather than from the reading: a failed poll leaves the old reading's
// timestamp alone, so an age-based hold would expire immediately.
func TestRefreshHonoursThePost429FloorAfterTheServeTTLHasLapsed(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	now := tickEpoch

	var polls int
	e := movableEngine(t, &now, func(context.Context, string) (*usage.Snapshot, error) {
		polls++
		return nil, &usage.StatusError{Status: 429}
	})
	if res := onlyResult(t, refresh(t, e, "")); res.State != RefreshFailed {
		t.Fatalf("state = %v, want RefreshFailed on a 429", res.State)
	}

	// Well past serveTTL, and still inside the backoff the 429 earned:
	// post429MinInterval times the AIMD multiplier is 540 s, not 360 s.
	now = tickEpoch.Add(pollpolicy.ServeTTL + time.Minute)
	res := onlyResult(t, refresh(t, e, ""))
	if polls != 1 {
		t.Fatalf("polls = %d, want 1 — the second refresh went through the 429 backoff", polls)
	}
	if res.State != RefreshHeld {
		t.Fatalf("state = %v, want RefreshHeld", res.State)
	}
	want := tickEpoch.Add(time.Duration(float64(pollpolicy.Post429MinInterval) * pollpolicy.Post429BackoffMult))
	if !res.At.Equal(want) {
		t.Fatalf("At = %s, want %s", res.At, want)
	}

	now = want
	if res := onlyResult(t, refresh(t, e, "")); res.State != RefreshFailed {
		t.Fatalf("state once the backoff has elapsed = %v, want the endpoint tried again", res.State)
	}
	if polls != 2 {
		t.Fatalf("polls = %d, want 2 once the backoff has elapsed", polls)
	}
}

// An account with no OAuth grant behind it can never be polled. Reporting it as
// a failure would put a permanent error under a command a user runs to look at
// their accounts.
func TestRefreshSkipsAnAccountWithNothingToPollWith(t *testing.T) {
	isolateEngine(t)
	seedTokenAccount(t, "u-1")
	now := tickEpoch

	e := movableEngine(t, &now, func(context.Context, string) (*usage.Snapshot, error) {
		t.Error("a token account was polled; it has no refresh grant behind it")
		return nil, errors.New("unreachable")
	})
	if res := onlyResult(t, refresh(t, e, "")); res.State != RefreshUnpollable {
		t.Fatalf("state = %v, want RefreshUnpollable", res.State)
	}
}

// A failed attempt reports the cause and keeps the last good reading: an
// unknown account is not an empty one, and one bad minute must not erase the
// evidence the engine ranks on.
func TestRefreshReportsAFailureAndKeepsTheLastGoodReading(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	now := tickEpoch

	fail := errors.New("the endpoint is having a bad day")
	var fetch func(context.Context, string) (*usage.Snapshot, error) = func(context.Context, string) (*usage.Snapshot, error) {
		return snapshotWith(42), nil
	}
	e := movableEngine(t, &now, func(ctx context.Context, tok string) (*usage.Snapshot, error) {
		return fetch(ctx, tok)
	})
	refresh(t, e, "")

	fetch = func(context.Context, string) (*usage.Snapshot, error) { return nil, fail }
	now = tickEpoch.Add(time.Hour)
	res := onlyResult(t, refresh(t, e, ""))
	if res.State != RefreshFailed {
		t.Fatalf("state = %v, want RefreshFailed", res.State)
	}
	if !errors.Is(res.Err, fail) {
		t.Fatalf("Err = %v, want it to carry the cause", res.Err)
	}
	entry, ok := cacheEntry(t, "u-1")
	if !ok || entry.Snapshot == nil {
		t.Fatal("the failed refresh erased the last good reading")
	}
	if pct, _ := entry.Snapshot.FiveHour.Percent(); pct != 42 {
		t.Fatalf("cached utilization = %v, want the 42 the earlier poll read", pct)
	}
}

// The endpoint's allowance belongs to an IDENTITY, so the cadence is divided
// among every account that draws on it — including the ones this listing left
// out. Sizing over `want` instead would let a filtered `ccdad status --refresh`
// schedule the whole organization at the single-account rate.
func TestRefreshDividesTheIdentityBudgetOverTheFleetAndNotTheListing(t *testing.T) {
	isolateEngine(t)
	one := seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-1") // the same identity, and NOT in `want`
	now := tickEpoch

	e := movableEngine(t, &now, func(context.Context, string) (*usage.Snapshot, error) {
		return snapshotWith(42), nil
	})
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	e.Refresh(context.Background(), s, []store.Account{one}, config.Defaults(), "")

	entry, ok := cacheEntry(t, "u-1")
	if !ok {
		t.Fatal("no reading was cached")
	}
	want := tickEpoch.Add(pollpolicy.PerIdentity(pollpolicy.CandidateMaxInterval, 2))
	if !entry.NextPollAt.Equal(want) {
		t.Fatalf("NextPollAt = %s, want %s — the cadence was not shared with the account left out",
			entry.NextPollAt, want)
	}
}

// The poll policy's cadence branches on which account is live, and a refresh
// that never told the policy which one that is would schedule the active
// account on the idle-alternate cadence: 600 s where 300 s belongs.
func TestRefreshSchedulesTheActiveAccountOnTheActiveCadence(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	now := tickEpoch

	e := movableEngine(t, &now, func(context.Context, string) (*usage.Snapshot, error) {
		return snapshotWith(42), nil
	})
	refresh(t, e, "u-1")

	entry, ok := cacheEntry(t, "u-1")
	if !ok {
		t.Fatal("no reading was cached")
	}
	if want := tickEpoch.Add(pollpolicy.ActiveMaxInterval); !entry.NextPollAt.Equal(want) {
		t.Fatalf("NextPollAt = %s, want the active cadence %s", entry.NextPollAt, want)
	}
}

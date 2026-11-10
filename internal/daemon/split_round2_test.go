package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// The second review round of the split-login repair.

func recordPending(t *testing.T, to string, left strategy.Departure) {
	t.Helper()
	if err := strategy.WithState(time.Second, func(st *strategy.State) error {
		st.RecordPending(to, tickEpoch, left)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func pendingOnDisk(t *testing.T) (string, bool) {
	t.Helper()
	st, err := strategy.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	to, _, ok := st.Pending()
	return to, ok
}

// E1, the daily path. The laptop slept through ccdad's own rotation band;
// Claude Code refreshed u-1 in the KEYCHAIN and left the file on the previous
// pair, whose access token has expired -- so the oracle can never answer for
// it. Adopting the newer pair records the one it replaced, which names the
// file's login without asking, and the next pass syncs the file.
func TestTheLoginClaudeCodeLeftInTheFileIsNamedWithoutTheOracle(t *testing.T) {
	isolateEngine(t)
	seedAccountExpiring(t, "u-1", "org-1", tickEpoch.Add(-time.Hour))
	seedAccount(t, "u-2", "org-2")
	writeLiveLogin(t, "u-1", tickEpoch.Add(-time.Hour))
	kc := cclink.NewFakeKeychain(blobExpiring("RT-u-1-refreshed", tickEpoch.Add(8*time.Hour)))
	t.Cleanup(cclink.SetKeychainForTest(kc))
	recordActive(t, "u-1")
	readings(t, map[string]float64{"u-1": 36, "u-2": 40})

	e := engineFor(t, tokensAreFine, noFetch)
	e.ResolveOwner = func(_ context.Context, token string) (string, error) {
		if token == "AT-RT-u-1-refreshed" {
			return "u-1", nil
		}
		return "", errors.New("401: that access token has expired")
	}
	now := tickEpoch
	e.Now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		tick(t, e)
		now = now.Add(resolveMinInterval)
	}
	if !holds(fileNow(t), "u-1-refreshed") {
		t.Fatalf("the file still holds the pair Claude Code left behind:\n%s", fileNow(t))
	}
}

// E1. An unnamed login NEWER than the named one holds the plan -- it may be
// somebody's latest decision -- but for at most unresolvedFreezeMax. After
// that the plan runs, and its switch writes both stores.
func TestANewerUnnamedLoginHoldsThePlanForABoundedTime(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	writeLiveLoginRT(t, "RT-stranger", tickEpoch.Add(16*time.Hour)) // newer than the item's +8h
	withItem(t, "u-1")
	recordActive(t, "u-1")
	splitReadings(t) // u-1 at 99%: the plan moves to u-2

	now := tickEpoch
	e := engineFor(t, tokensAreFine, noFetch)
	e.Now = func() time.Time { return now }
	tick(t, e)
	if got := activeOnDisk(t); got != "u-1" {
		t.Fatalf("active = %q: the plan ran beside a newer login nobody has named", got)
	}
	now = now.Add(unresolvedFreezeMax)
	tick(t, e)
	if got := activeOnDisk(t); got != "u-2" {
		t.Fatalf("active = %q, want u-2: the plan is held past its bound", got)
	}
}

// E1. An unnamed login OLDER than the named one is a superseded one: the plan
// is not held for it at all.
func TestAnOlderUnnamedLoginDoesNotHoldThePlan(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	writeLiveLoginRT(t, "RT-stranger", tickEpoch.Add(time.Hour)) // older than the item's +8h
	withItem(t, "u-1")
	recordActive(t, "u-1")
	splitReadings(t)

	e := engineFor(t, tokensAreFine, noFetch)
	tick(t, e)
	if got := activeOnDisk(t); got != "u-2" {
		t.Fatalf("active = %q, want u-2: an older unnamed login held the plan", got)
	}
}

// E1. A login the oracle CONFIRMED belongs to nobody here does not hold the
// plan, newer or not: it is not ccdad's to protect.
func TestAForeignLoginDoesNotHoldThePlan(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	writeLiveLoginRT(t, "RT-stranger", tickEpoch.Add(16*time.Hour))
	withItem(t, "u-1")
	recordActive(t, "u-1")
	splitReadings(t)

	e := engineFor(t, tokensAreFine, noFetch)
	e.ResolveOwner = func(context.Context, string) (string, error) { return "someone-not-here", nil }
	tick(t, e)
	if got := activeOnDisk(t); got != "u-2" {
		t.Fatalf("active = %q, want u-2: a confirmed foreign login held the plan", got)
	}
}

// E2. ccdad's OWN unconfirmed switch decides the direction. A switch installs a
// stored login that may be hours old, while the account it left was refreshed
// in place since: here the login ccdad chose (u-2, in the item) is the OLDER
// one, and recency alone would undo the switch.
func TestCcdadsOwnUnconfirmedSwitchDecidesTheDirection(t *testing.T) {
	isolateEngine(t)
	seedAccountExpiring(t, "u-1", "org-1", tickEpoch.Add(8*time.Hour))
	seedAccountExpiring(t, "u-2", "org-2", tickEpoch.Add(time.Hour))
	writeLiveLogin(t, "u-1", tickEpoch.Add(8*time.Hour))
	kc := cclink.NewFakeKeychain(loginExpiring("u-2", tickEpoch.Add(time.Hour)))
	t.Cleanup(cclink.SetKeychainForTest(kc))
	recordActive(t, "u-1")
	recordPending(t, "u-2", strategy.Departure{From: "u-1", Closing: usage.WindowFiveHour})
	readings(t, map[string]float64{"u-1": 36, "u-2": 40})

	e := engineFor(t, tokensAreFine, noFetch)
	tick(t, e)

	if !holds(fileNow(t), "u-2") {
		t.Fatalf("the repair undid ccdad's own switch:\n%s", fileNow(t))
	}
	if _, to := lastSwitchOnDisk(t); to != "u-2" {
		t.Fatalf("cooldown stamped for %q, want u-2: the repair completed ccdad's switch", to)
	}
	if !departureOnDisk(t, "u-1") {
		t.Fatal("the departure the switch named was not recorded")
	}
	if _, ok := pendingOnDisk(t); ok {
		t.Fatal("the completed switch is still pending")
	}
}

// astra 5. Both stores already hold the pending target -- the file reached it
// some other way. That switch took effect, and it is COMPLETED: departure,
// cooldown and record -- not dropped.
func TestAPendingSwitchBothStoresAgreeOnIsCompleted(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	writeLiveLoginRT(t, "RT-u-2", tickEpoch.Add(8*time.Hour))
	withItem(t, "u-2")
	recordActive(t, "u-1")
	recordPending(t, "u-2", strategy.Departure{From: "u-1", Closing: usage.WindowFiveHour})
	readings(t, map[string]float64{"u-1": 36, "u-2": 40})

	e := engineFor(t, tokensAreFine, noFetch)
	tick(t, e)

	if got := activeOnDisk(t); got != "u-2" {
		t.Fatalf("active = %q, want u-2", got)
	}
	if _, to := lastSwitchOnDisk(t); to != "u-2" {
		t.Fatalf("cooldown stamped for %q, want u-2", to)
	}
	if !departureOnDisk(t, "u-1") {
		t.Fatal("the departure was dropped with the pending switch")
	}
}

// astra 4. The write backoff is dated from when the failed attempt RETURNED. A
// write that took ninety seconds to fail would otherwise have used up most of
// a minute's wait before it was even reported.
func TestTheWriteBackoffRunsFromWhenTheFailureReturned(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	liveAs(t, "u-1")
	kc := withItem(t, "u-1")
	kc.WritesRefused = true
	recordActive(t, "u-1")
	splitReadings(t)

	now := tickEpoch
	kc.OnWrite = func() { now = now.Add(90 * time.Second) }
	e := engineFor(t, tokensAreFine, noFetch)
	e.Now = func() time.Time { return now }
	_ = e.Tick(context.Background())
	e.Wait()
	writes := kc.Writes()

	// Past a minute from the start of the attempt, inside a minute from its end.
	now = tickEpoch.Add(failedWriteRetry + time.Minute)
	kc.OnWrite = nil
	_ = e.Tick(context.Background())
	e.Wait()
	if kc.Writes() != writes {
		t.Fatal("the switch was retried less than a minute after the failed attempt returned")
	}
}

// Opus S4. A Stale stand-down decided UNDER the lock -- the locks taken, the
// keychain read -- waits like a failed write: the next tick would reach the
// same answer about the same login.
func TestAStaleRepairUnderTheLockWaitsBeforeTheNext(t *testing.T) {
	isolateEngine(t)
	seedAccountExpiring(t, "u-1", "org-1", tickEpoch.Add(time.Minute))
	seedAccount(t, "u-2", "org-2")
	writeLiveLogin(t, "u-2", tickEpoch.Add(8*time.Hour))
	kc := cclink.NewFakeKeychain(loginExpiring("u-1", tickEpoch.Add(time.Minute)))
	t.Cleanup(cclink.SetKeychainForTest(kc))
	recordActive(t, "u-1")
	readings(t, map[string]float64{"u-1": 36, "u-2": 40})
	// The file is the newer login, so recency would repair towards u-2; ccdad's
	// own intent pins the repair to u-1, whose login is inside the window.
	recordPending(t, "u-1", strategy.Departure{})

	e := engineFor(t, tokensAreFine, noFetch)
	tick(t, e)
	reads := kc.Reads()
	tick(t, e)
	if got := kc.Reads() - reads; got != 1 {
		t.Fatalf("the second tick read the keychain %d times, want 1 (its evaluation only): a stale "+
			"repair was retried under the lock at once", got)
	}
}

// astra 1, at the daemon. The item holds an older login of u-1 that ccdad never
// stored, so only the oracle can name it; the file holds u-1's stored, newer
// login. The repair copies the file into the item -- which it can do only if
// the decision under the lock is told what the oracle said.
func TestARepairOverALoginOnlyTheOracleNamedGoesThrough(t *testing.T) {
	isolateEngine(t)
	seedAccountExpiring(t, "u-1", "org-1", tickEpoch.Add(8*time.Hour))
	seedAccount(t, "u-2", "org-2")
	writeLiveLogin(t, "u-1", tickEpoch.Add(8*time.Hour))
	kc := cclink.NewFakeKeychain(blobExpiring("RT-u-1-old", tickEpoch.Add(time.Hour)))
	t.Cleanup(cclink.SetKeychainForTest(kc))
	recordActive(t, "u-1")
	readings(t, map[string]float64{"u-1": 36, "u-2": 40})

	e := engineFor(t, tokensAreFine, noFetch)
	e.ResolveOwner = func(context.Context, string) (string, error) { return "u-1", nil }
	tick(t, e)
	tick(t, e)
	if secret, _ := kc.Secret(); !holds(secret, "u-1") {
		t.Fatalf("the item still holds the older login: %s", secret)
	}
}

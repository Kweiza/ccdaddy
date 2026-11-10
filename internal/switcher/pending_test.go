package switcher

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// The pending switch's lifetime: written ahead, settled exactly.

func pendingState(t *testing.T) (string, time.Time, bool) {
	t.Helper()
	st, err := strategy.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	to, _, ok := st.Pending()
	return to, st.PendingAt(), ok
}

// S-C. The intent is on disk BEFORE the first store write. A keychain write
// that lands and is never reported -- Ctrl-C, a daemon replaced mid-write --
// otherwise leaves a split with no intent, repaired by recency.
func TestTheIntentIsOnDiskBeforeTheKeychainIsWritten(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "chan@example.com")
	target := seed(t, "u-2", "support@example.com")
	liveAs(t, "u-1")
	kc := withItem(t, "u-1")
	seen := ""
	kc.OnWrite = func() { seen = PendingTarget() }

	if _, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1"}); err != nil {
		t.Fatal(err)
	}
	if seen != "u-2" {
		t.Fatalf("pending target when the keychain was written = %q, want u-2", seen)
	}
	if _, _, ok := pendingState(t); ok {
		t.Fatal("a switch that succeeded left its intent pending")
	}
}

// S-C. A failure `security` ANSWERED is known not to have moved the item: the
// intent written ahead is cleared.
func TestAConfirmedFailureClearsTheIntent(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "chan@example.com")
	target := seed(t, "u-2", "support@example.com")
	liveAs(t, "u-1")
	withItem(t, "u-1").WritesRefused = true

	if _, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1"}); err == nil {
		t.Fatal("a refused keychain write reported success")
	}
	if to, _, ok := pendingState(t); ok {
		t.Fatalf("a confirmed failure left %q pending", to)
	}
}

// S-C. A stand-down writes nothing, and its intent is cleared too.
func TestAStandDownClearsTheIntent(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "chan@example.com")
	target := seed(t, "u-2", "support@example.com")
	liveAs(t, "u-1")
	withItem(t, "u-1")

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "someone-else", Unattended: true})
	if err != nil || res.Outcome != Raced {
		t.Fatalf("Execute = %v, %v; want a raced stand-down", res.Outcome, err)
	}
	if to, _, ok := pendingState(t); ok {
		t.Fatalf("a stand-down left %q pending", to)
	}
}

// S-C, astra 4. An attended retry that finds both stores already on the
// pending target -- the first attempt's keychain write landed and its
// read-back failed -- records THAT switch: departure, cooldown, and the
// pending cleared. Left pending, a later /login would be undone towards it.
func TestAnAttendedRetryThatIsAlreadyOnCompletesThePendingSwitch(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "chan@example.com")
	target := seed(t, "u-2", "support@example.com")
	liveAs(t, "u-2")
	withItem(t, "u-2")
	recordActive(t, "u-2")
	if err := strategy.WithState(time.Second, func(st *strategy.State) error {
		st.RecordPending("u-2", time.Now(), strategy.Departure{From: "u-1", Closing: usage.WindowFiveHour})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-2"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != AlreadyOn || !res.CompletedPending {
		t.Fatalf("outcome = %v completed=%v, want already on completing the pending switch", res.Outcome, res.CompletedPending)
	}
	if _, to := lastSwitch(t); to != "u-2" {
		t.Fatalf("cooldown stamped for %q, want u-2", to)
	}
	if _, _, ok := pendingState(t); ok {
		t.Fatal("the completed switch is still pending")
	}
}

// S-C, astra 5. SettlePending decides from a view, and an intent recorded
// AFTER that view was read is one the view knows nothing about: it is left
// alone, not removed as "did not take effect".
func TestSettlePendingLeavesAnIntentRecordedAfterItsView(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "chan@example.com")
	seed(t, "u-2", "support@example.com")
	liveAs(t, "u-1")
	withItem(t, "u-1")
	recordActive(t, "u-1")
	st, err := cclink.ReadLoginStores()
	if err != nil {
		t.Fatal(err)
	}
	s := openStore(t)
	v := ViewLogin(st, s.Accounts(), s.Credentials, s.ActiveUUID(), nil)
	viewAt := time.Now()
	if err := strategy.WithState(time.Second, func(st *strategy.State) error {
		st.RecordPending("u-2", viewAt.Add(time.Second), strategy.Departure{})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	outcome, _, err := SettlePending(s, v, viewAt, viewAt.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if outcome != PendingOpen {
		t.Fatalf("outcome = %v, want open: the intent is newer than the view", outcome)
	}
	if to, _, ok := pendingState(t); !ok || to != "u-2" {
		t.Fatal("an intent recorded after the view was removed")
	}
}

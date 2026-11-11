package switcher

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// Round 4: an intent written ahead is not settled while its switch may still
// be writing, and only its own switch clears it.

func viewNow(t *testing.T) LoginView {
	t.Helper()
	st, err := cclink.ReadLoginStores()
	if err != nil {
		t.Fatal(err)
	}
	s := openStore(t)
	return ViewLogin(st, s.Accounts(), s.Credentials, s.ActiveUUID(), nil)
}

// Opus A, the probe. A daemon tick that reads the stores while an attended
// switch is still writing sees them agree on the account being left. The
// intent is younger than a switch can take, so it is NOT settled as "did not
// take effect": after the write lands unconfirmed, the intent is still there
// and the repair goes towards the switch's target.
func TestAnIntentStillBeingWrittenIsNotSettledAsNotTaken(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "chan@example.com")
	target := seed(t, "u-2", "support@example.com")
	liveAs(t, "u-1")
	kc := withItem(t, "u-1")
	kc.WriteLandsThenTimesOut, kc.ReadsFailAfterWrite = true, true
	recordActive(t, "u-1")

	var settled PendingSettlement
	kc.OnWrite = func() {
		now := time.Now()
		settled, _, _ = SettlePending(openStore(t), viewNow(t), now, now)
	}
	_, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1"})
	if !errors.Is(err, cclink.ErrLoginOutcomeUnknown) {
		t.Fatalf("Execute = %v, want ErrLoginOutcomeUnknown", err)
	}
	if settled == PendingNotTaken {
		t.Fatal("an intent still being written was settled as not taken")
	}
	if got := PendingTarget(); got != "u-2" {
		t.Fatalf("pending target after the unconfirmed write = %q, want u-2", got)
	}
}

// Opus A. An intent older than the longest a switch can still be writing, with
// both stores on another account, did not take effect: it is settled.
func TestAnIntentOlderThanASwitchCanTakeIsSettledAsNotTaken(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "chan@example.com")
	seed(t, "u-2", "support@example.com")
	liveAs(t, "u-1")
	withItem(t, "u-1")
	recordActive(t, "u-1")
	recorded := time.Now()
	if err := strategy.WithState(time.Second, func(st *strategy.State) error {
		st.RecordPending("u-2", recorded, strategy.Departure{})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	later := recorded.Add(cclink.WorstCaseSwitch() + time.Second)

	outcome, _, err := SettlePending(openStore(t), viewNow(t), later, later)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != PendingNotTaken {
		t.Fatalf("outcome = %v, want not taken", outcome)
	}
	if _, _, ok := pendingState(t); ok {
		t.Fatal("an intent that did not take effect is still pending")
	}
}

func TestTheWorstCaseSwitchIsUnderThreeMinutes(t *testing.T) {
	if got := cclink.WorstCaseSwitch(); got < 90*time.Second || got > 3*time.Minute {
		t.Fatalf("WorstCaseSwitch = %s, want between a minute and a half and three minutes", got)
	}
}

// astra 3. A switch that succeeds clears ITS OWN intent only. Another switch's
// intent, written ahead while this one was writing, is left for it.
func TestASwitchClearsOnlyItsOwnIntent(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "chan@example.com")
	target := seed(t, "u-2", "support@example.com")
	seed(t, "u-3", "third@example.com")
	liveAs(t, "u-1")
	kc := withItem(t, "u-1")
	kc.OnWrite = func() {
		// A second switch, to u-3, writes its intent ahead while this one is
		// writing.
		_ = strategy.WithState(time.Second, func(st *strategy.State) error {
			st.RecordPending("u-3", time.Now(), strategy.Departure{})
			return nil
		})
	}

	if _, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1"}); err != nil {
		t.Fatal(err)
	}
	if got := PendingTarget(); got != "u-3" {
		t.Fatalf("pending target = %q, want u-3: the switch to u-2 cleared another switch's intent", got)
	}
}

// astra 4, Opus B. An intent that could not be written ahead is reported WITH
// the failure -- the one case it matters: an unconfirmed keychain write and no
// intent to follow.
func TestAnIntentThatCouldNotBeWrittenIsReportedWithTheFailure(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "chan@example.com")
	target := seed(t, "u-2", "support@example.com")
	liveAs(t, "u-1")
	kc := withItem(t, "u-1")
	kc.WriteLandsThenTimesOut, kc.ReadsFailAfterWrite = true, true
	obstructStateLock(t)

	_, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1"})
	if !errors.Is(err, cclink.ErrLoginOutcomeUnknown) {
		t.Fatalf("Execute = %v, want ErrLoginOutcomeUnknown", err)
	}
	if !strings.Contains(err.Error(), "could not be remembered") {
		t.Fatalf("the error does not say the switch's intent was lost: %v", err)
	}
}

// astra 4. When the intent could not be written ahead and the outcome is
// unconfirmed, it is tried once more after the write: whatever held the state
// back may be gone.
func TestAnIntentThatCouldNotBeWrittenAheadIsTriedOnceMore(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "chan@example.com")
	target := seed(t, "u-2", "support@example.com")
	liveAs(t, "u-1")
	kc := withItem(t, "u-1")
	kc.WriteLandsThenTimesOut, kc.ReadsFailAfterWrite = true, true
	kc.OnWrite = obstructStateLock(t)

	_, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1"})
	if !errors.Is(err, cclink.ErrLoginOutcomeUnknown) {
		t.Fatalf("Execute = %v, want ErrLoginOutcomeUnknown", err)
	}
	if strings.Contains(err.Error(), "could not be remembered") {
		t.Fatalf("the retry recorded the intent and the error still says it was lost: %v", err)
	}
	if got := PendingTarget(); got != "u-2" {
		t.Fatalf("pending target = %q, want u-2: the intent was not recorded on the second try", got)
	}
}

// obstructStateLock holds the engine state's lock, as another process writing
// it would, so every write of the state times out while it can still be READ --
// an unreadable state is a different thing, and holds every stored login. It
// returns the release.
func obstructStateLock(t *testing.T) (release func()) {
	t.Helper()
	lock := mustPath(strategy.StatePath()) + ".lock"
	if err := os.MkdirAll(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	return func() { _ = os.Remove(lock) }
}

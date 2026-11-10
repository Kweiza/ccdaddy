package switcher

import (
	"errors"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// Round 5: a call that took over an earlier pending switch never clears its
// intent on a failure or a stand-down -- that call failing says nothing about
// how the earlier switch went, and the split it may have left still needs the
// intent to be repaired the right way.

func pendingOn(t *testing.T, to string) {
	t.Helper()
	if err := strategy.WithState(time.Second, func(st *strategy.State) error {
		st.RecordPending(to, time.Now(), strategy.Departure{})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestATakenOverIntentSurvivesARacedRepair(t *testing.T) {
	splitOnto(t)
	pendingOn(t, "u-2")
	decided := viewNow(t).At()
	target, _ := openStore(t).Get("u-2")
	interposeLock(t, func() { writeLive(t, itemLogin("someone-else")) })

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-2", Unattended: true, Sync: true, Expect: &decided})
	if err != nil || res.Outcome != Raced || !res.CompletedPending {
		t.Fatalf("precondition: Execute = %v, %v, completed=%v; want a raced takeover", res.Outcome, err, res.CompletedPending)
	}
	if got := PendingTarget(); got != "u-2" {
		t.Fatalf("pending target = %q, want u-2: a raced repair cleared the switch it took over", got)
	}
}

func TestATakenOverIntentSurvivesAKeychainReadFailureUnderTheLock(t *testing.T) {
	kc := splitOnto(t)
	pendingOn(t, "u-2")
	target, _ := openStore(t).Get("u-2")
	kc.FailReads(true)

	if _, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-2", Unattended: true, Sync: true}); err == nil {
		t.Fatal("precondition: the keychain read under the lock did not fail")
	}
	if got := PendingTarget(); got != "u-2" {
		t.Fatalf("pending target = %q, want u-2: a failed read cleared the switch it took over", got)
	}
}

func TestATakenOverIntentSurvivesAStoredLoginThatMoved(t *testing.T) {
	splitOnto(t)
	pendingOn(t, "u-2")
	target, _ := openStore(t).Get("u-2")
	interposeLock(t, func() {
		if err := openStore(t).Add(target, oauthBlob("RT-u-2-rotated")); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1"}); !errors.Is(err, ErrStoredLoginMoved) {
		t.Fatalf("precondition: Execute = %v, want ErrStoredLoginMoved", err)
	}
	if got := PendingTarget(); got != "u-2" {
		t.Fatalf("pending target = %q, want u-2: the retry cleared the switch it took over", got)
	}
}

package switcher

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/cclink/cclinktest"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// The second round of the split-login fix: what a repair may write, and what it
// records.

// splitOnto is the incident's machine with the item on u-2 and the file on u-1.
func splitOnto(t *testing.T) *cclinktest.Keychain {
	t.Helper()
	isolate(t)
	seed(t, "u-1", "chan@example.com")
	seed(t, "u-2", "support@example.com")
	writeLive(t, itemLogin("u-1"))
	kc := withItem(t, "u-2")
	recordActive(t, "u-1")
	return kc
}

// interposeLock runs before the real locked write, standing in for whatever
// lands while this call waits for Claude Code's locks.
func interposeLock(t *testing.T, before func()) {
	t.Helper()
	saved := activateWith
	t.Cleanup(func() { activateWith = saved })
	activateWith = func(writes cclink.StoreWrites, decide func(cclink.Blob, cclink.LoginStores) (cclink.Blob, error)) error {
		before()
		return cclink.ActivateAgainst(writes, decide)
	}
}

// D2. The stored login is read again under the lock and what is written is
// that read. One that changed while the call waited means the decision was
// about a login that is gone, and the write stands down rather than put the
// pre-lock copy -- a spent refresh token -- into both stores.
func TestARepairStandsDownWhenTheStoredLoginMovedUnderIt(t *testing.T) {
	splitOnto(t)
	target, _ := openStore(t).Get("u-2")
	interposeLock(t, func() {
		if err := openStore(t).Add(target, oauthBlob("RT-u-2-rotated")); err != nil {
			t.Fatal(err)
		}
	})

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-2", Unattended: true, Sync: true})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != Raced {
		t.Fatalf("outcome = %v, want raced: the stored login moved while the call waited", res.Outcome)
	}
	if fileHolds(t, "u-2") {
		t.Fatalf("a login read before the wait was written:\n%s", readLive(t))
	}
}

func TestAnAttendedSwitchSaysWhenTheStoredLoginMovedUnderIt(t *testing.T) {
	splitOnto(t)
	target, _ := openStore(t).Get("u-2")
	interposeLock(t, func() {
		if err := openStore(t).Add(target, oauthBlob("RT-u-2-rotated")); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-2"}); !errors.Is(err, ErrStoredLoginMoved) {
		t.Fatalf("Execute = %v, want ErrStoredLoginMoved", err)
	}
}

// D2. A decision about a split is a decision about BOTH stores. A /login that
// landed in the file while the repair waited is a change the lead store alone
// cannot show, and the repair stands down rather than write over it.
func TestARepairStandsDownWhenEitherStoreMovedUnderIt(t *testing.T) {
	splitOnto(t)
	st, err := cclink.ReadLoginStores()
	if err != nil {
		t.Fatal(err)
	}
	s := openStore(t)
	decided := ViewLogin(st, s.Accounts(), s.Credentials, s.ActiveUUID(), nil).At()
	target, _ := s.Get("u-2")
	interposeLock(t, func() { writeLive(t, itemLogin("someone-else")) })

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-2", Unattended: true, Sync: true, Expect: &decided})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != Raced {
		t.Fatalf("outcome = %v, want raced: the file changed after the decision", res.Outcome)
	}
	if !strings.Contains(string(readLive(t)), "RT-someone-else") {
		t.Fatalf("the repair wrote over a login that landed after it decided:\n%s", readLive(t))
	}
}

// D6. A repair that WRITES is held to the refresh-window check like any other
// write. Only a call that writes nothing may carry such a login through.
func TestARepairThatWritesRefusesALoginClaudeCodeWouldRefreshOnSight(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "chan@example.com")
	target := seedExpiring(t, "u-2", "support@example.com", time.Minute)
	writeLive(t, itemLogin("u-1"))
	expiring, _ := openStore(t).Credentials("u-2")
	raw, _ := expiring["claudeAiOauth"]
	withItem(t, "u-2").Set(`{"claudeAiOauth":` + string(raw) + `}`)

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-2", Now: at(fixedNow)})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != Stale {
		t.Fatalf("outcome = %v, want stale: the repair would write a login inside Claude Code's refresh window", res.Outcome)
	}
	if !fileHolds(t, "u-1") {
		t.Fatalf("a login inside the refresh window was written:\n%s", readLive(t))
	}
}

// D4. A repair is not a move: it records the account and stamps no cooldown,
// which would hold the engine on an account it may need to leave at once.
func TestASyncRepairStampsNoCooldown(t *testing.T) {
	splitOnto(t)
	target, _ := openStore(t).Get("u-2")
	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-2", Unattended: true, Sync: true})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != Switched || !res.Synced {
		t.Fatalf("outcome = %v synced=%v, want a synced switch", res.Outcome, res.Synced)
	}
	if at, _ := lastSwitch(t); !at.IsZero() {
		t.Fatal("a sync repair stamped the cooldown")
	}
	if got := openStore(t).ActiveUUID(); got != "u-2" {
		t.Fatalf("active = %q, want u-2", got)
	}
}

// An attended switch to the account Claude Code already reads first is a sync
// too: the person named the live account, and the call makes the file agree.
func TestAnAttendedSwitchToTheLiveAccountStampsNoCooldown(t *testing.T) {
	splitOnto(t)
	target, _ := openStore(t).Get("u-2")
	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-2"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != Switched || !res.Synced {
		t.Fatalf("outcome = %v synced=%v, want a synced switch", res.Outcome, res.Synced)
	}
	if at, _ := lastSwitch(t); !at.IsZero() {
		t.Fatal("making the file agree with the live account stamped the cooldown")
	}
}

// D4. A repair that completes an ENGINE switch whose outcome was unknown IS that
// switch: it records the cooldown and the departure the plan named.
func TestARepairThatCompletesAnEngineSwitchRecordsItsDeparture(t *testing.T) {
	splitOnto(t)
	target, _ := openStore(t).Get("u-2")
	_, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-2", Unattended: true,
		Departure: strategy.Departure{From: "u-1", Closing: usage.WindowFiveHour}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, to := lastSwitch(t); to != "u-2" {
		t.Fatalf("cooldown stamped for %q, want u-2", to)
	}
	path, err := strategy.StatePath()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"five_hour"`) {
		t.Fatalf("the departure was not recorded:\n%s", raw)
	}
}

// astra 1. The decision under the lock attributes the stores again, and it has
// to know what the oracle knows: an older login of the target, named by the
// oracle and by nothing else, otherwise reads as unattributable there and the
// unattended guard stands every repair down.
func TestTheOraclesOwnersReachTheDecisionUnderTheLock(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "chan@example.com")
	target := seed(t, "u-2", "support@example.com")
	writeLive(t, itemLogin("u-2"))
	withItem(t, "u-2-older")
	recordActive(t, "u-2")
	owners := func(id string) (string, bool) { return "u-2", id == "refresh:RT-u-2-older" }
	st, err := cclink.ReadLoginStores()
	if err != nil {
		t.Fatal(err)
	}
	s := openStore(t)
	decided := ViewLogin(st, s.Accounts(), s.Credentials, s.ActiveUUID(), owners).At()

	without, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-2", Unattended: true, Sync: true, Expect: &decided})
	if err != nil {
		t.Fatal(err)
	}
	if without.Outcome != Unattributed {
		t.Fatalf("precondition: without the owners the outcome = %v, want unattributed", without.Outcome)
	}
	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-2", Unattended: true, Sync: true,
		Expect: &decided, Owners: owners})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Switched {
		t.Fatalf("outcome = %v, want switched: the oracle named the item's login as the target's", res.Outcome)
	}
}

// E2. An ATTENDED switch whose keychain write could not be confirmed leaves its
// intent on disk, as the engine's does, so the repair that follows completes
// it rather than undoing it.
func TestAnAttendedUnconfirmedSwitchLeavesItsIntentOnDisk(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "chan@example.com")
	target := seed(t, "u-2", "support@example.com")
	liveAs(t, "u-1")
	kc := withItem(t, "u-1")
	kc.WriteLandsThenTimesOut, kc.ReadsFailAfterWrite = true, true

	if _, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1"}); !errors.Is(err, cclink.ErrLoginOutcomeUnknown) {
		t.Fatalf("Execute = %v, want ErrLoginOutcomeUnknown", err)
	}
	if got := PendingTarget(); got != "u-2" {
		t.Fatalf("pending target on disk = %q, want u-2", got)
	}
}

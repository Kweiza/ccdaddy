package daemon

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// THE HOLD OUTLIVES A SECOND ROTATION. The oracle names G1 as u-1's newer
// login and storing it fails, so u-1's stored refresh token -- the one G1 was
// minted from -- is spent. Claude Code then refreshes G1 into G2 before anybody
// has named G2. G1 is gone from every store, and nothing on this machine can
// say G2 is u-1's; the stored token is no less spent for it, and presenting it
// now revokes G2 -- the rotation-stomp class. So the hold lasts until the
// stored login CHANGES from the spent one, and that fact is persisted, because
// it cannot be derived once G1 has left the stores.
func TestTheHoldOutlivesASecondRotationBeforeItIsNamed(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	withItem(t, "u-2")
	writeLiveLoginRT(t, "RT-u-1-g1", tickEpoch.Add(9*time.Hour))
	recordActive(t, "u-2")

	saved := adoptInto
	t.Cleanup(func() { adoptInto = saved })
	adoptInto = func(*store.Store, store.Account, cclink.Blob) error {
		return errors.New("locking the ccdad store: timed out")
	}

	now := tickEpoch
	e := engineFor(t, tokensAreFine, noFetch)
	e.Now = func() time.Time { return now }
	fetches := 0
	e.AccessToken = func(_ context.Context, uuid string) (string, error) {
		if uuid == "u-1" {
			fetches++
		}
		return "AT-" + uuid, nil
	}
	e.ResolveOwner = func(_ context.Context, token string) (string, error) {
		if token == "AT-RT-u-1-g1" {
			return "u-1", nil
		}
		return "", errors.New("the profile endpoint did not answer")
	}

	tick(t, e)
	if e.holdSpentToken("u-1") == nil {
		t.Fatal("precondition: naming G1 and failing to store it held nothing")
	}

	// Claude Code refreshes G1 into G2, into the same store, before G2 is named.
	writeLiveLoginRT(t, "RT-u-1-g2", tickEpoch.Add(10*time.Hour))
	now = now.Add(resolveMinInterval)
	fetches = 0
	tick(t, e)
	if _, err := e.accessToken(context.Background(), "u-1"); !errors.Is(err, store.ErrStoredLoginSpent) {
		t.Errorf("u-1's token read = %v, want store.ErrStoredLoginSpent: its stored refresh token is spent", err)
	}
	if fetches != 0 {
		t.Errorf("u-1's spent stored token was read %d times after G1 was rotated away", fetches)
	}
}

// A RECORD THAT COULD NOT BE WRITTEN STILL HOLDS. Adoption failed, and so did
// the write of the record -- the state's lock held elsewhere, a full disk. The
// daemon knows the stored token is spent all the same: it holds it from
// memory, from this pass on, and writes the record on a later pass, once it
// can. Before, nothing held anything until the oracle was asked again.
func TestARecordThatCouldNotBeWrittenStillHoldsAndIsWrittenLater(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	withItem(t, "u-2")
	writeLiveLoginRT(t, "RT-u-1-g1", tickEpoch.Add(9*time.Hour))
	recordActive(t, "u-2")

	saved := adoptInto
	t.Cleanup(func() { adoptInto = saved })
	adoptInto = func(*store.Store, store.Account, cclink.Blob) error {
		return errors.New("locking the ccdad store: timed out")
	}
	lock := mustPath(store.SpentRecordPath()) + ".lock"
	if err := os.MkdirAll(lock, 0o700); err != nil {
		t.Fatal(err)
	}

	now := tickEpoch
	e := engineFor(t, tokensAreFine, noFetch)
	e.Now = func() time.Time { return now }
	e.ResolveOwner = func(_ context.Context, token string) (string, error) {
		if token == "AT-RT-u-1-g1" {
			return "u-1", nil
		}
		return "", errors.New("the profile endpoint did not answer")
	}

	tick(t, e)
	if err := e.holdSpentToken("u-1"); !errors.Is(err, store.ErrStoredLoginSpent) {
		t.Fatalf("u-1 = %v after the record could not be written, want held", err)
	}

	// The lock comes free, Claude Code rotates G1 away, and a pass goes by.
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	writeLiveLoginRT(t, "RT-u-1-g2", tickEpoch.Add(10*time.Hour))
	now = now.Add(resolveMinInterval)
	tick(t, e)
	st, err := store.LoadSpentRecord()
	if err != nil {
		t.Fatal(err)
	}
	if spent, _, ok := st.SpentStoredLogin("u-1"); !ok || spent != store.LoginHash("refresh:RT-u-1") {
		t.Fatalf("the record was not written once it could be: %q %v", spent, ok)
	}
	if err := e.holdSpentToken("u-1"); !errors.Is(err, store.ErrStoredLoginSpent) {
		t.Fatalf("u-1 = %v after the record was written, want held", err)
	}
}

// A pass that cannot read the login stores keeps the last pass's verdict on
// which stored logins are spent: an unreadable store is no evidence that a hold
// has lifted. The hold here is the stores' evidence alone -- the oracle's
// naming of a newer login in the file, with no record of a spent login -- so
// only the last verdict can keep it.
func TestAPassThatCannotReadTheStoresKeepsTheLastVerdict(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	kc := withItem(t, "u-2")
	writeLiveLoginRT(t, "RT-u-1-newer", tickEpoch.Add(9*time.Hour))
	recordActive(t, "u-2")
	if err := store.WithSpentRecord(time.Second, func(st *store.SpentRecord) error {
		st.NameLogin(store.LoginHash("refresh:RT-u-1-newer"), "u-1")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e := engineFor(t, tokensAreFine, noFetch)

	tick(t, e)
	if e.holdSpentToken("u-1") == nil {
		t.Fatal("precondition: the newer login in the file held nothing")
	}
	kc.FailReads(true)
	tick(t, e)
	if err := e.holdSpentToken("u-1"); !errors.Is(err, store.ErrStoredLoginSpent) {
		t.Fatalf("u-1 = %v after a pass that could not read the stores, want still held", err)
	}
}

// AN UNREADABLE RECORD DOES NOT HOLD THE LOGIN CLAUDE CODE IS ON. Claude Code
// refreshed u-1's login G1, which the item holds, and with its keychain write
// failing wrote G2 to the file alone. The daemon names G2 and stores it; the
// split repair then puts G2 in the item. With the record unreadable, holding
// every stored login refused that repair: the item kept the spent G1, Claude
// Code reads the item first, and its next refresh presented G1 and revoked
// G2 -- the logout v0.29.1 exists to repair. A stored login a live store holds,
// with no later login beside it, is what Claude Code is already presenting;
// holding it protects nothing.
func TestAnUnreadableRecordDoesNotHoldTheRepairOfTheNewestLogin(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	kc := withItem(t, "u-1")
	writeLiveLoginRT(t, "RT-u-1-g2", tickEpoch.Add(9*time.Hour))
	recordActive(t, "u-1")
	if err := os.WriteFile(mustPath(store.SpentRecordPath()), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	now := tickEpoch
	e := engineFor(t, tokensAreFine, noFetch)
	e.Now = func() time.Time { return now }
	e.ResolveOwner = func(_ context.Context, token string) (string, error) {
		if token == "AT-RT-u-1-g2" {
			return "u-1", nil
		}
		return "", errors.New("the profile endpoint did not answer")
	}
	var log logLines
	log.attach(e)

	for i := 0; i < 3; i++ {
		tick(t, e)
		now = now.Add(resolveMinInterval)
	}
	if secret, _ := kc.Secret(); !strings.Contains(secret, "RT-u-1-g2") {
		t.Fatalf("the item still holds the spent G1 after three passes: %s\n%q", secret, log.lines)
	}
}

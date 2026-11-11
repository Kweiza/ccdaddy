package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/cclink/cclinktest"
	"github.com/Kweiza/ccdaddy/internal/identity"
)

// Round 5: the hold on a spent stored refresh token covers every token read,
// and lifts only once the store holds the newer login.

// spendStoredLogin is Claude Code refreshing uuid's login into the keychain
// item, and the daemon naming it and failing to store it: the item holds a
// newer login of uuid, the daemon's record of it is on disk, and e takes a
// pass's verdict -- uuid's stored refresh token is spent.
func spendStoredLogin(t *testing.T, e *Engine, uuid string) {
	t.Helper()
	newer := blobExpiring("RT-"+uuid+"-newer", tickEpoch.Add(9*time.Hour))
	cclinktest.Install(t, cclinktest.NewKeychain(newer))
	failToStore(t, e, uuid, newer)
	passSees(t, e)
	if e.holdSpentToken(uuid) == nil {
		t.Fatalf("precondition: %s's stored login is not spent", uuid)
	}
}

// failToStore is the daemon's record of the identity oracle naming login as
// uuid's and storing it failing, against what uuid stores now (nameUnstored).
func failToStore(t *testing.T, e *Engine, uuid, login string) {
	t.Helper()
	s := openEngineStore(t)
	a, ok := s.Get(uuid)
	if !ok {
		t.Fatalf("no account %s", uuid)
	}
	stored, err := s.Credentials(uuid)
	if err != nil {
		t.Fatal(err)
	}
	var b cclink.Blob
	mustUnmarshal(t, login, &b)
	e.nameUnstored(a, b, stored)
}

// passSees has e take a pass's verdict on which stored logins are spent, from
// the stores and the store as they are now.
func passSees(t *testing.T, e *Engine) {
	t.Helper()
	stores, err := cclink.ReadLoginStores()
	if err != nil {
		t.Fatal(err)
	}
	e.setSpent(openEngineStore(t).SpentLogins(stores))
}

// The profile check reads the account's token through the same hold as the
// usage poll. Before, it called AccessToken directly, which may refresh.
func TestTheProfileCheckIsHeldLikeThePoll(t *testing.T) {
	isolateEngine(t)
	a := seedAccount(t, "u-1", "org-1")
	fetches := 0
	e := engineFor(t, func(context.Context, string) (string, error) {
		fetches++
		return "AT-u-1", nil
	}, noFetch)
	e.FetchProfile = func(context.Context, string) (*identity.Profile, error) {
		return &identity.Profile{AccountUUID: "u-1"}, nil
	}
	spendStoredLogin(t, e, "u-1")

	e.checkProfile(context.Background(), a, "", tickEpoch, true)
	if fetches != 0 {
		t.Fatalf("the profile check fetched u-1's token %d times past the hold", fetches)
	}
}

// The hold stays while the account's stored login is the spent one, even with
// both login stores agreeing on the newer login; it lifts once the newer login
// is stored.
func TestTheHoldStaysWhileTheSpentLoginIsStillStored(t *testing.T) {
	isolateEngine(t)
	a := seedAccount(t, "u-1", "org-1")
	newer := blobExpiring("RT-u-1-newer", tickEpoch.Add(9*time.Hour))
	writeLiveLoginRT(t, "RT-u-1-newer", tickEpoch.Add(9*time.Hour))
	cclinktest.Install(t, cclinktest.NewKeychain(newer))
	e := engineFor(t, tokensAreFine, noFetch)
	failToStore(t, e, "u-1", newer)

	passSees(t, e)
	if e.holdSpentToken("u-1") == nil {
		t.Fatal("the hold lifted while the store still holds the spent login")
	}
	var b cclink.Blob
	mustUnmarshal(t, newer, &b)
	if err := openEngineStore(t).Add(a, b); err != nil {
		t.Fatal(err)
	}
	passSees(t, e)
	if err := e.holdSpentToken("u-1"); err != nil {
		t.Fatalf("the hold did not lift once the store held the newer login: %v", err)
	}
}

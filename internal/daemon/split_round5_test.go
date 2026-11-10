package daemon

import (
	"context"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/identity"
)

// Round 5: the hold on a spent stored refresh token covers every token read,
// and lifts only once the store holds the newer login.

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
	e.holdUntilStored("u-1", "refresh:RT-u-1-newer")

	e.checkProfile(context.Background(), a, "", tickEpoch, true)
	if fetches != 0 {
		t.Fatalf("the profile check fetched u-1's token %d times past the hold", fetches)
	}
}

// The hold lifts only when the account's STORED credential is the newer login
// (or has superseded it) -- not because the login stores agree.
func TestTheHoldLiftsOnlyOnceTheStoreHoldsTheNewerLogin(t *testing.T) {
	isolateEngine(t)
	a := seedAccount(t, "u-1", "org-1")
	liveAs(t, "u-1")
	withItem(t, "u-1") // both stores agree; the store still holds the old login
	e := engineFor(t, tokensAreFine, noFetch)
	e.holdUntilStored("u-1", "refresh:RT-u-1-newer")

	s := openEngineStore(t)
	e.releaseStored(s)
	if e.holdSpentToken("u-1") == nil {
		t.Fatal("the hold lifted while the store still holds the spent login")
	}
	if err := s.Add(a, oauthBlob("RT-u-1-newer")); err != nil {
		t.Fatal(err)
	}
	e.releaseStored(openEngineStore(t))
	if err := e.holdSpentToken("u-1"); err != nil {
		t.Fatalf("the hold did not lift once the store held the newer login: %v", err)
	}
}

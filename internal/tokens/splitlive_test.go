package tokens

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink/cclinktest"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// An account the CREDENTIALS FILE holds behind a keychain item naming somebody
// else is live: Claude Code falls back to the file whenever its keychain read
// fails, so a split login's file account can be the one a session is spending.
// Refreshing it here spends the refresh token that session holds and revokes
// its grant -- rotation-stomp invariant 1, through the store this check used
// not to look at.
func TestAnAccountLiveInTheFileBehindTheItemIsNotRefreshed(t *testing.T) {
	isolate(t)
	expired := oauthRecord("a1", "r1", time.Now().Add(-time.Hour), "")
	seed(t, "u-1", expired)
	writeLive(t, expired)
	cclinktest.Install(t, cclinktest.NewKeychain(
		`{"claudeAiOauth":`+string(oauthRecord("a2", "r2", time.Now().Add(time.Hour), ""))+`}`))

	src := sourceFor(t, refusingEndpoint(t))
	got, err := src.AccessToken(context.Background(), "u-1")
	if !errors.Is(err, ErrLiveTokenStale) {
		t.Fatalf("AccessToken = (%q, %v), want ErrLiveTokenStale and no request spent", got, err)
	}
	if rec := storedRecord(t, "u-1"); rec.RefreshToken != "r1" {
		t.Fatalf("the stored refresh token moved to %q; the grant the file's session holds was spent", rec.RefreshToken)
	}
}

// A stored login a newer one in a live store has superseded is neither
// refreshed nor handed out: the refresh token behind it is already spent, and
// presenting it can revoke the newer login. This is the check every process
// gets, the daemon's poll and a `ccdad status --refresh` alike, because it is
// derived from the stores and the identity oracle's record on disk rather than
// held in the daemon's memory.
func TestAStoredLoginANewerOneHasSpentIsNotPresented(t *testing.T) {
	isolate(t)
	expired := oauthRecord("a1", "r1", time.Now().Add(-time.Hour), "")
	seed(t, "u-1", expired)
	writeLive(t, oauthRecord("a1-newer", "r1-newer", time.Now().Add(8*time.Hour), ""))
	if err := store.WithSpentRecord(time.Second, func(st *store.SpentRecord) error {
		st.NameLogin(store.LoginHash("refresh:r1-newer"), "u-1")
		st.RecordSpentStoredLogin("u-1", store.LoginHash("refresh:r1"), store.LoginHash("refresh:r1-newer"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	src := sourceFor(t, refusingEndpoint(t))
	got, err := src.AccessToken(context.Background(), "u-1")
	if !errors.Is(err, store.ErrStoredLoginSpent) {
		t.Fatalf("AccessToken = (%q, %v), want store.ErrStoredLoginSpent and no request spent", got, err)
	}
	if rec := storedRecord(t, "u-1"); rec.RefreshToken != "r1" {
		t.Fatalf("the stored refresh token moved to %q", rec.RefreshToken)
	}

	// Claude Code rotates the newer login again before anybody names what
	// replaced it: nothing in the store connects u-1 to it, and the stored
	// token is exactly as spent.
	writeLive(t, oauthRecord("a1-again", "r1-again", time.Now().Add(9*time.Hour), ""))
	got, err = src.AccessToken(context.Background(), "u-1")
	if !errors.Is(err, store.ErrStoredLoginSpent) {
		t.Fatalf("after a second rotation AccessToken = (%q, %v), want store.ErrStoredLoginSpent", got, err)
	}
}

// A store that cannot be read does not weaken the record: the stored login is
// recorded as spent whatever the keychain says, so its access token is not
// handed out either.
func TestARecordedSpentLoginIsNotPresentedWhenTheStoresCannotBeRead(t *testing.T) {
	isolate(t)
	seed(t, "u-1", oauthRecord("a1", "r1", time.Now().Add(time.Hour), ""))
	if err := store.WithSpentRecord(time.Second, func(st *store.SpentRecord) error {
		st.RecordSpentStoredLogin("u-1", store.LoginHash("refresh:r1"), store.LoginHash("refresh:r1-newer"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	kc := cclinktest.NewKeychain(`{}`)
	kc.FailReads(true)
	cclinktest.Install(t, kc)

	got, err := sourceFor(t, refusingEndpoint(t)).AccessToken(context.Background(), "u-1")
	if !errors.Is(err, store.ErrStoredLoginSpent) {
		t.Fatalf("AccessToken = (%q, %v), want store.ErrStoredLoginSpent", got, err)
	}
}

// THE CHECK AND THE USE ARE ONE READ. The daemon stores u-1's newer login while
// this call is reading the login stores, after it read the spent one. A check
// that read the store again would see the newer login, find nothing held, and
// let the call refresh the spent copy it had in hand.
func TestTheHoldIsDecidedOnTheStoredLoginThatIsPresented(t *testing.T) {
	isolate(t)
	seed(t, "u-1", oauthRecord("a1", "r1", time.Now().Add(-time.Hour), ""))
	if err := store.WithSpentRecord(time.Second, func(st *store.SpentRecord) error {
		st.RecordSpentStoredLogin("u-1", store.LoginHash("refresh:r1"), store.LoginHash("refresh:r1-newer"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	kc := cclinktest.NewKeychain(`{}`)
	adopted := false
	kc.OnRead = func() {
		if !adopted {
			adopted = true
			seed(t, "u-1", oauthRecord("a1-newer", "r1-newer", time.Now().Add(8*time.Hour), ""))
		}
	}
	cclinktest.Install(t, kc)

	got, err := sourceFor(t, refusingEndpoint(t)).AccessToken(context.Background(), "u-1")
	if err == nil && got != "a1-newer" {
		t.Fatalf("AccessToken = %q, want the newer login's token or a refusal: the spent copy was used", got)
	}
}

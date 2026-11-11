package tokens

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink/cclinktest"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
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
	if err := strategy.WithState(time.Second, func(st *strategy.State) error {
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

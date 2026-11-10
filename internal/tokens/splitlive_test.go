package tokens

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
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
	t.Cleanup(cclink.SetKeychainForTest(cclink.NewFakeKeychain(
		`{"claudeAiOauth":` + string(oauthRecord("a2", "r2", time.Now().Add(time.Hour), "")) + `}`)))

	src := sourceFor(t, refusingEndpoint(t))
	got, err := src.AccessToken(context.Background(), "u-1")
	if !errors.Is(err, ErrLiveTokenStale) {
		t.Fatalf("AccessToken = (%q, %v), want ErrLiveTokenStale and no request spent", got, err)
	}
	if rec := storedRecord(t, "u-1"); rec.RefreshToken != "r1" {
		t.Fatalf("the stored refresh token moved to %q; the grant the file's session holds was spent", rec.RefreshToken)
	}
}

// Opus S3. Claude Code refreshed u-1 INTO THE FILE -- its keychain write
// failed -- so the file holds u-1's newer pair, which no account has stored
// yet, while the item holds somebody else. u-1's stored refresh token is
// already spent; refreshing it here can revoke the family, the session's new
// pair included. Until the newer login is named and adopted, u-1 is not
// refreshed.
func TestAnAccountThatMayOwnANewerUnnamedLoginIsNotRefreshed(t *testing.T) {
	isolate(t)
	seed(t, "u-1", oauthRecord("a1", "r1", time.Now().Add(-time.Hour), ""))
	writeLive(t, oauthRecord("a1-refreshed", "r1-refreshed", time.Now().Add(8*time.Hour), ""))
	t.Cleanup(cclink.SetKeychainForTest(cclink.NewFakeKeychain(
		`{"claudeAiOauth":` + string(oauthRecord("a2", "r2", time.Now().Add(time.Hour), "")) + `}`)))

	src := sourceFor(t, refusingEndpoint(t))
	if got, err := src.AccessToken(context.Background(), "u-1"); !errors.Is(err, ErrLivenessUnknown) {
		t.Fatalf("AccessToken = (%q, %v), want ErrLivenessUnknown and no request spent", got, err)
	}
	if rec := storedRecord(t, "u-1"); rec.RefreshToken != "r1" {
		t.Fatalf("the stored refresh token moved to %q", rec.RefreshToken)
	}
}

package tokens

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink/cclinktest"
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

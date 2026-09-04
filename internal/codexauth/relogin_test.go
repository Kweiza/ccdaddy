package codexauth

import (
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/store"
)

func codexBlob(refresh string) cclink.Blob {
	return Credential{AccessToken: "AT", RefreshToken: refresh, AccountID: "acct", UserID: "cx-1"}.ToBlob()
}

func TestAnUnmarkedAccountDoesNotNeedALogin(t *testing.T) {
	a := store.Account{UUID: "cx-1"}
	if NeedsRelogin(a, codexBlob("RT-1")) {
		t.Fatal("NeedsRelogin = true on an account with no mark")
	}
}

func TestAMarkNamingTheStoredTokenNeedsALogin(t *testing.T) {
	a := store.Account{UUID: "cx-1", CodexReloginFor: RefreshTokenHash("RT-1")}
	if !NeedsRelogin(a, codexBlob("RT-1")) {
		t.Fatal("NeedsRelogin = false while the mark names the token the account still holds")
	}
}

// The whole point of hashing the token rather than setting a flag: a later
// `ccdad add codex` stores a new refresh token, and the mark stops matching
// with nothing having to clear it. A stale mark that survived a re-login would
// hold an account out of rotation for good.
func TestAMarkStopsMatchingOnceTheTokenHasMovedOn(t *testing.T) {
	a := store.Account{UUID: "cx-1", CodexReloginFor: RefreshTokenHash("RT-1")}
	if NeedsRelogin(a, codexBlob("RT-2")) {
		t.Fatal("NeedsRelogin = true after a re-login stored a different token")
	}
}

// A blob with no codex credential in it at all. The eligibility rule excludes
// such an account on the "has a codex credential" term, so answering true here
// would be a second refusal wearing the wrong reason.
func TestABlobWithNoCodexCredentialDoesNotNeedALogin(t *testing.T) {
	a := store.Account{UUID: "cx-1", CodexReloginFor: RefreshTokenHash("RT-1")}
	if NeedsRelogin(a, cclink.Blob{}) {
		t.Fatal("NeedsRelogin = true on a blob holding no codex credential")
	}
}

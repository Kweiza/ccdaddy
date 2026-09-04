package codexauth

import (
	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// NeedsRelogin answers store.Account.NeedsRelogin from the account's stored
// credential blob, which is where the CURRENT refresh token is.
//
// The mark is a hash of the refresh token whose grant the endpoint rejected,
// and it means "needs a login" only while the account still holds that token.
// That is what makes a re-login self-clearing: `ccdad add codex` stores a new
// token, the hash stops matching, and nothing had to remember to clear a flag.
//
// A blob that carries no codex credential, or one that cannot be parsed,
// answers FALSE. Such an account is already excluded by the eligibility rule's
// "has a codex credential" term, and answering true here would put a second
// refusal on it wearing the wrong reason -- the surface would tell a user to
// run `ccdad add codex` about an account whose real problem is a missing file.
func NeedsRelogin(a store.Account, b cclink.Blob) bool {
	if a.CodexReloginFor == "" {
		return false
	}
	c, ok, err := FromBlob(b)
	if err != nil || !ok || c.RefreshToken == "" {
		return false
	}
	return a.NeedsRelogin(RefreshTokenHash(c.RefreshToken))
}

package cli

import (
	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// spentLogins is every account whose stored refresh token is spent
// (store.Account.StoredLoginSpent), from the stores as this command reads
// them. Stores that cannot be read are no evidence either way, and the
// daemon's record decides alone: a keychain that will not answer -- locked
// over SSH, or past the lookup bound on a loaded Mac -- must never let a login
// the record holds through.
//
// It is the same decision the daemon applies to its polls, probes and plan,
// tokens.Source to a refresh and switcher.Execute to a switch -- taken against
// the identity oracle's record on disk, which is how a command sees a hold the
// daemon places.
func spentLogins(s *store.Store) map[string]bool {
	stores, _ := cclink.ReadLoginStores() // the zero value when it failed: no evidence
	return s.SpentLogins(stores, store.LoadOracleRecord())
}

// spentLoginReason is why a command will not present a's stored login, in the
// words every such command uses after its own "Not ...ing X:".
func spentLoginReason(a store.Account) string {
	return "Claude Code holds a newer login of " + a.Label() + " than ccdad has stored, so its stored " +
		"refresh token is already spent, and presenting it could revoke the newer login. The daemon " +
		"stores the newer login as soon as it can; `ccdad doctor` shows which login store holds what"
}

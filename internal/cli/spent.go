package cli

import (
	"github.com/Kweiza/ccdaddy/internal/store"
)

// spentLoginReason is why a command will not present a's stored login, in the
// words every such command uses after its own "Not ...ing X:".
func spentLoginReason(a store.Account) string {
	return "Claude Code holds a newer login of " + a.Label() + " than ccdad has stored, so its stored " +
		"refresh token is already spent, and presenting it could revoke the newer login. The daemon " +
		"stores the newer login as soon as it can; `ccdad doctor` shows which login store holds what"
}

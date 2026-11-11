package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/switcher"
)

// spentLoginReason is why a's stored login is not presented, and what lifts
// the hold, in the words every command uses after its own "Not ...ing X:".
//
// What lifts it depends on why it holds (store.Hold) and, for a hold the
// record keeps, on what the login stores hold now -- v, nil when they could not
// be read. A newer login still in a store under the oracle's name is the
// daemon's to store. One that has left every store may be back as a login
// nobody can name yet, refreshed again, which the daemon names and stores if
// it is a's; when no store holds anything unnamed, nothing will ever store it,
// and logging a in again is the only way out. Saying "the daemon stores it"
// there sent people to wait for something that would not happen.
func spentLoginReason(a store.Account, hold store.Hold, v *switcher.LoginView) string {
	label := a.Label()
	switch hold {
	case store.HeldUnreadableRecord:
		path, err := strategy.StatePath()
		if err != nil {
			path = strategy.StateFileName
		}
		return "the engine state that records which stored logins are spent cannot be read, so no stored " +
			"login is presented until it can. Repair or remove " + path + "; `ccdad doctor` says why it " +
			"cannot be read"
	case store.HeldByNewerLogin:
		return "Claude Code holds a newer login of " + label + " than ccdad has stored, so its stored refresh " +
			"token is already spent, and presenting it could revoke the newer login. The daemon stores the " +
			"newer login as soon as it can"
	}
	spent := label + "'s stored refresh token was already spent by a newer login ccdad could not store, " +
		"and presenting it could revoke that login and every one refreshed from it since"
	relogin := "log " + label + " in again with `ccdad add claude`"
	switch {
	case v == nil:
		return spent + ". Whether a login store still holds that login cannot be told while the stores " +
			"cannot be read; if none does, " + relogin
	case holdsUnnamedLogin(*v):
		return spent + ". Claude Code holds a login ccdad cannot name yet, which may be that login refreshed " +
			"again: the daemon asks the profile endpoint whose it is, and stores it if it is " + label +
			"'s. If it is not, " + relogin
	}
	return spent + ". No login store holds that login any more, so nothing will store it: " + relogin
}

// holdsUnnamedLogin reports whether a login store holds a login this store
// cannot name: one the identity oracle has still to name.
func holdsUnnamedLogin(v switcher.LoginView) bool {
	return (v.HasItem && v.Item.State == switcher.LiveUnattributed) || v.File.State == switcher.LiveUnattributed
}

// loginView is the stores as a command read them, attributed against s, or nil
// when the read failed.
func loginView(s *store.Store, stores cclink.LoginStores, err error) *switcher.LoginView {
	if err != nil {
		return nil
	}
	v := switcher.ViewLogin(stores, s.Accounts(), s.Credentials, s.ActiveUUID(), nil)
	return &v
}

// spentHoldOf is the hold a refusal carries, and whether it carries one.
func spentHoldOf(err error) (store.Hold, bool) {
	var se *store.SpentError
	if errors.As(err, &se) {
		return se.Hold, true
	}
	return store.NotHeld, false
}

// spentLoginNotices is one line per account whose stored login no command
// presents, for status: such an account is out of every plan and every poll,
// and a hold only a new login lifts would otherwise go unseen.
func spentLoginNotices(s *store.Store, stores cclink.LoginStores, storesErr error) []string {
	holds := s.SpentLogins(stores, store.LoadOracleRecord()) // stores are the zero value when the read failed
	v := loginView(s, stores, storesErr)
	var out []string
	for _, a := range s.Accounts() {
		if hold := holds[a.UUID]; hold.Held() {
			out = append(out, fmt.Sprintf("Not presenting %s's stored login: %s.\n", a.Label(), spentLoginReason(a, hold, v)))
		}
	}
	return out
}

// checkSpentLogins reports every account whose stored login no command
// presents, and what lifts each hold (spentLoginReason). It is a warning: the
// hold is what keeps a spent refresh token from revoking a newer login, but the
// account is out of every plan, every poll and every probe while it lasts, and
// some holds only a new login lifts.
func checkSpentLogins(root string, usable, accountsUsable bool, stores cclink.LoginStores, storesErr error) check {
	const name = "spent-logins"
	if !usable {
		return check{name, levelSkipped, "there is no store to check"}
	}
	if !accountsUsable {
		return check{name, levelSkipped, noAccountList}
	}
	accounts, err := store.AccountsAt(root)
	if err != nil {
		return check{name, levelFail, fmt.Sprintf("the account list cannot be read: %v", err)}
	}
	recorded, _ := store.ActiveAt(root)
	lookup := func(uuid string) (cclink.Blob, error) { return store.CredentialsAt(root, uuid) }
	var v *switcher.LoginView
	if storesErr == nil {
		view := switcher.ViewLogin(stores, accounts, lookup, recorded, nil)
		v = &view
	} else {
		stores = cclink.LoginStores{} // no evidence: the record decides alone
	}
	rec := store.LoadOracleRecord()
	var held []string
	for _, a := range accounts {
		if a.Provider != provider.Claude {
			continue
		}
		stored, _ := lookup(a.UUID)
		if hold := a.StoredLoginSpent(stored, stores, rec); hold.Held() {
			held = append(held, fmt.Sprintf("%s (%s): %s", a.Label(), a.UUID, spentLoginReason(a, hold, v)))
		}
	}
	if len(held) == 0 {
		return check{name, levelOK, "no stored login is held back as spent"}
	}
	return check{name, levelWarn, "ccdad presents none of these stored logins -- to a refresh, a switch, " +
		"`ccdad run` or a probe -- and the engine plans around them: " + strings.Join(held, "; ")}
}

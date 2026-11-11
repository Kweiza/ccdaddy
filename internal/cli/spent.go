package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/switcher"
)

// spentLoginReason is why a's stored login is not presented, and what lifts
// the hold, in the words every command uses after its own "Not ...ing X:" --
// store.HoldReason's, with what the login stores hold now taken from v (nil
// when they could not be read).
func spentLoginReason(a store.Account, hold store.Hold, v *switcher.LoginView) string {
	now := store.StoresUnknown
	switch {
	case v == nil:
	case holdsUnnamedLogin(*v):
		now = store.StoresHoldUnnamed
	default:
		now = store.StoresHoldNoneUnnamed
	}
	return store.HoldReason(hold, a.Label(), now)
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
// presents, and what lifts each hold (spentLoginReason).
//
// A hold the record keeps is a warning: it is what keeps a spent refresh token
// from revoking a newer login, but the account is out of every plan, every
// poll and every probe while it lasts, and some holds only a new login lifts.
// A record that cannot be read is a failure, and its row names every account
// the record may hold, for the advice that comes with it
// (store.SpentRecordAdvice).
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
	owners := store.OwnersOf(accounts, lookup)
	var held, unreadable []string
	for _, a := range accounts {
		if a.Provider != provider.Claude {
			continue
		}
		stored, lerr := lookup(a.UUID)
		// A stored login that cannot be read is no evidence it is not spent:
		// the record may hold it, and once the file reads again removing the
		// record would let it be presented. It is on the list like the rest.
		if lerr != nil && rec.LoadError() != nil {
			unreadable = append(unreadable, fmt.Sprintf("%s (%s, whose stored login cannot be read either)",
				a.Label(), a.UUID))
			continue
		}
		switch hold := a.StoredLoginSpent(stored, stores, owners, rec); hold {
		case store.NotHeld:
		case store.HeldUnreadableRecord:
			unreadable = append(unreadable, fmt.Sprintf("%s (%s)", a.Label(), a.UUID))
		default:
			held = append(held, fmt.Sprintf("%s (%s): %s", a.Label(), a.UUID, spentLoginReason(a, hold, v)))
		}
	}
	// AN UNREADABLE RECORD is a failure, and its advice names every account
	// it may hold: each must be logged in again before the file can go, or the
	// file restored (store.SpentRecordAdvice).
	if lerr := rec.LoadError(); lerr != nil {
		who := "each account it may hold as spent"
		if len(unreadable) > 0 {
			who += ": " + strings.Join(unreadable, ", ")
		}
		return check{name, levelFail, fmt.Sprintf("the record of which stored logins are spent cannot be "+
			"read (%v), so no stored login Claude Code is not already using is presented -- to a refresh, a "+
			"switch, `ccdad run` or a probe -- until it can. %s", lerr, store.SpentRecordAdvice(who))}
	}
	if len(held) == 0 {
		return check{name, levelOK, "no stored login is held back as spent"}
	}
	return check{name, levelWarn, "ccdad presents none of these stored logins -- to a refresh, a switch, " +
		"`ccdad run` or a probe -- and the engine plans around them: " + strings.Join(held, "; ")}
}

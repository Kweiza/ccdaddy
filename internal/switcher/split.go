package switcher

import (
	"strings"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// A SPLIT LOGIN is Claude Code's two login stores, or ccdad's record of which
// account is live, naming different accounts.
//
// On macOS a login lives in two places: the keychain item, which Claude Code
// reads first, and ~/.claude/.credentials.json, which it falls back to whenever
// that read fails -- and on a loaded Mac it fails often, because Claude Code
// gives `security` 10 s. A switch writes both, item first; a write the item
// accepted and the file never received leaves the item on one account and the
// file on another. On 2026-11-10 that is what a timed-out keychain write did:
// every ccdad command read the item and saw the new account, and Claude Code,
// falling back to the file, spent the old one for three hours.
//
// The FILE can be ahead of the item too. Claude Code writes its login to the
// file whenever its own keychain write fails -- a /login, or its token refresh
// -- so on the same loaded Mac the file can hold the newer login, of the same
// account or of another.
//
// So "who is live" has three answers to compare, and this file compares them.
// It is pure: the reads are cclink.ReadLoginStores (the daemon, once a tick)
// and the stores handed to Execute's decision under Claude Code's locks.

// StoreHolder is what one login store holds.
type StoreHolder struct {
	Account store.Account
	State   LiveState
	// ID is the login's cclink.LoginIdentity, "" when the store holds none or
	// could not be read. Two stores with equal IDs hold the same login.
	ID string
	// Expires is the login's expiry, zero when it does not say. Every grant
	// lasts as long, so the later expiry is the login issued later.
	Expires time.Time
}

// known reports a store whose OWNER is established: by its refresh token
// matching a stored snapshot (LiveManaged), or by the identity oracle
// (LiveResolved).
func (h StoreHolder) known() bool { return h.State == LiveManaged || h.State == LiveResolved }

// Owners names the managed account a login belongs to, by its LoginIdentity,
// when the identity oracle has already answered for it. Nil knows nothing.
type Owners func(id string) (uuid string, ok bool)

// LoginView is each login store's account beside the account ccdad records as
// active (accounts.toml's active_uuid).
type LoginView struct {
	// HasItem is whether there is a keychain item. Without one the file is the
	// only store, and Item is not consulted.
	HasItem bool
	Item    StoreHolder
	File    StoreHolder
	// Recorded is the account ccdad last recorded as active. Its UUID is ""
	// when nothing is recorded; an account no longer in the store keeps only
	// its UUID.
	Recorded store.Account
}

// ViewLogin attributes each store on its own and sets them beside the record.
// owners may be nil.
func ViewLogin(st cclink.LoginStores, accounts []store.Account, lookup Lookup, recorded string, owners Owners) LoginView {
	v := LoginView{HasItem: st.HasItem, Recorded: store.Account{UUID: recorded}}
	if st.HasItem {
		v.Item = holderOf(st.Item, accounts, lookup, owners)
	}
	if st.FileErr != nil {
		v.File.State = LiveUnreadable
	} else {
		v.File = holderOf(st.File, accounts, lookup, owners)
	}
	for _, a := range accounts {
		if a.UUID == recorded {
			v.Recorded = a
		}
	}
	return v
}

func holderOf(b cclink.Blob, accounts []store.Account, lookup Lookup, owners Owners) StoreHolder {
	h := StoreHolder{ID: cclink.LoginIdentity(b)}
	h.Expires, _ = cclink.LoginExpiry(b)
	h.Account, h.State = LiveStateOf(b, accounts, lookup)
	if h.State != LiveUnattributed {
		return h
	}
	// An older generation of a managed account's login is named by the
	// account's own history -- no oracle, no network -- and anything else by
	// what the oracle has already said.
	for _, a := range accounts {
		if a.Superseded(h.ID) {
			h.Account, h.State = a, LiveResolved
			return h
		}
	}
	if owners != nil {
		if uuid, ok := owners(h.ID); ok {
			for _, a := range accounts {
				if a.UUID == uuid {
					h.Account, h.State = a, LiveResolved
				}
			}
		}
	}
	return h
}

// StoresAt is what the two stores held when a decision was made, by login
// identity. A write that acts on a decision checks it again under Claude
// Code's locks and stands down if either store moved (Request.Expect).
type StoresAt struct {
	HasItem bool
	ItemID  string
	FileID  string
}

// At is the view's StoresAt.
func (v LoginView) At() StoresAt {
	return StoresAt{HasItem: v.HasItem, ItemID: v.Item.ID, FileID: v.File.ID}
}

// Lead is the store Claude Code reads first: the item when there is one.
func (v LoginView) Lead() StoreHolder {
	if v.HasItem {
		return v.Item
	}
	return v.File
}

// AllOn reports whether every store AND the record name uuid: the only state
// in which a switch to uuid has nothing at all to do.
func (v LoginView) AllOn(uuid string) bool {
	return v.StoresOn(uuid) && v.Recorded.UUID == uuid
}

// StoresOn reports whether the stores hold uuid's stored login and nothing
// else. It is what decides that a switch to uuid writes no credential.
//
// With an item, the file must hold the SAME login or none. A file with a
// different login -- another account's, an older or newer one of the same
// account, or one this store cannot name -- is a store a switch to uuid has to
// write, and it is the state in which `ccdad switch` used to say "Already on".
// A missing file is not: some Claude Code releases delete it behind an item,
// and creating it is not a switch's business. This is the same line Split and
// Unresolved draw, so a machine `switch` would rewrite is one the daemon and
// doctor do not call fine.
func (v LoginView) StoresOn(uuid string) bool {
	if !v.HasItem {
		return v.File.State == LiveManaged && v.File.Account.UUID == uuid
	}
	if v.Item.State != LiveManaged || v.Item.Account.UUID != uuid {
		return false
	}
	return v.File.State == LiveNone || v.File.ID == v.Item.ID
}

// StoresDisagree reports two stores holding different logins whose owners are
// both established, with the newer one -- the one a repair copies -- held as
// ccdad's stored login for its account. That is a split the engine can repair:
// it knows what it is copying and whose it is.
//
// The newer one has to be the stored snapshot because the repair installs the
// snapshot (read again under the lock); a newer login the store has not taken
// in yet is adopted first, by the daemon's oracle step, and repaired on the
// pass after.
func (v LoginView) StoresDisagree() bool {
	if !v.HasItem || !v.Item.known() || !v.File.known() || v.Item.ID == v.File.ID {
		return false
	}
	return v.newer().State == LiveManaged
}

// Unresolved reports two stores holding different logins where the engine
// cannot repair: one of them cannot be named, or the newer one is not yet
// ccdad's stored login. Nothing is written over it; the daemon asks the
// identity oracle, and doctor warns.
//
// It is the case the first version of this file waved through as "Claude Code
// refreshed the item and left the file behind". That is one reading. Claude
// Code writing its newer login to the FILE when its keychain write failed is
// another, and the two cannot be told apart without naming the login.
func (v LoginView) Unresolved() bool {
	if !v.HasItem || v.Item.ID == "" || v.File.ID == "" || v.Item.ID == v.File.ID {
		return false
	}
	return !v.StoresDisagree()
}

// RecordBehind reports stores that agree on a named account while ccdad's
// record names another. Nothing needs installing; the record is corrected.
func (v LoginView) RecordBehind() bool {
	if v.StoresDisagree() || v.Unresolved() {
		return false
	}
	lead := v.Lead()
	return lead.State == LiveManaged && v.Recorded.UUID != lead.Account.UUID
}

// Split reports a login the engine repairs: stores that disagree, or a record
// behind agreeing stores.
func (v LoginView) Split() bool {
	return v.StoresDisagree() || v.RecordBehind()
}

// newer is the store holding the more recently issued login, the item on a
// tie or when either expiry is unknown.
func (v LoginView) newer() StoreHolder {
	if !v.Item.Expires.IsZero() && v.File.Expires.After(v.Item.Expires) {
		return v.File
	}
	return v.Item
}

// RepairTarget is the account a split is repaired TOWARDS: the one whose
// login was issued most recently, the keychain item's on a tie.
//
// Recency rather than a fixed store, because either store can be ahead:
//
//   - The item is ahead after a ccdad switch whose file write never happened
//     -- the 2026-11-10 incident -- and after Claude Code's /login or refresh,
//     which write the item and skip the file.
//   - The file is ahead after any of those whose KEYCHAIN write failed: Claude
//     Code's store then writes the file instead, and on the loaded Mac where
//     splits happen its keychain writes fail as often as ccdad's do.
//
// Whichever was issued later is the most recent decision anyone made, and the
// older login is the one that has been superseded -- for the same account its
// refresh token is already spent. The record is never the target: ccdad writes
// it only after both stores, so it is the oldest of the three. The tie goes to
// the item because it is the store Claude Code reads first.
func (v LoginView) RepairTarget() store.Account {
	if v.StoresDisagree() {
		return v.newer().Account
	}
	return v.Lead().Account
}

// Describe names which store holds which account, in a user's terms: email
// and uuid, never a token.
func (v LoginView) Describe() string {
	var parts []string
	if v.HasItem {
		parts = append(parts, "the keychain item Claude Code reads first holds "+v.Item.describe())
	}
	parts = append(parts, "the credentials file holds "+v.File.describe())
	parts = append(parts, "ccdad records "+v.RecordedName()+" as live")
	return strings.Join(parts, "; ")
}

// RecordedName is the recorded account as Describe names it, or "no account".
func (v LoginView) RecordedName() string {
	if v.Recorded.UUID == "" {
		return "no account"
	}
	return accountName(v.Recorded)
}

func (h StoreHolder) describe() string {
	switch h.State {
	case LiveManaged:
		return accountName(h.Account)
	case LiveResolved:
		return "a login of " + accountName(h.Account) + " that ccdad has not stored"
	case LiveNone:
		return "no login"
	case LiveUnreadable:
		return "a login ccdad could not read"
	}
	return "a login this store cannot name"
}

// accountName is an account as a person and a script can both find it again.
func accountName(a store.Account) string {
	if a.Email == "" && a.Alias == "" {
		return a.UUID
	}
	return a.Label() + " (" + a.UUID + ")"
}

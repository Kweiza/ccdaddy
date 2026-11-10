package switcher

import (
	"strings"

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
// So "who is live" has three answers to compare, and this file compares them.
// It is pure: the reads are cclink.ReadLoginStores (the daemon, once a tick)
// and the stores handed to Execute's decision under Claude Code's locks.

// StoreHolder is what one login store holds, in LiveStateOf's four answers.
type StoreHolder struct {
	Account store.Account
	State   LiveState
}

func (h StoreHolder) named() bool { return h.State == LiveManaged }

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
func ViewLogin(st cclink.LoginStores, accounts []store.Account, lookup Lookup, recorded string) LoginView {
	v := LoginView{HasItem: st.HasItem, Recorded: store.Account{UUID: recorded}}
	if st.HasItem {
		v.Item.Account, v.Item.State = LiveStateOf(st.Item, accounts, lookup)
	}
	if st.FileErr != nil {
		v.File.State = LiveUnreadable
	} else {
		v.File.Account, v.File.State = LiveStateOf(st.File, accounts, lookup)
	}
	for _, a := range accounts {
		if a.UUID == recorded {
			v.Recorded = a
		}
	}
	return v
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

// StoresOn reports whether every login store names uuid. It is what decides
// that a switch to uuid writes no credential: the item naming it while the file
// names someone else is exactly the state a switch to it must repair, and the
// one in which `ccdad switch` used to say "Already on". With the stores on
// uuid and only the record behind, there is nothing to install and the record
// is what gets corrected.
func (v LoginView) StoresOn(uuid string) bool {
	if v.HasItem && !(v.Item.named() && v.Item.Account.UUID == uuid) {
		return false
	}
	return v.File.named() && v.File.Account.UUID == uuid
}

// StoresDisagree reports that both stores hold a login this store can name, and
// they name different accounts.
func (v LoginView) StoresDisagree() bool {
	return v.HasItem && v.Item.named() && v.File.named() && v.Item.Account.UUID != v.File.Account.UUID
}

// Split reports a login the engine repairs: the stores name different accounts,
// or they agree and the record names another.
//
// It is decided only when the store Claude Code reads first can be NAMED. A
// lead that is unattributable is very often a managed account Claude Code has
// just rotated, and an unreadable one is not known at all; a repair is a write,
// and writing over a login this store cannot name is the rotation stomp. Those
// states have their own handling -- the profile oracle, and standing down --
// and a split is judged once they have resolved.
//
// A file this store cannot name beside a named item is NOT a split. Claude
// Code refreshes the login in the item and leaves the file behind it, so that
// is the ordinary state of a machine whose item is in use, and calling it split
// would rewrite the file after every refresh.
func (v LoginView) Split() bool {
	lead := v.Lead()
	if !lead.named() {
		return false
	}
	return v.StoresDisagree() || v.Recorded.UUID != lead.Account.UUID
}

// RepairTarget is the account a split is repaired TOWARDS: the lead store's,
// which is the keychain item's whenever there is one.
//
// Why the item and not the file, nor the record:
//
//   - The item is the store every writer reaches first. ccdad writes it before
//     the file (cclink.writeMerged), and Claude Code's /login and its own token
//     refresh write it and skip the file. So when the item and the file
//     disagree, the item holds the most recent decision anyone made -- the
//     switch whose file write never happened, or the user's own /login -- and
//     the file holds the one before it.
//   - The record is older still: ccdad writes it only after both stores have
//     been written, so a switch that stopped part-way never reaches it.
//     Repairing towards it would undo a switch that took effect, and then the
//     engine would have to redo it, through the same slow keychain.
//   - That the incident's sessions spent the FILE's account does not make the
//     file the intent. They spent it because Claude Code's keychain read was
//     failing under load; the repair puts one login in both stores, so which
//     one Claude Code falls back to stops mattering.
func (v LoginView) RepairTarget() store.Account {
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

package store

import (
	"errors"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// ErrStoredLoginSpent is a path refusing to present an account's stored login
// -- to refresh it, hand out its access token, install it, or seed a session
// or a probe with it -- because a newer login of the account sits in one of
// Claude Code's login stores (Account.StoredLoginSpent). It is a transient
// HOLD, not a failure: it lifts once ccdad stores that login, or anything
// later, and says nothing bad about the account.
var ErrStoredLoginSpent = errors.New("a newer login of this account sits in Claude Code's login store, and ccdad " +
	"has not stored it yet, so its stored refresh token is already spent and is not presented")

// NamedLogins is the identity oracle's record of whose a login is, by the
// login's LoginHash, for the logins it named and ccdad could not store.
type NamedLogins func(loginHash string) (uuid string, ok bool)

// StoredLoginSpent reports whether a login sitting in one of Claude Code's
// live login stores has already superseded a's stored login, so that the
// stored refresh token is SPENT: whoever minted the newer login presented it,
// and presenting it again -- a refresh, a switch that installs it, a probe or a
// `ccdad run` session seeded with it -- can revoke the newer login with it.
//
// A store's login has superseded the stored one when it is a's -- the identity
// oracle named it so -- and is neither the stored login nor one the stored
// login replaced (the superseded history), and was issued after it
// (cclink.IssuedAfter). A stored login that cannot be read is superseded by
// any such login: what it holds cannot be shown to be the later.
//
// IT IS THE ONE DECISION, and every process applies it to the stores it read
// itself: the daemon to its polls, probes, plan and installs, tokens.Source
// before it refreshes or hands out a stored token, switcher.Execute under
// Claude Code's locks, and `ccdad run` and `ccdad probe` before they seed a
// session. It used to be a hold in the daemon's memory, which no `ccdad`
// command could see. Nothing is persisted for it but the oracle's naming --
// the one fact no process can derive (strategy.State.NamedLogin) -- so it
// lifts by itself the moment the stores or the stored login say otherwise.
func (a Account) StoredLoginSpent(stored cclink.Blob, live cclink.LoginStores, named NamedLogins) bool {
	storedID := cclink.LoginIdentity(stored)
	for _, login := range live.Logins() {
		id := cclink.LoginIdentity(login)
		if id == "" || id == storedID || a.Superseded(id) {
			continue
		}
		if uuid, ok := named(LoginHash(id)); !ok || uuid != a.UUID {
			continue
		}
		if cclink.IssuedAfter(login, stored) {
			return true
		}
	}
	return false
}

// LoadNamedLogins is the identity oracle's record as the engine state holds
// it. A state that cannot be read names nothing, which is the state a machine
// whose daemon has never named a login is in.
func LoadNamedLogins() NamedLogins {
	st, err := strategy.LoadState()
	if err != nil {
		return func(string) (string, bool) { return "", false }
	}
	return st.NamedLogin
}

// StoredLoginSpent is Account.StoredLoginSpent for one stored account, against
// the oracle's record on disk.
func (s *Store) StoredLoginSpent(uuid string, live cclink.LoginStores) bool {
	a, ok := s.Get(uuid)
	if !ok {
		return false
	}
	stored, _ := s.Credentials(uuid)
	return a.StoredLoginSpent(stored, live, LoadNamedLogins())
}

// SpentLogins is every Claude account whose stored login a login in live has
// superseded (Account.StoredLoginSpent), against the oracle's record on disk.
func (s *Store) SpentLogins(live cclink.LoginStores) map[string]bool {
	named := LoadNamedLogins()
	out := map[string]bool{}
	for _, a := range s.Accounts() {
		if a.Provider != provider.Claude {
			continue
		}
		stored, _ := s.Credentials(a.UUID)
		if a.StoredLoginSpent(stored, live, named) {
			out[a.UUID] = true
		}
	}
	return out
}

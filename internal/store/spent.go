package store

import (
	"errors"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// ErrStoredLoginSpent is a path refusing to present an account's stored login
// -- to refresh it, hand out its access token, install it, or seed a session
// or a probe with it -- because its refresh token is spent
// (Account.StoredLoginSpent). It is a transient HOLD, not a failure: it lifts
// once ccdad stores another login for the account, and says nothing bad about
// the account.
var ErrStoredLoginSpent = errors.New("a newer login of this account was minted from its stored refresh token, " +
	"and ccdad has not stored it, so the stored refresh token is already spent and is not presented")

// OracleRecord is what the daemon has recorded about logins it could not
// store (strategy.State): whose a login is, by LoginHash, as the identity
// oracle named it; and, per account, the stored login known to be spent and
// the newer login that spent it, by LoginHash ("" for a spent login that could
// not be read). *strategy.State is one.
type OracleRecord interface {
	NamedLogin(loginHash string) (uuid string, ok bool)
	SpentStoredLogin(uuid string) (spent, newer string, ok bool)
}

// StoredLoginSpent reports whether a's stored refresh token is SPENT: a newer
// login was minted from it, and presenting it again -- a refresh, a switch that
// installs it, a probe or a `ccdad run` session seeded with it -- can revoke
// that newer login, and every login refreshed from it since, with it.
//
// Either of two things says so:
//
//   - A login sitting in one of Claude Code's live stores supersedes it: the
//     identity oracle named that login a's, it is neither the stored login
//     nor one the stored login replaced (the superseded history), and it was
//     issued after it (cclink.IssuedAfter). A stored login that cannot be
//     read is superseded by any such login.
//   - The record says it is spent (SpentRecordHolds): the stored login has not
//     changed since a newer login was named and could not be stored. This is
//     the half that outlives the evidence. Claude Code can rotate the newer
//     login again, out of every store, before the oracle names what replaced
//     it -- and then nothing in the stores connects the stored login to it,
//     while its refresh token is exactly as spent.
//
// IT IS THE ONE DECISION, and every process applies it to the stores it read
// itself: the daemon to its polls, probes, plan and installs, tokens.Source
// before it refreshes or hands out a stored token, switcher.Execute under
// Claude Code's locks, and `ccdad run` and `ccdad probe` before they seed a
// session. It used to be a hold in the daemon's memory, which no `ccdad`
// command could see and a restart forgot. What is persisted is only what no
// process can derive: the oracle's naming, and which stored login was spent.
// It lifts by itself the moment the account stores another login.
func (a Account) StoredLoginSpent(stored cclink.Blob, live cclink.LoginStores, rec OracleRecord) bool {
	if a.SpentRecordHolds(stored, rec) {
		return true
	}
	storedID := cclink.LoginIdentity(stored)
	for _, login := range live.Logins() {
		id := cclink.LoginIdentity(login)
		if id == "" || id == storedID || a.Superseded(id) {
			continue
		}
		if uuid, ok := rec.NamedLogin(LoginHash(id)); !ok || uuid != a.UUID {
			continue
		}
		if cclink.IssuedAfter(login, stored) {
			return true
		}
	}
	return false
}

// SpentRecordHolds reports whether the record of a's spent stored login still
// applies: the account still stores the login recorded as spent. Any other
// login stored since -- the newer one adopted, a later one, a re-login -- is a
// change from it, and lifts it.
//
// A stored login that cannot be read now cannot be shown to have changed, so
// the record holds. One that could not be read when it was recorded ("") is
// changed from by any login that can be: holding out for the newer login
// itself, which is what the daemon's memory used to do, would keep an account
// that was logged in again held for good now that the record outlives the
// daemon. The daemon forgets a record once it no longer holds.
func (a Account) SpentRecordHolds(stored cclink.Blob, rec OracleRecord) bool {
	spent, _, ok := rec.SpentStoredLogin(a.UUID)
	if !ok {
		return false
	}
	id := cclink.LoginIdentity(stored)
	if id == "" {
		return true
	}
	return spent != "" && LoginHash(id) == spent
}

// LoadOracleRecord is the daemon's record as the engine state holds it. A
// state that cannot be read records nothing, which is the state a machine whose
// daemon has never named a login is in.
func LoadOracleRecord() OracleRecord {
	st, err := strategy.LoadState()
	if err != nil {
		return strategy.NewState()
	}
	return st
}

// StoredLoginSpent is Account.StoredLoginSpent for one stored account, against
// the record on disk.
func (s *Store) StoredLoginSpent(uuid string, live cclink.LoginStores) bool {
	a, ok := s.Get(uuid)
	if !ok {
		return false
	}
	stored, _ := s.Credentials(uuid)
	return a.StoredLoginSpent(stored, live, LoadOracleRecord())
}

// SpentLogins is every Claude account whose stored refresh token is spent
// (Account.StoredLoginSpent), against the record on disk.
func (s *Store) SpentLogins(live cclink.LoginStores) map[string]bool {
	rec := LoadOracleRecord()
	out := map[string]bool{}
	for _, a := range s.Accounts() {
		if a.Provider != provider.Claude {
			continue
		}
		stored, _ := s.Credentials(a.UUID)
		if a.StoredLoginSpent(stored, live, rec) {
			out[a.UUID] = true
		}
	}
	return out
}

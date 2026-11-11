package store

import (
	"errors"
	"fmt"

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
// not be read). LoadError is why the record could not be read, if it could
// not. *strategy.State is one.
type OracleRecord interface {
	NamedLogin(loginHash string) (uuid string, ok bool)
	SpentStoredLogin(uuid string) (spent, newer string, ok bool)
	LoadError() error
}

// Hold is why an account's stored login is not presented, or NotHeld.
type Hold uint8

const (
	// NotHeld: nothing says the stored refresh token is spent.
	NotHeld Hold = iota
	// HeldByNewerLogin: a login store holds a newer login of the account that
	// the identity oracle named and ccdad has not stored. The daemon stores it
	// as soon as it can, and the hold lifts.
	HeldByNewerLogin
	// HeldByRecord: the daemon recorded the stored login as spent, and no
	// login store holds the newer login under the oracle's name any more --
	// Claude Code refreshed it again, or it was written over. It lifts once
	// the account stores another login: the daemon's own adoption of what
	// Claude Code holds now, if that is the account's, or a new login.
	HeldByRecord
	// HeldUnreadableRecord: the record itself could not be read, so whether
	// any stored login is spent is unknown, and every one is held until it
	// can be read.
	HeldUnreadableRecord
)

// Held reports whether the stored login is not to be presented.
func (h Hold) Held() bool { return h != NotHeld }

// StoredLoginSpent reports whether a's stored refresh token is SPENT, and why:
// a newer login was minted from it, and presenting it again -- a refresh, a
// switch that installs it, a probe or a `ccdad run` session seeded with it --
// can revoke that newer login, and every login refreshed from it since, with
// it.
//
// Any of three things says so:
//
//   - The record cannot be read (HeldUnreadableRecord). What it held is then
//     unknown, and a stored login it may have recorded as spent cannot be
//     told from one it did not, so every stored login is held until it can be
//     read. A record that was never written is not unreadable: nothing was
//     ever held by it.
//   - A login sitting in one of Claude Code's live stores supersedes it
//     (HeldByNewerLogin): the identity oracle named that login a's, it is
//     neither the stored login nor one the stored login replaced (the
//     superseded history), and it was issued after it (cclink.IssuedAfter). A
//     stored login that cannot be read is superseded by any such login.
//   - The record says it is spent (HeldByRecord, SpentRecordHolds): the stored
//     login has not changed since a newer login was named and could not be
//     stored. This is the half that outlives the evidence. Claude Code can
//     rotate the newer login again, out of every store, before the oracle
//     names what replaced it -- and then nothing in the stores connects the
//     stored login to it, while its refresh token is exactly as spent.
//
// live is the stores as the caller read them. A read that FAILED is passed as
// the zero LoginStores: it is no evidence either way, and the record decides
// alone -- an unreadable store must never weaken a hold the record holds.
//
// IT IS THE ONE DECISION, and every process applies it to the stores it read
// itself: the daemon to its polls, probes, plan and installs, tokens.Source
// before it refreshes or hands out a stored token, switcher.Execute under
// Claude Code's locks, and `ccdad run` and `ccdad probe` before they seed a
// session. It used to be a hold in the daemon's memory, which no `ccdad`
// command could see and a restart forgot. What is persisted is only what no
// process can derive: the oracle's naming, and which stored login was spent.
// It lifts by itself the moment the account stores another login.
func (a Account) StoredLoginSpent(stored cclink.Blob, live cclink.LoginStores, rec OracleRecord) Hold {
	storedID := cclink.LoginIdentity(stored)
	if rec.LoadError() != nil {
		if storedID != "" {
			return HeldUnreadableRecord
		}
		return NotHeld
	}
	for _, login := range live.Logins() {
		id := cclink.LoginIdentity(login)
		if id == "" || id == storedID || a.Superseded(id) {
			continue
		}
		if uuid, ok := rec.NamedLogin(LoginHash(id)); !ok || uuid != a.UUID {
			continue
		}
		if cclink.IssuedAfter(login, stored) {
			return HeldByNewerLogin
		}
	}
	if a.SpentRecordHolds(stored, rec) {
		return HeldByRecord
	}
	return NotHeld
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
// state that was never written records nothing. One that cannot be read or
// parsed reports why in LoadError, and holds every stored login
// (StoredLoginSpent).
func LoadOracleRecord() OracleRecord {
	st, err := strategy.LoadState()
	if err != nil {
		return unreadableRecord{err}
	}
	return st
}

// unreadableRecord is a record whose state could not even be located.
type unreadableRecord struct{ err error }

func (unreadableRecord) NamedLogin(string) (string, bool)               { return "", false }
func (unreadableRecord) SpentStoredLogin(string) (string, string, bool) { return "", "", false }
func (r unreadableRecord) LoadError() error                             { return r.err }

// presentRereads bounds how many more times PresentableLogin reads a stored
// login that changed under it.
const presentRereads = 3

// PresentableLogin is uuid's stored login as it is about to be PRESENTED --
// refreshed, handed out, or seeded into a session -- and the hold on that
// exact copy (Account.StoredLoginSpent).
//
// The decision and the use are one read, and a caller presents the copy
// returned here and no other. Deciding on a copy read again elsewhere is what
// let a path check G1, the newer login the daemon had just stored, and then
// present the spent G0 it had read first. When the copy is held, the store is
// read again, and if the account has stored another login meanwhile the
// decision is taken again on that one -- a few times at most: a hold that has
// just lifted is no reason to refuse.
func (s *Store) PresentableLogin(uuid string, live cclink.LoginStores, rec OracleRecord) (cclink.Blob, Hold, error) {
	a, ok := s.Get(uuid)
	if !ok {
		return nil, NotHeld, fmt.Errorf("%w: %q", ErrNotFound, uuid)
	}
	stored, err := s.Credentials(uuid)
	if err != nil {
		return nil, NotHeld, err
	}
	for i := 0; ; i++ {
		hold := a.StoredLoginSpent(stored, live, rec)
		if !hold.Held() || i == presentRereads {
			return stored, hold, nil
		}
		again, err := s.Credentials(uuid)
		if err != nil || cclink.LoginIdentity(again) == cclink.LoginIdentity(stored) {
			return stored, hold, nil
		}
		stored = again
	}
}

// SpentLogins is every Claude account whose stored refresh token is spent
// (Account.StoredLoginSpent), against rec.
func (s *Store) SpentLogins(live cclink.LoginStores, rec OracleRecord) map[string]bool {
	out := map[string]bool{}
	for _, a := range s.Accounts() {
		if a.Provider != provider.Claude {
			continue
		}
		stored, _ := s.Credentials(a.UUID)
		if a.StoredLoginSpent(stored, live, rec).Held() {
			out[a.UUID] = true
		}
	}
	return out
}

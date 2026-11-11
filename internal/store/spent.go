package store

import (
	"errors"
	"fmt"
	"sync"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/provider"
)

// ErrStoredLoginSpent is a path refusing to present an account's stored login
// -- to refresh it, hand out its access token, install it, or seed a session
// or a probe with it -- because its refresh token is spent
// (Account.StoredLoginSpent). It is a transient HOLD, not a failure: it lifts
// once ccdad stores another login for the account, and says nothing bad about
// the account.
var ErrStoredLoginSpent = errors.New("a newer login of this account was minted from its stored refresh token, " +
	"and ccdad has not stored it, so the stored refresh token is already spent and is not presented")

// SpentError is ErrStoredLoginSpent for one account, with why its stored login
// is held -- which is what says what lifts it. Label names the account in the
// text; without one it is named by UUID.
type SpentError struct {
	UUID  string
	Label string
	Hold  Hold
}

// Error is HoldReason's sentence for this hold. It is what daemon.log and
// `ccdad status --refresh` print, so it says what lifts the hold as every
// other place does -- from the same table.
func (e *SpentError) Error() string {
	label := e.Label
	if label == "" {
		label = "account " + e.UUID
	}
	return HoldReason(e.Hold, label, StoresUnknown)
}

// Unwrap makes errors.Is(err, ErrStoredLoginSpent) hold.
func (e *SpentError) Unwrap() error { return ErrStoredLoginSpent }

// OracleRecord is what the daemon has recorded about logins it could not
// store (SpentRecord): whose a login is, by LoginHash, as the identity
// oracle named it; and, per account, the stored login known to be spent and
// the newer login that spent it, by LoginHash ("" for a spent login that could
// not be read). LoadError is why the record could not be read, if it could
// not. *SpentRecord is one.
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
	// the stored login is spent is unknown, and it is held until the record
	// can be read -- unless Claude Code is on it already (liveHoldsNewest).
	HeldUnreadableRecord
)

// Held reports whether the stored login is not to be presented.
func (h Hold) Held() bool { return h != NotHeld }

// StoresNow is what Claude Code's login stores hold now, as far as what lifts
// a hold the record keeps depends on it.
type StoresNow uint8

const (
	// StoresUnknown: the stores could not be read, or were not looked at.
	StoresUnknown StoresNow = iota
	// StoresHoldUnnamed: a store holds a login nobody can name yet -- possibly
	// the newer login, refreshed again.
	StoresHoldUnnamed
	// StoresHoldNoneUnnamed: no store holds a login nobody can name.
	StoresHoldNoneUnnamed
)

// HoldReason is why label's stored login is held, and what lifts the hold, in
// a user's words. It is the one table of them: every refusal, `ccdad doctor`,
// `ccdad status`, daemon.log and SpentError say it through here.
//
// What lifts a hold the record keeps depends on the stores (now). A newer
// login still in a store under the oracle's name is the daemon's to store. One
// that has left every store may be back as a login nobody can name yet,
// refreshed again, which the daemon names and stores if it is the account's;
// when no store holds anything unnamed, nothing will store it, and logging the
// account in again is the only way out.
func HoldReason(h Hold, label string, now StoresNow) string {
	switch h {
	case NotHeld:
		return label + "'s stored login is not held"
	case HeldUnreadableRecord:
		return "the record of which stored logins are spent cannot be read, and it may hold " + label +
			"'s as spent. Until it can be read, a stored login is presented only when Claude Code's own " +
			"login stores show it is on that login -- one holds it, both could be read, and neither holds a " +
			"later login that could be the same account's -- and they do not show that for " + label + "'s. " +
			SpentRecordAdvice(label+", and every other account `ccdad doctor` lists as held")
	case HeldByNewerLogin:
		return "Claude Code holds a newer login of " + label + " than ccdad has stored, so its stored refresh " +
			"token is already spent, and presenting it could revoke the newer login. The daemon stores the " +
			"newer login as soon as it can"
	}
	spent := label + "'s stored refresh token was already spent by a newer login ccdad could not store, " +
		"and presenting it could revoke that login and every one refreshed from it since"
	relogin := "log " + label + " in again with `ccdad add claude`"
	switch now {
	case StoresHoldUnnamed:
		return spent + ". Claude Code holds a login ccdad cannot name yet, which may be that login refreshed " +
			"again: the daemon asks the profile endpoint whose it is, and stores it if it is " + label +
			"'s. If it is not, " + relogin
	case StoresHoldNoneUnnamed:
		return spent + ". No login store holds that login any more, so nothing will store it: " + relogin
	}
	return spent + ". If no login store holds that login any more, " + relogin
}

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
//     told from one it did not, so stored logins are held until it can be
//     read -- every one but a login Claude Code is on already: one of its
//     stores holds it, both could be read, and neither holds a later login
//     that could be its account's (liveHoldsNewest). A record that was never
//     written is not unreadable: nothing was ever held by it.
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
func (a Account) StoredLoginSpent(stored cclink.Blob, live cclink.LoginStores, owners LoginOwners, rec OracleRecord) Hold {
	storedID := cclink.LoginIdentity(stored)
	if rec.LoadError() != nil {
		if storedID == "" || a.liveHoldsNewest(stored, live, owners) {
			return NotHeld
		}
		return HeldUnreadableRecord
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

// liveHoldsNewest reports whether one of Claude Code's login stores holds a's
// stored login and none holds a later-issued login (cclink.IssuedAfter) that
// could be a's.
//
// Such a login is what Claude Code is presenting already, so a record that
// cannot be read is no reason to hold it: holding it protects nothing, and
// costs the split repair that puts it in the other store -- leaving there a
// login it replaced, which Claude Code may present next. A stored login no
// store holds, or one beside a later login that may be a's, stays held -- and
// so does every stored login when a store could not be read, since that store
// may hold the later login.
//
// "Could be a's" is any login owners does not name as ANOTHER account's --
// another account's stored login, or one its stored login replaced. Such a
// login says nothing about whether a's is spent, whatever put it there:
// ccdad's own switch from that account to a whose outcome was unknown -- which
// the repair completes by putting a's login in the other store -- or a /login
// to that account that Claude Code wrote to the file while its keychain write
// was failing. A later login nobody can name still counts, because it may be
// a's newer one, and the repair would then write the spent login over it.
func (a Account) liveHoldsNewest(stored cclink.Blob, live cclink.LoginStores, owners LoginOwners) bool {
	// A store that could not be read may hold the later login. The file's
	// failure is FileErr; the keychain's is the zero stores ReadLoginStores
	// returns with its error, in which no store holds the stored login.
	if live.FileErr != nil {
		return false
	}
	storedID := cclink.LoginIdentity(stored)
	held := false
	for _, login := range live.Logins() {
		switch id := cclink.LoginIdentity(login); {
		case id == storedID:
			held = true
		case id == "" || owners.ownedElsewhere(id, a.UUID):
		case cclink.IssuedAfter(login, stored):
			return false
		}
	}
	return held
}

// LoginOwners names the managed account a login belongs to as far as this
// store can say without asking anybody (OwnerByEvidence). Nil names nothing.
type LoginOwners func(loginIdentity string) (uuid string, ok bool)

// ownedElsewhere reports whether o names the login as an account other than
// uuid's.
func (o LoginOwners) ownedElsewhere(loginIdentity, uuid string) bool {
	if o == nil {
		return false
	}
	owner, ok := o(loginIdentity)
	return ok && owner != uuid
}

// Evidence is how the store names a login's owner by its own records.
type Evidence uint8

const (
	// NoEvidence: nothing stored names the login.
	NoEvidence Evidence = iota
	// ByStoredLogin: it is a Claude account's stored login.
	ByStoredLogin
	// BySupersededLogin: an account's stored login replaced it
	// (Account.Superseded) -- an older generation of that account's login.
	BySupersededLogin
)

// OwnerByEvidence names the managed account a login belongs to by the store's
// own evidence, asking nobody: the Claude account whose stored login it is,
// read through lookup, or failing that the account whose stored login replaced
// it. It is the one answer to that question -- the switcher's attribution of a
// login store (switcher.LiveStateOf, and its view of both stores) and the
// spent-login hold's (OwnersOf) both ask it -- so the two cannot name one
// login two ways.
//
// ONLY CLAUDE ROWS are matched by their stored login. `ccdad import` trusts a
// row's provider, and cclink.Extract carries a claudeAiOauth blob through
// whatever the provider, so a row can be Provider: codex beside a
// Claude-shaped login: shape alone does not prove ownership. daemon.pollable
// guards the same reachable row the same way.
func OwnerByEvidence(loginIdentity string, accounts []Account, lookup func(uuid string) (cclink.Blob, error)) (Account, Evidence) {
	if loginIdentity == "" {
		return Account{}, NoEvidence
	}
	for _, a := range accounts {
		if a.Provider != provider.Claude {
			continue
		}
		stored, err := lookup(a.UUID)
		if err != nil {
			continue
		}
		if cclink.LoginIdentity(stored) == loginIdentity {
			return a, ByStoredLogin
		}
	}
	for _, a := range accounts {
		if a.Superseded(loginIdentity) {
			return a, BySupersededLogin
		}
	}
	return Account{}, NoEvidence
}

// OwnersOf is LoginOwners over accounts: OwnerByEvidence, reading each stored
// login through lookup.
//
// LAZILY, and once. Nothing is read until a login is asked about, which the
// hold rule does only when the record cannot be read (liveHoldsNewest); and a
// stored login read once is not read again, since the daemon decides for
// every account on every pass and each decision may ask about two logins.
func OwnersOf(accounts []Account, lookup func(uuid string) (cclink.Blob, error)) LoginOwners {
	type read struct {
		blob cclink.Blob
		err  error
	}
	var mu sync.Mutex
	seen := map[string]read{}
	once := func(uuid string) (cclink.Blob, error) {
		mu.Lock()
		defer mu.Unlock()
		r, ok := seen[uuid]
		if !ok {
			r.blob, r.err = lookup(uuid)
			seen[uuid] = r
		}
		return r.blob, r.err
	}
	return func(id string) (string, bool) {
		a, ev := OwnerByEvidence(id, accounts, once)
		return a.UUID, ev != NoEvidence
	}
}

// LoginOwners is OwnersOf over this store's accounts and stored logins.
func (s *Store) LoginOwners() LoginOwners { return OwnersOf(s.Accounts(), s.Credentials) }

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

// LoadOracleRecord is the daemon's record as its file holds it
// (LoadSpentRecord). A record that was never written records nothing. One
// that cannot be read or parsed reports why in LoadError, and holds stored
// logins (StoredLoginSpent).
func LoadOracleRecord() OracleRecord {
	r, err := LoadSpentRecord()
	if err != nil {
		return unreadableRecord{err}
	}
	return r
}

// unreadableRecord is a record whose file could not even be located.
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
	owners := s.LoginOwners()
	for i := 0; ; i++ {
		hold := a.StoredLoginSpent(stored, live, owners, rec)
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
// (Account.StoredLoginSpent), against rec, with why. An account it does not
// name is NotHeld.
func (s *Store) SpentLogins(live cclink.LoginStores, rec OracleRecord) map[string]Hold {
	out := map[string]Hold{}
	owners := s.LoginOwners()
	for _, a := range s.Accounts() {
		if a.Provider != provider.Claude {
			continue
		}
		stored, _ := s.Credentials(a.UUID)
		if hold := a.StoredLoginSpent(stored, live, owners, rec); hold.Held() {
			out[a.UUID] = hold
		}
	}
	return out
}

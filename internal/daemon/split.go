package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/switcher"
)

// The daemon's half of the split-login repair: Claude Code's keychain item and
// its credentials file holding different logins. switcher/split.go decides what
// counts as split and which way it is repaired; this file decides when, and
// remembers what a repair needs from the ticks before it.

// failedWriteRetry is how long the engine waits after a credential write
// failed before it writes again -- a plan's swap or a split repair alike. See
// execute for why there is one backoff and not one per caller.
const failedWriteRetry = time.Minute

// settlePending closes out ccdad's own unconfirmed switch once the stores
// agree, so no repair will (switcher.SettlePending): agreeing on its target
// completes it -- cooldown and departure recorded -- and agreeing on another
// account forgets it. A tick that could not read the stores settles nothing.
func (e *Engine) settlePending(s *store.Store, ev switcher.Evaluation, viewAt time.Time) {
	if !ev.LoginKnown {
		return
	}
	outcome, to, err := switcher.SettlePending(s, ev.Login, viewAt, e.now())
	switch {
	case err != nil:
		e.logf("the switch to %s whose keychain write could not be confirmed could not be settled: %v", to, err)
	case outcome == switcher.PendingCompleted:
		e.logf("the switch to %s whose keychain write could not be confirmed took effect; recorded it", to)
	case outcome == switcher.PendingNotTaken:
		e.logf("the switch to %s whose keychain write could not be confirmed did not take effect; "+
			"both login stores hold %s", to, ev.Login.Lead().Account.Label())
	}
}

// repairSplit makes both of Claude Code's login stores hold the same login and
// records it, when they hold different logins whose owners are known
// (switcher.LoginView.StoresDisagree) and the plan is not leaving the account
// they are repaired towards (the plan's own switch repairs it then).
//
// It runs INSTEAD of the plan, for the tick it runs in: the next tick ranks a
// machine whose stores agree.
//
// The account is the target of ccdad's own unconfirmed switch when a store
// holds it, else the one whose login was issued most recently
// (LoginView.RepairTarget says why). It is already live in one store, so this
// is a SYNC: it records the account as live and stamps no cooldown -- a
// cooldown would hold the engine on an account it may need to leave at once --
// unless it completes ccdad's own unconfirmed switch, which Execute records as
// that switch, departure included.
//
// It writes only the store that differs (cclink.writeMerged skips a store that
// already holds the login), decides against both stores as this tick read them
// (Request.Expect), and writes the stored login read again under the lock. A
// login inside Claude Code's refresh window is not written.
//
// Manual mode does not hold it off. Manual mode stops the engine CHOOSING an
// account; a repair chooses nothing, it carries out a login one store already
// holds.
func (e *Engine) repairSplit(ctx context.Context, s *store.Store, ev switcher.Evaluation) (switcher.Result, error) {
	target := ev.Login.RepairTarget()
	if described := ev.Login.Describe(); described != e.saidSplit {
		e.saidSplit = described
		e.logf("the Claude Code login is split: %s. Claude Code reads the keychain item first and falls "+
			"back to the credentials file when that read fails, so sessions can spend either login; "+
			"putting %s in both and recording it", described, target.Label())
	}
	at := ev.Login.At()
	res, err := e.execute(ctx, s, switcher.Request{Target: target, LiveUUID: target.UUID, Expect: &at, Sync: true})
	if err != nil {
		return res, fmt.Errorf("repairing the split login: %w", err)
	}
	switch {
	case res.Outcome == switcher.Switched && res.CompletedPending:
		e.logf("repaired the split login and completed the switch to %s: both stores and ccdad's record "+
			"now name it", target.Label())
	case res.Outcome == switcher.Switched:
		e.logf("repaired the split login: both stores and ccdad's record now name %s", target.Label())
	case res.RecordedNow:
		e.logf("ccdad's record of the live account now names %s, which both stores already held", target.Label())
	}
	return res, nil
}

// loginMemoryGrace is how long the daemon keeps what it knows about a login --
// the identity oracle's answer, that a line about it has been said -- after the
// last pass that saw it in one of Claude Code's login stores.
//
// A login leaves its stores for good almost every time it leaves them: Claude
// Code rotated it, or a switch replaced it. The grace is for the exception, a
// store that swings between two logins -- Claude Code writing the file when its
// keychain write fails and the item when it does not -- so that neither the ask
// nor the line repeats on every swing. Past it the login is forgotten; if it
// comes back it costs one more ask, held to resolveMinInterval like any other,
// and one more line.
const loginMemoryGrace = 10 * time.Minute

// loginMemory is what the daemon knows per login, by cclink.LoginIdentity,
// kept while the login can still matter: while a login store holds it, and for
// loginMemoryGrace after the last pass that saw it there (keepHeld).
//
// It used to be a map wiped whole once it reached 64 logins. A login's identity
// changes on every refresh, so the map did fill, and wiping it forgot the
// logins still sitting in a store along with the ones long gone: the profile
// endpoint was asked about them again, and their line said again.
type loginMemory[V any] map[string]rememberedLogin[V]

type rememberedLogin[V any] struct {
	value V
	seen  time.Time
}

func (m loginMemory[V]) get(id string) (V, bool) {
	r, ok := m[id]
	return r.value, ok
}

// put remembers v about the login id, seen at now.
func (m *loginMemory[V]) put(id string, v V, now time.Time) {
	if *m == nil {
		*m = loginMemory[V]{}
	}
	(*m)[id] = rememberedLogin[V]{value: v, seen: now}
}

// keepHeld marks each login the stores hold as seen at now, and forgets every
// other one no pass has seen there for loginMemoryGrace.
func (m loginMemory[V]) keepHeld(held []string, now time.Time) {
	for _, id := range held {
		if r, ok := m[id]; ok {
			r.seen = now
			m[id] = r
		}
	}
	for id, r := range m {
		if now.Sub(r.seen) > loginMemoryGrace {
			delete(m, id)
		}
	}
}

// ageLoginMemory holds the daemon's per-login memory against the logins this
// pass's stores hold (loginMemory). A store that could not be read is no
// evidence that a login left it, so a pass with one ages nothing.
func (e *Engine) ageLoginMemory(st cclink.LoginStores, now time.Time) {
	if st.FileErr != nil {
		return
	}
	held := []string{cclink.LoginIdentity(st.File)}
	if st.HasItem {
		held = append(held, cclink.LoginIdentity(st.Item))
	}
	e.owners.keepHeld(held, now)
	e.saidStore.keepHeld(held, now)
}

// ownerOf is the identity oracle's memory as switcher.Owners: the managed
// account a login belongs to, when the oracle has said.
func (e *Engine) ownerOf(id string) (string, bool) {
	uuid, ok := e.owners.get(id)
	return uuid, ok && uuid != ""
}

// identifyStores asks the identity oracle about every login store whose login
// this store cannot name, when the two stores hold different logins
// (switcher.LoginView.Unresolved).
//
// The first version of the split repair called a file it could not name, beside
// an item it could, fine: "Claude Code refreshed the item and left the file
// behind." That is one reading. Claude Code writing its NEWER login to the file
// because its own keychain write failed -- a refresh, or a /login -- is the
// other, and on the loaded Mac where splits happen it is the likelier one.
// Calling that fine left a second account being spent with nothing reporting
// it.
//
// So the login is named, one ask per login and no more than resolveMinInterval
// apart, and then:
//
//   - a managed account's login NEWER than its stored one is adopted into the
//     store, so the next pass sees two named stores and repairs towards it;
//   - a managed account's OLDER login is remembered as that account's, so the
//     next pass sees two generations of one account and repairs towards the
//     newer -- the older's refresh token is already spent;
//   - a login that is nobody's here, or that cannot be named, is left as it
//     is and said so, once. Nothing is written over a login nobody has named;
//     doctor warns about it.
//
// It reports whether it NAMED a managed account's newer login -- adopted, or
// named and not adopted because storing it failed. Either way the evaluation
// this tick made did not know whose that login is, and the caller does not
// act on it. A login named and not adopted also holds its account's stored
// refresh token, in every process, until the account stores another login
// (nameUnstored).
func (e *Engine) identifyStores(ctx context.Context, s *store.Store, ev switcher.Evaluation) (named bool) {
	stores := []struct {
		name   string
		blob   cclink.Blob
		holder switcher.StoreHolder
	}{
		{"the keychain item", ev.Stores.Item, ev.Login.Item},
		{"the credentials file", ev.Stores.File, ev.Login.File},
	}
	for _, st := range stores {
		if st.holder.State != switcher.LiveUnattributed || st.holder.ID == "" {
			continue
		}
		owner, verdict := e.resolveStore(ctx, s, st.blob)
		switch verdict {
		case liveAdoptFailed:
			// verdictFor has placed the hold.
			named = true
			e.sayStoreOnce(st.holder.ID, fmt.Sprintf("%s holds a newer login of %s than ccdad has stored, "+
				"and storing it failed; it is tried again, and %s is not refreshed meanwhile -- its stored "+
				"refresh token is already spent", st.name, owner.Label(), owner.Label()))
		case liveAdopted:
			named = true
			e.logf("%s holds a newer login of %s than ccdad had stored; it is %s's stored login now",
				st.name, owner.Label(), owner.Label())
		case liveOwned:
			e.sayStoreOnce(st.holder.ID, fmt.Sprintf("%s holds an older login of %s, whose refresh token is "+
				"already spent; it is repaired towards the newer one", st.name, owner.Label()))
		case liveForeign:
			e.sayStoreOnce(st.holder.ID, fmt.Sprintf("%s holds a login that belongs to no account in this "+
				"store, beside a different login in the other store (%s). It is not repaired; run "+
				"`ccdad doctor` and `ccdad switch` to the account you mean", st.name, ev.Login.Describe()))
		default:
			e.sayStoreOnce(st.holder.ID, fmt.Sprintf("%s holds a login this store cannot name, beside a "+
				"different login in the other store (%s). The profile endpoint could not say whose it is yet, "+
				"and it is asked again; until then it is not repaired from or towards", st.name, ev.Login.Describe()))
		}
	}
	return named
}

// nameUnstored records what a failed adoption established and nothing can
// derive later, in the record every process reads (store.SpentRecord): the
// identity oracle's answer that live is owner's, and that owner's stored login
// spent -- the one the verdict compared, nil if it could not be read -- is
// spent, by live.
//
// That is what holds owner's stored refresh token, in this daemon and in every
// `ccdad` command, until owner stores another login
// (store.Account.StoredLoginSpent). Nothing lifts it by hand: the hold is
// derived again from the record, the stores and the stored login wherever it
// is asked, and forgetOracleRecord drops what no longer applies.
//
// A RECORD THAT CANNOT BE WRITTEN STILL HOLDS, in this process: it is kept in
// memory (oracleRecord lays it over the record on disk) and written again on
// every pass until it lands (writeUnstored). The fact it records -- this
// stored token is spent -- does not stop being true because a disk was full
// or a lock was held, and the daemon's memory is the one place it is known
// meanwhile; other processes learn it when the write lands.
func (e *Engine) nameUnstored(owner store.Account, live, spent cclink.Blob) {
	op := unstoredLogin{uuid: owner.UUID, newer: store.LoginHash(cclink.LoginIdentity(live))}
	if id := cclink.LoginIdentity(spent); id != "" {
		op.spent = store.LoginHash(id)
	}
	e.unstored = append(e.unstored, op)
	e.writeUnstored()
}

// unstoredLogin is one record nameUnstored has to write: the newer login, by
// hash, named as uuid's, and the stored login it spent, by hash or "".
type unstoredLogin struct{ uuid, newer, spent string }

// applyTo makes the record in st, the way every write of it does.
func (u unstoredLogin) applyTo(st *store.SpentRecord) {
	st.NameLogin(u.newer, u.uuid)
	st.RecordSpentStoredLogin(u.uuid, u.spent, u.newer)
}

// writeUnstored writes every record that has not been written yet, and keeps
// them in memory when it cannot. The tick calls it on every pass.
func (e *Engine) writeUnstored() {
	if len(e.unstored) == 0 {
		return
	}
	err := store.WithSpentRecord(stateTimeout, func(st *store.SpentRecord) error {
		for _, op := range e.unstored {
			op.applyTo(st)
		}
		return nil
	})
	if err != nil {
		if !e.saidUnstored {
			e.saidUnstored = true
			e.logf("could not record that Claude Code holds a newer login than ccdad has stored: %v; this "+
				"daemon holds the stored refresh token meanwhile and writes the record on every pass until "+
				"it lands, and other ccdad commands do not know of it until then", err)
		}
		return
	}
	if e.saidUnstored {
		e.saidUnstored = false
		e.logf("recorded the newer login(s) ccdad could not store; every ccdad command holds the spent " +
			"stored refresh token now")
	}
	e.unstored = nil
}

// oracleRecord is the record this daemon decides from: the record on disk,
// with every one not yet written laid over it.
func (e *Engine) oracleRecord() store.OracleRecord {
	rec := store.LoadOracleRecord()
	st, ok := rec.(*store.SpentRecord)
	if !ok {
		return rec
	}
	for _, op := range e.unstored {
		op.applyTo(st)
	}
	return st
}

// forgetOracleRecord drops what the daemon recorded about logins it could not
// store once it can no longer hold anything: the naming of a login no login
// store holds any more, and the record of a spent stored login once the
// account has stored another (store.Account.SpentRecordHolds) or is gone. A
// pass whose stores could not all be read forgets nothing, and one with
// nothing to forget does not take the state lock.
func (e *Engine) forgetOracleRecord(s *store.Store, st cclink.LoginStores) {
	if st.FileErr != nil {
		return
	}
	held := map[string]bool{}
	for _, login := range st.Logins() {
		if id := cclink.LoginIdentity(login); id != "" {
			held[store.LoginHash(id)] = true
		}
	}
	keepNamed := func(h string) bool { return held[h] }
	keepSpent := func(rec store.OracleRecord) func(string) bool {
		return func(uuid string) bool {
			a, ok := s.Get(uuid)
			if !ok {
				return false
			}
			stored, _ := s.Credentials(uuid)
			return a.SpentRecordHolds(stored, rec)
		}
	}
	cur, err := store.LoadSpentRecord()
	if err != nil {
		return
	}
	if forgot := cur.ForgetNamedLogins(keepNamed); !cur.ForgetSpentStoredLogins(keepSpent(cur)) && !forgot {
		return
	}
	if err := store.WithSpentRecord(stateTimeout, func(state *store.SpentRecord) error {
		keep := keepSpent(state)
		state.ForgetNamedLogins(keepNamed)
		state.ForgetSpentStoredLogins(keep)
		return nil
	}); err != nil {
		e.logf("could not forget the record of logins ccdad could not store that no longer hold anything: %v", err)
	}
}

// setSpent records which accounts' stored logins this pass found spent
// (store.Store.SpentLogins), as the refusal holdSpentToken gives for each --
// naming the account by its label, as every other daemon.log line does.
// Guarded by e.mu: the swap path and the pollers read it.
func (e *Engine) setSpent(s *store.Store, holds map[string]store.Hold) {
	spent := make(map[string]*store.SpentError, len(holds))
	for uuid, hold := range holds {
		a, _ := s.Get(uuid)
		spent[uuid] = &store.SpentError{UUID: uuid, Label: a.Label(), Hold: hold}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spent = spent
}

// holdSpentToken is the ONE predicate every path in the engine that may
// present an account's stored refresh token asks first: store.ErrStoredLoginSpent
// while this pass found the account's stored login superseded by a newer one
// in a login store, nil otherwise. Every path that hands a stored login or
// token out of the engine asks it: accessToken (the usage poll and the profile
// check), freshen, probeDue, and execute -- and a spent account is not a plan
// candidate at all (switcher.Evaluation.Spent).
//
// It answers from the pass's own read, through the same decision
// tokens.Source and switcher.Execute apply to theirs, which is what every
// other process gets.
func (e *Engine) holdSpentToken(uuid string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if refusal, held := e.spent[uuid]; held {
		return refusal
	}
	return nil
}

// accessToken is the daemon's ONE way to an account's access token, which may
// refresh and so present the stored refresh token. It asks holdSpentToken
// first, so no path gets a token past the hold: the usage poll and the profile
// check both read through it.
func (e *Engine) accessToken(ctx context.Context, uuid string) (string, error) {
	if err := e.holdSpentToken(uuid); err != nil {
		return "", err
	}
	return e.AccessToken(ctx, uuid)
}

// sayStoreOnce logs line about the login store holding the login id, once for
// as long as the daemon remembers that login (loginMemory).
func (e *Engine) sayStoreOnce(id, line string) {
	if _, said := e.saidStore.get(id); said {
		return
	}
	e.saidStore.put(id, struct{}{}, e.now())
	e.logf("%s", line)
}

// remember records what the oracle said about a login.
func (e *Engine) remember(id, uuid string) {
	e.owners.put(id, uuid, e.now())
}

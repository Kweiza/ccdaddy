package daemon

import (
	"context"
	"errors"
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
// refresh token (verdictFor places the hold; releaseStored lifts it).
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

// spentHold is one account's hold: the newer login the oracle named, and the
// SPENT login -- the one ccdad had stored when the hold was placed, whose
// refresh token whoever minted the newer one has already presented.
type spentHold struct {
	newer string
	spent string
}

// holdUntilStored holds uuid's stored refresh token: the oracle named the
// login with identity newer as the account's newer one, and storing it failed.
// spent is the account's stored login at that moment, "" if it could not be
// read. While it is held, presenting that stored refresh token can revoke the
// whole family, newer included. Guarded by e.mu: the swap path and the pollers
// read it.
func (e *Engine) holdUntilStored(uuid, newer, spent string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.unadopted == nil {
		e.unadopted = map[string]spentHold{}
	}
	e.unadopted[uuid] = spentHold{newer: newer, spent: spent}
}

// releaseStored lifts each hold once the account's STORED credential is no
// longer the spent one -- whatever replaced it: the newer login adopted, a
// later generation adopted after the newer one was missed, a re-login with
// `ccdad add`, a switch that stored a new login. What the hold protects is
// "the stored refresh token is spent", and that stops being true the moment
// another login is stored. A spent identity that could not be read falls back
// to waiting for the newer login itself (or one that superseded it).
//
// Two login stores AGREEING lifts nothing: they can agree on the newer login
// while ccdad still stores the spent one, and the next poll would present it.
func (e *Engine) releaseStored(s *store.Store) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for uuid, h := range e.unadopted {
		a, ok := s.Get(uuid)
		if !ok {
			delete(e.unadopted, uuid)
			continue
		}
		stored, err := s.Credentials(uuid)
		cur := ""
		if err == nil {
			cur = cclink.LoginIdentity(stored)
		}
		moved := cur != "" && (cur == h.newer || (h.spent != "" && cur != h.spent))
		if moved || a.Superseded(h.newer) {
			delete(e.unadopted, uuid)
		}
	}
}

// holdSpentToken is the ONE predicate every path that may present an
// account's stored refresh token asks first: errUnadopted while the account has
// a newer login named and not yet stored, nil otherwise. Every path that hands
// a stored login or token out of the engine asks it: accessToken (the usage
// poll and the profile check), freshen, probeDue, and execute -- and a held
// account is not a plan candidate at all (EvalOptions.Held).
func (e *Engine) holdSpentToken(uuid string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, held := e.unadopted[uuid]; held {
		return errUnadopted
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

// errUnadopted is a path refusing an account whose newer login is named and
// not yet stored. It is a transient HOLD, not a failure: handleFailure files it
// with ErrLiveTokenStale, which reschedules at the ordinary cadence and
// neither backs off nor quarantines.
var errUnadopted = errors.New("a newer login of this account is named and not yet stored, so its stored " +
	"refresh token is already spent and is not presented")

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

// newerThanStored reports whether login was issued after what the account
// already has stored. Two logins with no expiry are not ordered, and the
// login in hand wins, which is how adoption behaved before it was ordered at
// all; a stored expiry beside a login without one keeps the stored.
func newerThanStored(login, stored cclink.Blob) bool {
	at, ok := cclink.LoginExpiry(login)
	was, wasOK := cclink.LoginExpiry(stored)
	if !ok {
		return !wasOK
	}
	return !wasOK || at.After(was)
}

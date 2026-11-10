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
func (e *Engine) settlePending(s *store.Store, ev switcher.Evaluation) {
	if !ev.LoginKnown {
		return
	}
	outcome, to, err := switcher.SettlePending(s, ev.Login, e.now())
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

// unresolvedFreezeMax is the longest the plan is held for a login store whose
// login cannot be named.
//
// The plan is held at all only while the unnamed login is NEWER than the named
// one (LoginView.UnnamedNewer): that is the one case where it may be somebody's
// latest decision -- Claude Code's /login or refresh that went to the file
// because its keychain write failed -- and where the plan's switch, which
// writes both stores, could overwrite it. An older unnamed login is a
// superseded one, and a login the oracle confirmed belongs to nobody here is
// not ccdad's to protect; neither holds anything.
//
// Ten minutes, from what the wait can buy. The oracle is asked at most once a
// resolveMinInterval (a minute), so ten minutes is ten asks: enough to ride out
// a profile endpoint having a bad few minutes. An oracle that has not answered
// in ten asks is failing in a way that does not clear by waiting -- an expired
// access token is never answered -- and the cost of waiting longer is the
// incident's own: a plan that cannot move while the live account runs to its
// limit. After the bound the plan runs; its switch writes both stores, as
// every switch before the split repair did.
const unresolvedFreezeMax = 10 * time.Minute

// freezesPlan reports whether the plan waits this tick for an Unresolved login
// to be named. See unresolvedFreezeMax for when, and why for at most that long.
func (e *Engine) freezesPlan(ev switcher.Evaluation) bool {
	now := e.now()
	if e.unresolvedSince.IsZero() {
		e.unresolvedSince = now
	}
	unnamed, newer := ev.Login.UnnamedNewer()
	if !newer {
		return false
	}
	if uuid, asked := e.owners[unnamed.ID]; asked && uuid == "" {
		return false
	}
	if now.Sub(e.unresolvedSince) < unresolvedFreezeMax {
		return true
	}
	if !e.saidUnfroze {
		e.saidUnfroze = true
		e.logf("a newer login this store cannot name has been beside a different login for %s; the plan "+
			"runs again, and a switch writes both stores (%s)", unresolvedFreezeMax, ev.Login.Describe())
	}
	return false
}

// maxOwners bounds the identity oracle's memory. A login's identity changes on
// every refresh, so the map would otherwise grow for the life of the daemon;
// forgetting costs one more ask, rate-limited.
const maxOwners = 64

// ownerOf is the identity oracle's memory as switcher.Owners: the managed
// account a login belongs to, when the oracle has said.
func (e *Engine) ownerOf(id string) (string, bool) {
	uuid, ok := e.owners[id]
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
func (e *Engine) identifyStores(ctx context.Context, s *store.Store, ev switcher.Evaluation) {
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
		case liveAdopted:
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
				"different login in the other store (%s). The profile endpoint could not say whose it is yet; "+
				"it is not repaired from, and the plan waits for it only while it is the newer login, for "+
				"at most "+unresolvedFreezeMax.String(), st.name, ev.Login.Describe()))
		}
	}
}

func (e *Engine) sayStoreOnce(id, line string) {
	if e.saidStore == nil {
		e.saidStore = map[string]bool{}
	}
	if e.saidStore[id] {
		return
	}
	if len(e.saidStore) >= maxOwners {
		e.saidStore = map[string]bool{}
	}
	e.saidStore[id] = true
	e.logf("%s", line)
}

// remember records what the oracle said about a login.
func (e *Engine) remember(id, uuid string) {
	if e.owners == nil || len(e.owners) >= maxOwners {
		e.owners = map[string]string{}
	}
	e.owners[id] = uuid
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

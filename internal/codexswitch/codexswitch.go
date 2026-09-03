// Package codexswitch repoints the account ccdad's codex proxy serves.
//
// It is a package of its own, and a very small one, so that a machine-checkable
// statement can be made about it: its dependency closure contains neither
// internal/switcher nor internal/cclink nor internal/store, so nothing reachable
// from here can install a Claude Code credential, rewrite the credentials file,
// or read one. That is why Execute takes a uuid STRING rather than a
// store.Account -- taking the account would put the store in the closure and
// cost the guarantee for a convenience.
//
// A Codex "switch" is not a credential swap at all. Codex holds no token on
// this machine; the daemon's proxy holds them and rewrites the bearer per
// request. So all a switch does is move a pointer, and the pointer is read on
// every request rather than cached, which is what makes a repoint apply to new
// threads without disturbing threads already in flight.
package codexswitch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kweiza/ccdaddy/internal/atomicfile"
	"github.com/Kweiza/ccdaddy/internal/cclock"
	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// ErrPointerMovedUnstamped marks an Execute failure that happened AFTER the
// pointer already moved: the proxy is serving the new account, but the
// anti-flap cooldown for that switch was not recorded, so a poll tick shortly
// after can re-switch immediately with nothing holding it back. A caller that
// sees an error satisfying errors.Is(err, ErrPointerMovedUnstamped) cannot
// conclude the repoint did not happen -- it must re-read ReadServing to learn
// what is actually being served.
var ErrPointerMovedUnstamped = errors.New("codexswitch: the pointer moved but the switch cooldown was not recorded")

// dirName and fileName are the two path components, spelled once.
const (
	dirName  = "codex"
	fileName = "serving"
)

// ServingPath is where the pointer lives under a ccdad store root.
func ServingPath(root string) string {
	return filepath.Join(root, dirName, fileName)
}

// servingLockDir is a DIRECTORY, because that is what cclock's mutex is. It
// sits beside the pointer file itself, under the same "codex" directory.
const servingLockDir = fileName + ".lock"

// servingLockStale mirrors the other cross-process locks in this tree: a
// pointer write is sub-second, so anything held longer than this is a lock a
// crashed process left behind, and cclock's stale rule is what clears it.
const servingLockStale = 30 * time.Second

// servingLockTimeout bounds the wait for the pointer lock. Five seconds is the
// same bound RecordCodexSwitch waits for the state lock, for the same reason:
// the write under it is sub-second, so anything longer is a wedged lock rather
// than a busy one.
const servingLockTimeout = 5 * time.Second

// acquireServingLock takes the lock BOTH Execute and ClearIfServing hold
// across their read-then-write of the pointer, so the two can never
// interleave: a repoint landing between ClearIfServing's read and its remove,
// or a clear landing between Execute's write and its cooldown stamp, is
// exactly the gap this package used to leave open at each of its callers in
// turn. It lives here rather than inline in both because the two must take
// the identical lock directory or locking either one buys nothing against the
// other.
func acquireServingLock(root string) (*cclock.Lock, error) {
	dir := filepath.Join(root, dirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	return cclock.Acquire(filepath.Join(dir, servingLockDir), cclock.Options{
		Stale:   servingLockStale,
		Timeout: servingLockTimeout,
	})
}

// ReadServing is the account codex is served from, and whether there is one.
//
// Every failure answers "no pointer" rather than an error, and that is the
// right degradation for this file: the reader is a request handler, and a
// pointer that cannot be read means the same thing to it as a pointer that was
// never written -- fall through to the top-ranked eligible account. An account
// named by a pointer that no longer exists is the caller's to notice; this
// function only reports what the file says.
func ReadServing(root string) (string, bool) {
	raw, err := os.ReadFile(ServingPath(root))
	if err != nil {
		return "", false
	}
	uuid := strings.TrimSpace(string(raw))
	if uuid == "" {
		return "", false
	}
	return uuid, true
}

// Execute repoints codex at one account.
//
// It runs under the SAME LOCK ClearIfServing takes, so a repoint can never
// interleave with a concurrent check-then-clear on this pointer: the pointer
// is never observed half-changed by a caller deciding whether to remove it,
// and a clear that lands after Execute has already moved the pointer to a
// different account cannot undo that move on the strength of a decision made
// before it happened.
//
// TWO CRITICAL SECTIONS, IN THIS ORDER, and the order is the whole of the
// correctness here. The pointer is written first, because it is the thing that
// changes what the machine spends; the cooldown stamp is written only after
// that succeeded, because a cooldown earned by a repoint that did not happen
// would hold the lane off the retry that would have made it happen.
//
// The pointer write is atomic -- a sibling temp file and a rename -- so the
// proxy, which reads this file on every request, sees one whole uuid or the
// previous one, never a torn line.
//
// A non-nil error does NOT mean the repoint did not happen. The pointer is
// written first and the stamp only after, on purpose (see above), so a
// failure in the stamp step returns an error while the pointer has already
// moved: the proxy is already serving uuid, and the cooldown that would hold
// the lane off an immediate re-switch was never recorded. That case wraps
// ErrPointerMovedUnstamped. A caller that needs to know what is actually
// being served on any error must re-read ReadServing rather than infer it
// from Execute's return value.
//
// What it deliberately does NOT do: store.SetActive, switcher.RecordSwitch,
// releaseManagedAPIKey, SyncGlobalConfigIdentity. None of them is reachable
// from this package, which is asserted rather than remembered.
func Execute(root, uuid string) (err error) {
	uuid = strings.TrimSpace(uuid)
	if uuid == "" {
		return fmt.Errorf("codexswitch: no account named")
	}
	lock, lerr := acquireServingLock(root)
	if lerr != nil {
		return fmt.Errorf("locking the codex serving pointer: %w", lerr)
	}
	// Release's return value is part of the answer, not noise: the re-stat it
	// performs is the only check that can see a takeover in the window between
	// the touch goroutine's last tick and now, and discarding it would report
	// success for exactly the write that raced.
	defer func() { err = errors.Join(err, lock.Release()) }()

	if err := atomicfile.WriteFile(ServingPath(root), []byte(uuid+"\n"), 0o600); err != nil {
		return fmt.Errorf("repointing codex at %s: %w", uuid, err)
	}
	if err := strategy.RecordCodexSwitch(uuid); err != nil {
		return fmt.Errorf("%w: %w", ErrPointerMovedUnstamped, err)
	}
	return nil
}

// Clear removes the pointer UNCONDITIONALLY, regardless of what it currently
// names -- there is no read before the remove, so nothing here can race a
// concurrent repoint the way a caller that read the pointer first and acted
// on that reading could.
//
// Reach for THIS only when the decision to clear does not depend on what the
// pointer used to say -- a full store wipe, say, where every account is going
// away regardless and there is nothing to race. A caller that means "clear it
// if it still names the account I am about to remove" wants ClearIfServing
// instead: `ccdad remove` used to call Clear for exactly that decision, and it
// raced a daemon that had just repointed codex TO the account being removed,
// deleting a pointer that had nothing to do with the removal.
//
// Calling it on a machine with no pointer is not a failure -- nothing asks
// first.
func Clear(root string) error {
	if err := os.Remove(ServingPath(root)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clearing the codex serving pointer: %w", err)
	}
	return nil
}

// ClearIfServing clears the pointer only if it currently names uuid, and does
// the read and the remove under the SAME LOCK Execute takes -- so a repoint
// landing between the two cannot be undone by a decision made before it
// happened. That is the gap a bare ReadServing-then-Clear at the call site
// cannot close: the pointer can legitimately move between the read and the
// remove, and nothing outside this package can serialise against Execute
// without importing it.
//
// cleared reports whether the pointer was actually removed, which is false
// both when there was no pointer and when the pointer named a different
// account -- a caller that must say something different in the two cases
// needs ReadServing's own answer, not this one.
func ClearIfServing(root, uuid string) (cleared bool, err error) {
	uuid = strings.TrimSpace(uuid)
	if uuid == "" {
		return false, nil
	}
	lock, lerr := acquireServingLock(root)
	if lerr != nil {
		return false, fmt.Errorf("locking the codex serving pointer: %w", lerr)
	}
	defer func() { err = errors.Join(err, lock.Release()) }()

	current, ok := ReadServing(root)
	if !ok || current != uuid {
		return false, nil
	}
	clearIfServingRaceHook()
	if err := os.Remove(ServingPath(root)); err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("clearing the codex serving pointer: %w", err)
	}
	return true, nil
}

// clearIfServingRaceHook runs between ClearIfServing's own read of the pointer
// and its removal of it -- while still holding acquireServingLock's lock, if
// there is one to hold. Production code never overrides it; the zero value is
// a no-op, so it costs nothing here.
//
// It exists only so a test can widen that gap on purpose. Left alone it is a
// handful of microseconds -- Execute's write (its own lock, a temp file, a
// rename, then a SEPARATE strategy-lock stamp) takes far longer than that, so
// concurrent goroutines essentially never land inside it by chance; measured
// at over three thousand unweighted attempts with none landing. Widening it
// deliberately is the same move cclock's own tests make with
// setStatLockForTest to force ITS takeover window open -- and a ClearIfServing
// that is genuinely still serialized against Execute must tolerate the gap
// being wide open just as well as it tolerates it being a few microseconds.
var clearIfServingRaceHook = func() {}

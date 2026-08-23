package credhome

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// ErrLocksUnsupported reports that the filesystem holding Claude Code's
// credential home cannot do locks at all — ENOLCK on an NFS or CIFS mount with
// no lock daemon, or a GOOS gofrs/flock has no implementation for.
//
// It is this package's own sentinel rather than internal/daemon's, and that is
// the tree's stated rule rather than an oversight: each package owns its own
// lock file, and store/lock.go records why coupling them to share twenty lines
// is the wrong trade. It also matters that the two are distinguishable — a
// machine whose STORE is local and whose credential home sits on a network
// mount reaches this one with the singleton already taken, and `ccdad doctor`
// has to be able to say which of the two filesystems is the problem.
//
// Unlike the store's, this one is never a refusal. An engine that cannot claim
// the credential home degrades and keeps running: refusing would take ccdad
// away from every machine with a network home — a configuration that works
// today — to guard a hazard that requires a second store to exist at all.
var ErrLocksUnsupported = errors.New("this filesystem does not support locks")

const (
	// A probe takes the lock it reads — there is no read-only "is it held"
	// primitive — so an engine starting at the same moment as a probe can lose
	// a race it should win. Three attempts, a hundred milliseconds apart, the
	// same window internal/daemon's singleton uses.
	acquireAttempts   = 3
	acquireRetryDelay = 100 * time.Millisecond
)

// tryLockMu guards tryLockImpl. The daemon's tick loop reaches this package
// from a background goroutine, and swapping a package-level function value out
// from under a concurrent reader is a data race under the Go memory model no
// matter how much time separates the two. Guarding both sides gives the
// override a real happens-before edge.
var (
	tryLockMu   sync.RWMutex
	tryLockImpl = defaultTryLock
)

// defaultTryLock takes the claim at path without blocking.
//
// create decides whether the lock file may be brought into existence, and it is
// the difference between acquiring and probing. gofrs/flock defaults its open
// flags to os.O_CREATE|os.O_RDONLY, so a bare probe would CREATE engine.lock
// and destroy the one piece of evidence that says no engine has ever claimed
// this credential home. flock.SetFlag REPLACES those flags rather than adding
// to them, and os.O_RDONLY is 0, so passing it removes O_CREATE — after which a
// missing file surfaces as ENOENT from the open. An exclusive flock(2) on a
// read-only descriptor is legal, which is what makes the probe possible at all.
//
// On success the returned release closure is the only thing keeping the
// *flock.Flock reachable, and that matters: os.File carries a finalizer that
// closes the fd, flock(2) releases on last close of the open file description,
// and a *Flock that becomes unreachable therefore drops the claim with no error
// anywhere. internal/daemon reproduced exactly that — three runtime.GC() cycles
// were enough — which is why Acquire also anchors the claim in a package-level
// variable for the life of the process.
//
// A probe takes a SHARED lock, and that is not an optimisation. An exclusive
// probe contends with every OTHER PROBE as well as with a holder, and a
// contended try-lock is indistinguishable from a held one — so two ccdad
// commands probing at the same instant would each report an engine whose only
// holder was the other one. This is on the hot path of `maybeAutoStart`, so
// there are genuinely many probers.
func defaultTryLock(path string, create bool) (locked bool, release func() error, err error) {
	var (
		fl *flock.Flock
		ok bool
	)
	if create {
		fl = flock.New(path)
		ok, err = fl.TryLock()
	} else {
		fl = flock.New(path, flock.SetFlag(os.O_RDONLY))
		ok, err = fl.TryRLock()
	}
	if err != nil {
		return false, nil, err
	}
	if !ok {
		// TryLock reports contention as (false, nil), NOT as an error. Any gate
		// written as `if err != nil` reads "another engine is driving this
		// login" as success, which is why this returns three values.
		return false, nil, nil
	}
	// A method value, so the closure holds the *Flock for as long as the caller
	// holds the closure.
	return true, fl.Unlock, nil
}

func tryLock(path string, create bool) (bool, func() error, error) {
	tryLockMu.RLock()
	impl := tryLockImpl
	tryLockMu.RUnlock()
	return impl(path, create)
}

// SetTryLockForTest replaces the primitive tryLock calls and returns a function
// restoring the previous one. It exists so a test can reach the "cannot
// determine" branch, which is otherwise only reachable on a filesystem where
// locking is broken. Production code must never call it.
//
// It is EXPORTED, unlike the equivalent in internal/daemon and internal/store,
// because the callers whose behaviour depends on this branch — the daemon's
// startup, `ccdad auto`, the switch executor, `ccdad doctor` — all live in
// other packages.
//
// A fake passed to this must DISPATCH ON PATH if the test also reaches a
// caller that takes the daemon singleton. Both locks are try-locked during a
// daemon start, they fail in a fixed order, and a path-blind fake makes the
// first one answer for the second: a test meaning to describe an unlockable
// credential home would in fact be describing an unlockable store, and would
// pass while never executing a line of this package.
func SetTryLockForTest(fn func(string, bool) (bool, func() error, error)) (restore func()) {
	tryLockMu.Lock()
	prev := tryLockImpl
	tryLockImpl = fn
	tryLockMu.Unlock()
	return func() {
		tryLockMu.Lock()
		tryLockImpl = prev
		tryLockMu.Unlock()
	}
}

// classifyLockError turns a lock failure into the error the caller has to be
// able to act on, leaving everything else as itself.
//
// errors.ErrUnsupported covers the platforms gofrs/flock has no implementation
// for — every method there returns it. The errno cases are unix-only and live
// in a build-tagged file, because syscall does not define them everywhere.
func classifyLockError(err error) error {
	if errors.Is(err, errors.ErrUnsupported) || locksUnsupported(err) {
		// Both verbs are %w. Classifying must not consume the cause: `ccdad
		// doctor` wants to print the errno it actually got, and a caller that
		// checks for a specific one would otherwise find nothing in the chain.
		return fmt.Errorf("%w: %w", ErrLocksUnsupported, err)
	}
	return err
}

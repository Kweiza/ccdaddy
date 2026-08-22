package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// ErrSingletonHeld reports that another process holds the singleton — a daemon
// is already running. It is a sentinel rather than a plain error because the
// callers that matter have to tell it apart from "cannot determine": `ccdad
// daemon status` maps the three outcomes onto exits 0, 5 and 1, and `ccdad
// auto` without --once refuses on this one specifically rather than failing.
// Two processes executing switches would fight the cooldown and anti-flap
// state the engine persists to disk, so exactly one must lose.
var ErrSingletonHeld = errors.New("another ccdad daemon holds the singleton")

// ErrLocksUnsupported reports that this filesystem cannot do locks at all —
// ENOLCK on an NFS or CIFS mount with no lock daemon, or a GOOS gofrs/flock
// has no implementation for. It is never folded into "not running": a
// supervisor gating on that would respawn forever. `ccdad doctor` exists to
// name this condition, which is why it is distinguishable rather than just an
// error.
var ErrLocksUnsupported = errors.New("this filesystem does not support locks")

const (
	// A probe takes the lock it reads — there is no read-only "is it held"
	// primitive — so a daemon starting at the same moment as a probe can lose
	// a race it should win. Three attempts, a hundred milliseconds apart.
	acquireAttempts   = 3
	acquireRetryDelay = 100 * time.Millisecond

	// The window the retry is worth, as a literal rather than as a multiple of
	// the constant above: an assertion written in terms of the value it is
	// checking moves with it, so shrinking the delay would shrink the test too.
	acquireRetryBudget = 200 * time.Millisecond
)

// tryLockMu guards tryLockImpl. Nothing in this package reads it from a
// background goroutine today, but the daemon's tick loop will, and swapping a
// package-level function value out from under a concurrent reader is a data
// race under the Go memory model no matter how much time separates the two.
// Guarding both sides gives the override a real happens-before edge, which is
// the same reason cclock guards its own stat seam.
var (
	tryLockMu   sync.RWMutex
	tryLockImpl = defaultTryLock
)

// defaultTryLock takes the singleton lock at path without blocking.
//
// create decides whether the lock file may be brought into existence, and it
// is the difference between acquiring and probing. gofrs/flock defaults its
// open flags to os.O_CREATE|os.O_RDONLY, so a bare probe would CREATE
// ccdad.lock and destroy the one piece of evidence that says no daemon has
// ever started here. flock.SetFlag REPLACES those flags rather than adding to
// them, and os.O_RDONLY is 0, so passing it removes O_CREATE — after which a
// missing file surfaces as ENOENT from the open. An exclusive flock(2) on a
// read-only descriptor is legal, which is what makes the probe possible at all.
//
// On success the returned release closure is the only thing that keeps the
// *flock.Flock reachable, and that matters: os.File carries a finalizer that
// closes the fd, flock(2) releases on last close of the open file description,
// and a *Flock that becomes unreachable therefore drops the lock with no error
// anywhere. Reproduced on this machine — three runtime.GC() cycles were enough.
//
// A probe takes a SHARED lock, and that is not an optimisation. An exclusive
// probe contends with every OTHER PROBE as well as with a daemon, and a
// contended try-lock is indistinguishable from a held one — so two ccdad
// commands probing at the same instant would each report a running daemon
// whose only holder was the other one. Measured with this library on this
// machine: sixteen concurrent exclusive probers against a free lock produced
// 2059 false "running" answers out of 8000, and the same run with shared locks
// produced none. A shared lock still fails against the daemon's exclusive one,
// which is the answer the probe exists to give.
//
// The reverse direction is unchanged and deliberate: a live shared probe does
// briefly block an exclusive acquire, which is what the acquire retry is for.
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
		// TryLock reports contention as (false, nil), NOT as an error. Any
		// gate written as `if err != nil` reads "another daemon is running" as
		// success, which is why this returns three values.
		return false, nil, nil
	}
	// A method value, so the closure holds the *Flock for as long as the
	// caller holds the closure.
	return true, fl.Unlock, nil
}

func tryLock(path string, create bool) (bool, func() error, error) {
	tryLockMu.RLock()
	impl := tryLockImpl
	tryLockMu.RUnlock()
	return impl(path, create)
}

// setTryLockForTest replaces the primitive tryLock calls and returns a function
// restoring the previous one. It exists so a test can reach the "cannot
// determine" branch, which is otherwise only reachable on a filesystem where
// locking is broken. Production code must never call it.
func setTryLockForTest(fn func(string, bool) (bool, func() error, error)) (restore func()) {
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

// SingletonHeld reports whether a daemon is running.
//
// It never returns (false, nil) on an I/O failure. That is the whole contract:
// a supervisor gating on this would respawn forever on a filesystem where locks
// do not work, so "cannot determine" has to be a third outcome rather than
// folding into "not running". `ccdad daemon status` spends the three on exits
// 0, 5 and 1.
//
// It never creates the lock file. A missing file is genuine evidence that no
// daemon has ever started against this store, and a probe that manufactures it
// destroys that evidence permanently.
//
// A missing store DIRECTORY answers "not running" too, for the same reason a
// missing lock file does — a directory that does not exist has no daemon in it.
// It is indistinguishable from a mistyped CCDAD_HOME at this layer: both
// produce a *fs.PathError satisfying os.ErrNotExist. "Your store points at
// nothing" is a configuration question, and `ccdad doctor` is where it gets
// answered; reporting "cannot determine" for a fresh install would be worse.
//
// Held is not the same as held-by-someone-else. flock(2) is per open file
// description, so a process that already holds the singleton sees its own lock
// through a second descriptor exactly as it would see another process's. The
// answer "a daemon is running" is still correct.
func SingletonHeld() (held bool, err error) {
	root, err := storeRoot()
	if err != nil {
		return false, err
	}
	locked, release, err := tryLock(filepath.Join(root, LockFileName), false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, classifyLockError(err)
	}
	if !locked {
		// A shared probe only fails against an exclusive holder, and the only
		// thing that takes this lock exclusively is a daemon.
		return true, nil
	}
	// We took it, so nobody held it. Give it straight back — and report a
	// failure to do so rather than swallowing it, because a probe that keeps
	// the singleton locks out the daemon it just said was not running.
	if err := release(); err != nil {
		return false, fmt.Errorf("releasing the singleton after probing it: %w", err)
	}
	return false, nil
}

// Singleton is a held singleton lock. It is released when the process exits,
// by the kernel, which is the property that makes it the sole authority on
// whether a daemon is running: no staleness heuristic can be wrong about it.
type Singleton struct {
	release func() error
}

// heldMu guards heldSingleton, which exists for exactly one reason: to keep the
// acquired lock reachable for the life of the process. See defaultTryLock.
var (
	heldMu        sync.Mutex
	heldSingleton *Singleton
)

// AcquireSingleton takes the singleton, or reports why it could not.
//
// A lost race is ErrSingletonHeld and nothing else, so a caller can tell it
// from a filesystem that cannot lock. It retries first: a probe momentarily
// OWNS the lock it reads, so a daemon starting alongside `ccdad daemon status`
// can lose a race it should win.
//
// The lock file is created here and never unlinked — not on shutdown, not by
// uninstall, not by a test teardown. flock is per-inode, so delete-and-recreate
// lets two daemons each hold "the" lock on a different inode.
func AcquireSingleton() (*Singleton, error) {
	heldMu.Lock()
	defer heldMu.Unlock()
	if heldSingleton != nil {
		return nil, fmt.Errorf("this process already holds the singleton: %w", ErrSingletonHeld)
	}
	root, err := storeRoot()
	if err != nil {
		return nil, err
	}
	// The acquire path creates the store directory; the probe path must never
	// do so, because a probe that manufactures the directory manufactures the
	// evidence it was asked to read.
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("creating the ccdad store: %w", err)
	}
	for attempt := 1; ; attempt++ {
		locked, release, err := tryLock(filepath.Join(root, LockFileName), true)
		if err != nil {
			return nil, classifyLockError(err)
		}
		if locked {
			s := &Singleton{release: release}
			heldSingleton = s
			return s, nil
		}
		if attempt >= acquireAttempts {
			return nil, ErrSingletonHeld
		}
		time.Sleep(acquireRetryDelay)
	}
}

// Release gives the singleton back. It is safe on a nil receiver and safe to
// call twice, so a shutdown path can call it without tracking whether some
// earlier path already did.
//
// It unlocks; it never removes the file.
func (s *Singleton) Release() error {
	if s == nil {
		return nil
	}
	heldMu.Lock()
	defer heldMu.Unlock()
	if s.release == nil {
		return nil
	}
	err := s.release()
	s.release = nil
	if heldSingleton == s {
		heldSingleton = nil
	}
	if err != nil {
		return fmt.Errorf("releasing the singleton: %w", err)
	}
	return nil
}

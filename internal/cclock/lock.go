// Package cclock cooperates with the advisory locks Claude Code takes while
// mutating its own files.
//
// Claude Code uses the npm proper-lockfile package, whose mutex is mkdir of a
// DIRECTORY — not a lock file, not flock. Reimplementing it exactly is what
// makes ccdad's writes and Claude Code's token refreshes mutually exclusive:
//
//   - mkdir(lockPath) succeeds        => acquired
//   - EEXIST                          => stat it, and steal (rmdir + retry)
//     only if mtime < now - stale
//   - the holder utimes the directory every TouchInterval so waiters do not
//     deem it stale, and verifies its own mtime before every touch so it
//     notices when a waiter stole the lock out from under a stalled toucher
//
// Skipping the touch makes a long hold look abandoned and lets a waiter steal
// a live lock. Skipping the staleness check makes a crashed holder wedge every
// future writer forever. Skipping the ownership check before each touch lets
// a lock's former holder go on believing it is still the exclusive owner
// after a takeover, because Chtimes on the same path silently succeeds
// against the new owner's directory too.
package cclock

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrTimeout is returned when a lock stayed held past Options.Timeout.
var ErrTimeout = errors.New("lock is held by another process")

// ErrCompromised means the lock was taken over by another process while
// held. Whatever the lock was protecting must be treated as not written.
var ErrCompromised = errors.New("lock was taken over by another process while held")

// DefaultTouchInterval is how often a holder advances the lock's mtime. It is
// deliberately faster than Claude Code's own 5s so a ccdad hold always looks
// live to a Claude Code waiter.
const DefaultTouchInterval = 3 * time.Second

// statRetries and statRetryDelay bound the retry on a stat of a directory we
// have just successfully created or touched. A failure there is a filesystem
// blip rather than a protocol event, and treating the first one as fatal
// turns a transient error into a permanent orphaned lock.
const (
	statRetries    = 3
	statRetryDelay = 5 * time.Millisecond
)

// statLockMu guards statLockImpl. touch runs for as long as a Lock is held,
// so a test that wants to force statLock's failure path is necessarily
// swapping the implementation out from under a concurrently running
// goroutine; without this lock that swap and touch's read of the same
// package-level function value race under the Go memory model regardless of
// how much time separates them; -race deterministically catches it within a
// handful of iterations. Guarding both sides gives the override a real
// happens-before edge.
var (
	statLockMu   sync.RWMutex
	statLockImpl = defaultStatLock
)

// defaultStatLock is the implementation statLockImpl starts as, and the one
// production code runs under normal operation.
func defaultStatLock(dir string) (os.FileInfo, error) {
	var err error
	for i := 0; i < statRetries; i++ {
		var info os.FileInfo
		if info, err = os.Stat(dir); err == nil {
			return info, nil
		}
		time.Sleep(statRetryDelay)
	}
	return nil, err
}

// statLock reads a lock directory's metadata, retrying briefly on failure.
// Production code must always go through this function, never through
// statLockImpl directly.
func statLock(dir string) (os.FileInfo, error) {
	statLockMu.RLock()
	impl := statLockImpl
	statLockMu.RUnlock()
	return impl(dir)
}

// setStatLockForTest replaces the implementation statLock calls and returns
// a function that restores the previous one. It exists only so tests can
// force statLock's failure path while a Lock's touch goroutine may be
// concurrently calling it; production code must never call it.
func setStatLockForTest(fn func(string) (os.FileInfo, error)) (restore func()) {
	statLockMu.Lock()
	prev := statLockImpl
	statLockImpl = fn
	statLockMu.Unlock()
	return func() {
		statLockMu.Lock()
		statLockImpl = prev
		statLockMu.Unlock()
	}
}

// Options configures one acquisition.
type Options struct {
	// Stale is how old a lock's mtime must be before it may be taken over.
	// Use the value Claude Code uses for that lock; never a shorter one, or a
	// live holder whose toucher stalled (suspend, blocked event loop) gets its
	// lock stolen out from under it.
	Stale time.Duration
	// Timeout bounds how long Acquire waits. Zero means a single attempt.
	Timeout time.Duration
	// TouchInterval is how often the mtime is advanced while held.
	// Zero means DefaultTouchInterval. Must be at most half of Stale, or a
	// holder's own lock could go stale by its own definition between two
	// touches; Acquire rejects a contradictory pair rather than clamping it.
	TouchInterval time.Duration
}

// Lock is a held directory lock.
type Lock struct {
	dir      string
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}

	lost     chan struct{} // closed when ownership is lost
	lostOnce sync.Once

	mu    sync.Mutex
	mtime time.Time // the mtime this holder last observed on its own directory

	releaseErr error // the outcome of the first Release call; every call returns this
}

// Acquire takes the lock at lockDir, waiting up to opts.Timeout.
func Acquire(lockDir string, opts Options) (*Lock, error) {
	if opts.TouchInterval <= 0 {
		opts.TouchInterval = DefaultTouchInterval
	}
	if opts.TouchInterval*2 > opts.Stale {
		return nil, fmt.Errorf("cclock: touch interval %s must be at most half of stale threshold %s, or a stalled toucher lets the lock go stale by its own definition between touches", opts.TouchInterval, opts.Stale)
	}
	if err := os.MkdirAll(filepath.Dir(lockDir), 0o700); err != nil {
		return nil, fmt.Errorf("creating lock parent: %w", err)
	}

	deadline := time.Now().Add(opts.Timeout)
	for attempt := 0; ; attempt++ {
		// Bound every path back to the top of this loop, including the two
		// "continue" paths below: a stale lock whose removal keeps failing
		// (non-empty directory, read-only mount, ...) must not spin forever
		// just because it never reaches the backoff at the bottom. The very
		// first attempt is exempt so Timeout == 0's documented "a single
		// attempt" still gets to make that attempt instead of expiring
		// before ever calling Mkdir.
		if attempt > 0 && time.Now().After(deadline) {
			return nil, fmt.Errorf("%s: %w", filepath.Base(lockDir), ErrTimeout)
		}

		err := os.Mkdir(lockDir, 0o700)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("acquiring %s: %w", filepath.Base(lockDir), err)
		}

		// Held by someone. Steal it only if the holder is provably gone.
		info, statErr := os.Stat(lockDir)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				continue // released between mkdir and stat; retry immediately
			}
			return nil, fmt.Errorf("inspecting %s: %w", filepath.Base(lockDir), statErr)
		}
		if time.Since(info.ModTime()) > opts.Stale {
			if rmErr := os.Remove(lockDir); rmErr != nil {
				// Another waiter beat us to the removal, or we cannot remove it
				// at all. Either way, do not spin hot.
				time.Sleep(50 * time.Millisecond)
			}
			continue
		}

		// Jittered backoff so several waiters do not retry in lockstep.
		time.Sleep(250*time.Millisecond + time.Duration(rand.N(250))*time.Millisecond)
	}

	info, err := statLock(lockDir)
	if err != nil {
		// The directory was created microseconds ago by the Mkdir above, so
		// no waiter can have stolen it yet -- stealing requires an mtime
		// older than Stale, and nothing has had time to elapse. Removing it
		// here is therefore safe in a way it is NOT safe in Release, where a
		// long hold may genuinely have outlived Stale and the directory may
		// already belong to a new owner. Leaving it in place on this path
		// would instead orphan it: no *Lock is returned, so nothing could
		// ever call Release, and it would block every other acquirer until
		// Stale elapses.
		_ = os.Remove(lockDir)
		return nil, fmt.Errorf("inspecting %s after acquire: %w", filepath.Base(lockDir), err)
	}

	lk := &Lock{
		dir:   lockDir,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
		lost:  make(chan struct{}),
		mtime: info.ModTime(),
	}
	go lk.touch(opts.TouchInterval)
	return lk, nil
}

// touch advances the lock's mtime until the lock is released, so no waiter
// deems a live hold stale.
//
// Before every touch it verifies the directory still carries the mtime this
// holder last wrote. If it does not, a waiter deemed us stale and took the
// lock (rmdir + mkdir) while our ticker was stalled -- and Chtimes on the same
// path would silently succeed against THAT holder's directory, leaving two
// processes each believing they hold the lock. proper-lockfile calls this
// ECOMPROMISED; we stop touching and mark ownership lost.
func (l *Lock) touch(every time.Duration) {
	defer close(l.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			info, err := statLock(l.dir)
			if err != nil {
				l.markLost() // removed outright; we no longer hold anything
				return
			}
			l.mu.Lock()
			owned := info.ModTime().Equal(l.mtime)
			l.mu.Unlock()
			if !owned {
				l.markLost()
				return
			}
			now := time.Now()
			if err := os.Chtimes(l.dir, now, now); err != nil {
				l.markLost()
				return
			}
			// Read back what actually landed on disk rather than trusting
			// `now`: a coarse-granularity filesystem may store a rounded
			// value. If that read-back cannot be verified even after
			// retrying, we no longer know our own mtime and must not keep
			// asserting ownership on unverified state: silently keeping the
			// stale in-memory value would make the *next* tick's comparison
			// fail against the (genuinely changed, just unread) on-disk
			// value and manufacture a false compromise for a lock nobody
			// actually stole. Fail closed instead -- a lock whose ownership
			// cannot be verified must not keep being asserted.
			info, err = statLock(l.dir)
			if err != nil {
				l.markLost()
				return
			}
			l.mu.Lock()
			l.mtime = info.ModTime()
			l.mu.Unlock()
		}
	}
}

// markLost records that this holder no longer owns the lock directory. It is
// idempotent.
func (l *Lock) markLost() {
	l.lostOnce.Do(func() {
		close(l.lost)
	})
}

// Compromised is closed when this lock discovers it no longer owns its
// directory. A caller holding the lock across more than a brief critical
// section should select on it and abandon whatever it was protecting.
func (l *Lock) Compromised() <-chan struct{} {
	if l == nil {
		return nil
	}
	return l.lost
}

// Release stops touching and removes the lock directory. It is idempotent:
// every call, including a concurrent one, observes the exact outcome of the
// first -- sync.Once guarantees that, but only because the outcome itself is
// stored on the Lock rather than in a local variable a second caller would
// never see.
//
// If the lock was compromised -- taken over by another process while held --
// Release does not remove the directory (it belongs to the new owner now)
// and returns ErrCompromised instead. This is checked twice. First, via
// l.lost, which the touch goroutine sets on its own ticker -- so a takeover
// can be up to one TouchInterval old before that goroutine notices it. Second,
// synchronously, right here: a final stat of the directory compared against
// the mtime this holder last confirmed. The second check is what catches a
// takeover in the window between touch's last tick and this call, which the
// first check alone cannot see -- and it is why Release must never remove
// the directory on the strength of l.lost being merely unclosed: unclosed
// only means "not detected yet", not "did not happen".
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	l.stopOnce.Do(func() {
		close(l.stop)
		<-l.done

		select {
		case <-l.lost:
			l.releaseErr = ErrCompromised
			return
		default:
		}

		// touch has already exited (we waited on l.done above), so nothing
		// else in this process can be racing this read or l.mtime.
		info, statErr := statLock(l.dir)
		l.mu.Lock()
		owned := statErr == nil && info.ModTime().Equal(l.mtime)
		l.mu.Unlock()
		if !owned {
			// Mark it lost too, not just report it: a caller separately
			// watching Compromised() must see the same event Release just
			// reported, not silence, or the two signals disagree about
			// whether this ever happened.
			l.markLost()
			l.releaseErr = ErrCompromised
			return
		}

		if rmErr := os.Remove(l.dir); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			l.releaseErr = fmt.Errorf("releasing %s: %w", filepath.Base(l.dir), rmErr)
		}
	})
	return l.releaseErr
}

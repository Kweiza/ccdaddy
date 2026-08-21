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
	for {
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

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s: %w", filepath.Base(lockDir), ErrTimeout)
		}
		// Jittered backoff so several waiters do not retry in lockstep.
		time.Sleep(250*time.Millisecond + time.Duration(rand.N(250))*time.Millisecond)
	}

	info, err := os.Stat(lockDir)
	if err != nil {
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
			info, err := os.Stat(l.dir)
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
			// Re-stat rather than trusting `now`: a filesystem may store a
			// coarser mtime than we asked for, and the next comparison has to
			// match what actually landed on disk, not what we requested.
			if info, err := os.Stat(l.dir); err == nil {
				l.mu.Lock()
				l.mtime = info.ModTime()
				l.mu.Unlock()
			}
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
func (l *Lock) Compromised() <-chan struct{} { return l.lost }

// Release stops touching and removes the lock directory. It is idempotent.
//
// If the lock was compromised -- taken over by another process while held --
// Release does not remove the directory (it belongs to the new owner now)
// and returns ErrCompromised instead.
func (l *Lock) Release() error {
	var err error
	l.stopOnce.Do(func() {
		close(l.stop)
		<-l.done

		select {
		case <-l.lost:
			err = ErrCompromised
			return
		default:
		}

		if rmErr := os.Remove(l.dir); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			err = fmt.Errorf("releasing %s: %w", filepath.Base(l.dir), rmErr)
		}
	})
	return err
}

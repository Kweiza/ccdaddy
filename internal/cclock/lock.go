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
//     deem it stale
//
// Skipping the touch makes a long hold look abandoned and lets a waiter steal
// a live lock. Skipping the staleness check makes a crashed holder wedge every
// future writer forever.
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
	// Zero means DefaultTouchInterval.
	TouchInterval time.Duration
}

// Lock is a held directory lock.
type Lock struct {
	dir      string
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// Acquire takes the lock at lockDir, waiting up to opts.Timeout.
func Acquire(lockDir string, opts Options) (*Lock, error) {
	if opts.TouchInterval <= 0 {
		opts.TouchInterval = DefaultTouchInterval
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

	lk := &Lock{dir: lockDir, stop: make(chan struct{}), done: make(chan struct{})}
	go lk.touch(opts.TouchInterval)
	return lk, nil
}

// touch advances the lock's mtime until the lock is released, so no waiter
// deems a live hold stale.
func (l *Lock) touch(every time.Duration) {
	defer close(l.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			now := time.Now()
			if err := os.Chtimes(l.dir, now, now); err != nil {
				return // stolen or removed; nothing left to keep alive
			}
		}
	}
}

// Release stops touching and removes the lock directory. It is idempotent.
func (l *Lock) Release() error {
	var err error
	l.stopOnce.Do(func() {
		close(l.stop)
		<-l.done
		if rmErr := os.Remove(l.dir); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			err = fmt.Errorf("releasing %s: %w", filepath.Base(l.dir), rmErr)
		}
	})
	return err
}

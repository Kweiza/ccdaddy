package cclock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// Lock parameters, verbatim from Claude Code 2.1.238. Do not shorten them:
// Stale is the window in which a live holder's lock may NOT be stolen, so a
// smaller value here steals locks Claude Code still owns.
const (
	// storageWriteStale is proper-lockfile's stale window for .storage-write.
	storageWriteStale = 15 * time.Second
	// refreshStale is the window for both OAuth refresh locks.
	refreshStale = 60 * time.Second
)

// oauthRefreshLockDir, legacyRefreshLockDir and storageWriteLockDir compute
// each lock path from an already-resolved credential home, so a caller that
// needs more than one of them -- AcquireCredentials needs all three -- can
// resolve ccpath.CredentialHome() exactly once and pass it in. Resolving it
// separately per path would let CLAUDE_SECURESTORAGE_CONFIG_DIR /
// CLAUDE_CONFIG_DIR / HOME change between calls (ccdad itself mutates these
// for per-account scoping) and split the three locks across two directories
// with no error anywhere.
func oauthRefreshLockDir(home string) string {
	return filepath.Join(home, ".oauth_refresh.lock")
}

func legacyRefreshLockDir(home string) string {
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	return home + ".lock"
}

func storageWriteLockDir(home string) string {
	return filepath.Join(home, ".storage-write.lock")
}

// StorageWriteLockDir guards every read-modify-write of .credentials.json.
// cswap does not take this lock; ccdad does.
func StorageWriteLockDir() string {
	return storageWriteLockDir(ccpath.CredentialHome())
}

// OAuthRefreshLockDir is Claude Code's primary token-refresh lock.
func OAuthRefreshLockDir() string {
	return oauthRefreshLockDir(ccpath.CredentialHome())
}

// LegacyRefreshLockDir is the older sibling-of-the-directory lock Claude Code
// still takes for compatibility with external tools. Claude Code names it after
// the REAL path of the credential home, so a symlinked home must resolve first
// or the two processes lock different files and exclude nothing.
func LegacyRefreshLockDir() string {
	return legacyRefreshLockDir(ccpath.CredentialHome())
}

// lockStep is one entry in the credential-lock sequence.
type lockStep struct {
	dir   string
	stale time.Duration
}

// credentialLockOrder is the sequence AcquireCredentials takes against the
// given (already-resolved) credential home, in Claude Code's own order, with
// Claude Code's own stale windows (60s, 60s, 15s, read directly from the
// 2.1.238 binary).
//
// Extracted so a test can pin it. Both properties are load-bearing and
// neither is observable from the outcome of an acquisition: the ORDER is
// what makes a deadlock between a waiting ccdad and a waiting Claude Code
// impossible, and the STALE values matching Claude Code's own mean ccdad
// never deems stale a lock Claude Code still legitimately holds. Reversing
// the order, or shortening a window, leaves every behavioural test in this
// package green -- TestCredentialLockOrder pins this function's exact output
// to catch that class of regression directly, since no test of externally
// observable behaviour can.
func credentialLockOrder(home string) []lockStep {
	return []lockStep{
		{oauthRefreshLockDir(home), refreshStale},
		{legacyRefreshLockDir(home), refreshStale},
		{storageWriteLockDir(home), storageWriteStale},
	}
}

// Held is a set of credential locks held together.
type Held struct {
	locks []*Lock
	home  string

	compromised     chan struct{}
	compromisedOnce sync.Once
	watchStop       chan struct{}
	watchDone       chan struct{}
	releaseOnce     sync.Once
	releaseErr      error
}

// newHeld wraps already-acquired locks and starts the goroutine that fans
// their individual Compromised channels into Held's aggregate one. home is
// the credential home directory these locks actually cover, resolved once by
// the caller.
func newHeld(locks []*Lock, home string) *Held {
	h := &Held{
		locks:       locks,
		home:        home,
		compromised: make(chan struct{}),
		watchStop:   make(chan struct{}),
		watchDone:   make(chan struct{}),
	}
	go h.watch()
	return h
}

// watch closes h.compromised the moment any member lock reports a takeover.
// It exits as soon as Release closes watchStop, so it never outlives the
// Held it belongs to -- Release waits on watchDone before returning, which
// is what guarantees this goroutine (and the one it starts per lock) cannot
// leak past Release.
func (h *Held) watch() {
	defer close(h.watchDone)
	var wg sync.WaitGroup
	for _, lk := range h.locks {
		wg.Add(1)
		go func(lk *Lock) {
			defer wg.Done()
			select {
			case <-lk.Compromised():
				h.compromisedOnce.Do(func() { close(h.compromised) })
			case <-h.watchStop:
			}
		}(lk)
	}
	wg.Wait()
}

// Compromised is closed the moment any one of the held locks is taken over
// by another process. A caller holding these across a credentials write
// should select on it and treat the write as NOT durable if it fires: a
// compromised lock means Claude Code may believe it now owns exclusive
// access to that lock and could be concurrently reading or writing the same
// credentials file.
//
// A nil *Held reports no compromise: it returns a channel that never closes,
// the same behaviour a nil receiver gives a caller who skipped the error
// check on a failed AcquireCredentials.
func (h *Held) Compromised() <-chan struct{} {
	if h == nil {
		return nil
	}
	return h.compromised
}

// CredentialHome is the credential home directory this Held's locks actually
// cover -- the value ccpath.CredentialHome() resolved to at the moment
// AcquireCredentials was called, not a fresh re-resolution. A caller about to
// write the credentials file under this Held must write under THIS
// directory rather than calling ccpath.CredentialHome() again: the
// environment variables it reads are exactly the ones ccdad itself mutates
// for per-account credential scoping, so a change between resolutions would
// lock one directory and write another.
func (h *Held) CredentialHome() string {
	if h == nil {
		return ""
	}
	return h.home
}

// Release gives the locks back in reverse acquisition order, releasing all
// three even if one of them fails. It is idempotent: every call, including
// concurrent ones, observes the exact outcome of the first.
//
// If any lock was compromised -- taken over by another process while held --
// the returned error satisfies errors.Is(err, ErrCompromised), regardless of
// what the other locks' Release calls returned and regardless of iteration
// order. A compromise means Claude Code may have concurrently touched the
// credentials file; that is the failure a caller must act on, so it must
// never be masked by a more mundane error from another lock in the set.
func (h *Held) Release() error {
	if h == nil {
		return nil
	}
	h.releaseOnce.Do(func() {
		close(h.watchStop)
		<-h.watchDone

		var errs []error
		for i := len(h.locks) - 1; i >= 0; i-- {
			if e := h.locks[i].Release(); e != nil {
				errs = append(errs, e)
			}
		}
		h.locks = nil
		h.releaseErr = errors.Join(errs...)
	})
	return h.releaseErr
}

// AcquireCredentials takes all three credential locks in Claude Code's own
// order: the primary refresh lock, then the legacy one, then the storage-write
// lock around the actual write.
//
// The order is load-bearing. Mirroring Claude Code's sequence means a waiting
// ccdad and a waiting Claude Code can never hold each other's next lock, so a
// deadlock between them is impossible. Taking them in any other order
// reintroduces one.
//
// timeout is per lock, so the worst case is roughly 3x timeout plus up to one
// backoff interval per lock.
//
// Never make a network call while these are held. Claude Code's refresh reads,
// round-trips the token endpoint, and saves, all under the two refresh locks;
// holding them across our own network call would stall Claude Code for the
// duration of our request.
func AcquireCredentials(timeout time.Duration) (*Held, error) {
	home := ccpath.CredentialHome()
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("creating credential home: %w", err)
	}

	var locks []*Lock
	for _, step := range credentialLockOrder(home) {
		lk, err := Acquire(step.dir, Options{Stale: step.stale, Timeout: timeout})
		if err != nil {
			// Give back whatever we already took, in reverse order; a
			// partial hold would wedge Claude Code for a full stale window.
			// Join in anything THOSE releases report too: an already-held
			// lock can be compromised while we are still waiting on a later
			// one, and that must not be silently dropped on the failure
			// path either.
			errs := []error{acquireCredentialsError(err)}
			for i := len(locks) - 1; i >= 0; i-- {
				if e := locks[i].Release(); e != nil {
					errs = append(errs, e)
				}
			}
			return nil, errors.Join(errs...)
		}
		locks = append(locks, lk)
	}
	return newHeld(locks, home), nil
}

// acquireCredentialsError describes why one lock in the set could not be
// taken. A contended lock most plausibly means Claude Code is mid-refresh;
// anything else (a permission error, a filesystem error) is not that, and
// saying so would send a caller investigating the wrong problem.
func acquireCredentialsError(err error) error {
	if errors.Is(err, ErrTimeout) {
		return fmt.Errorf("claude code appears to be refreshing credentials: %w", err)
	}
	return fmt.Errorf("acquiring credential lock: %w", err)
}

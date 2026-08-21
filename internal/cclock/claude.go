package cclock

import (
	"fmt"
	"os"
	"path/filepath"
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

// StorageWriteLockDir guards every read-modify-write of .credentials.json.
// cswap does not take this lock; ccdad does.
func StorageWriteLockDir() string {
	return filepath.Join(ccpath.CredentialHome(), ".storage-write.lock")
}

// OAuthRefreshLockDir is Claude Code's primary token-refresh lock.
func OAuthRefreshLockDir() string {
	return filepath.Join(ccpath.CredentialHome(), ".oauth_refresh.lock")
}

// LegacyRefreshLockDir is the older sibling-of-the-directory lock Claude Code
// still takes for compatibility with external tools. Claude Code names it after
// the REAL path of the credential home, so a symlinked home must resolve first
// or the two processes lock different files and exclude nothing.
func LegacyRefreshLockDir() string {
	dir := ccpath.CredentialHome()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return dir + ".lock"
}

// Held is a set of credential locks held together.
type Held struct{ locks []*Lock }

// Release gives the locks back in reverse acquisition order.
func (h *Held) Release() error {
	var firstErr error
	for i := len(h.locks) - 1; i >= 0; i-- {
		if err := h.locks[i].Release(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	h.locks = nil
	return firstErr
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
// timeout is per lock, so the worst case is roughly 3x it.
//
// Never make a network call while these are held. Claude Code's refresh reads,
// round-trips the token endpoint, and saves, all under the two refresh locks;
// holding them across our own network call would stall Claude Code for the
// duration of our request.
func AcquireCredentials(timeout time.Duration) (*Held, error) {
	if err := os.MkdirAll(ccpath.CredentialHome(), 0o700); err != nil {
		return nil, fmt.Errorf("creating credential home: %w", err)
	}

	order := []struct {
		dir   string
		stale time.Duration
	}{
		{OAuthRefreshLockDir(), refreshStale},
		{LegacyRefreshLockDir(), refreshStale},
		{StorageWriteLockDir(), storageWriteStale},
	}

	held := &Held{}
	for _, step := range order {
		lk, err := Acquire(step.dir, Options{Stale: step.stale, Timeout: timeout})
		if err != nil {
			// Give back whatever we already took; a partial hold would wedge
			// Claude Code for a full stale window.
			_ = held.Release()
			return nil, fmt.Errorf("claude code appears to be refreshing credentials: %w", err)
		}
		held.locks = append(held.locks, lk)
	}
	return held, nil
}

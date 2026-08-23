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
	refreshStale = RefreshStale
)

// GlobalConfigStale is the staleness window for the ~/.claude.json lock.
//
// Claude Code takes that one through proper-lockfile WITHOUT passing a stale
// option (`nv(configPath, {lockfilePath: configPath + ".lock", ...})`), so the
// window is proper-lockfile's own default: `{stale:1e4}` followed by
// `Math.max(stale, 2000)`. Ten seconds, and it is the shortest of the four
// windows ccdad honours -- which is exactly why it must not be re-spelled as
// one of the others.
const GlobalConfigStale = 10 * time.Second

// RefreshStale is Claude Code's staleness window for the OAuth refresh locks.
//
// It is exported because ccdad has a second caller that takes ONE of these
// locks on its own — internal/tokens, refreshing the live login's credential —
// and a caller that re-spells the window is a caller that can drift from it.
// Never pass a shorter value: a live holder whose toucher stalled gets its lock
// stolen out from under it, and the holder here can be Claude Code itself.
const RefreshStale = 60 * time.Second

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
	// The EvalSymlinks error is deliberately dropped rather than surfaced. It
	// fails when the home does not exist yet — the ordinary first-run state,
	// since this name is computed before anything creates the directory — and
	// Claude Code's own realpath call degrades the same way. Falling back to the
	// unresolved path is what it does too, so the two still agree on the lock
	// name. A hard failure here would make a first run unable to lock at all.
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	return home + ".lock"
}

func storageWriteLockDir(home string) string {
	return filepath.Join(home, ".storage-write.lock")
}

// globalConfigLockDir is the mutex directory for Claude Code's global config.
//
// Claude Code passes proper-lockfile an explicit `lockfilePath` of
// `${configPath}.lock`, so the name is the config file's own path with .lock
// appended -- NOT a directory beside it and not a name derived from the config
// home. It is computed from an already-resolved config path for the same reason
// the credential lock names are: the caller resolves once and passes it in.
func globalConfigLockDir(configPath string) string {
	return configPath + ".lock"
}

// StorageWriteLockDir guards every read-modify-write of .credentials.json.
// cswap does not take this lock; ccdad does.
func StorageWriteLockDir() (string, error) {
	home, err := ccpath.CredentialHome()
	if err != nil {
		return "", err
	}
	return storageWriteLockDir(home), nil
}

// OAuthRefreshLockDir is Claude Code's primary token-refresh lock.
func OAuthRefreshLockDir() (string, error) {
	home, err := ccpath.CredentialHome()
	if err != nil {
		return "", err
	}
	return oauthRefreshLockDir(home), nil
}

// LegacyRefreshLockDir is the older sibling-of-the-directory lock Claude Code
// still takes for compatibility with external tools. Claude Code names it after
// the REAL path of the credential home, so a symlinked home must resolve first
// or the two processes lock different files and exclude nothing.
func LegacyRefreshLockDir() (string, error) {
	home, err := ccpath.CredentialHome()
	if err != nil {
		return "", err
	}
	return legacyRefreshLockDir(home), nil
}

// GlobalConfigLockDir is the lock guarding ~/.claude.json.
func GlobalConfigLockDir() (string, error) {
	path, err := ccpath.GlobalConfigPath()
	if err != nil {
		return "", err
	}
	return globalConfigLockDir(path), nil
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

// Held is a set of Claude Code locks held together.
type Held struct {
	locks []*Lock
	scope string

	compromised     chan struct{}
	compromisedOnce sync.Once
	watchStop       chan struct{}
	watchDone       chan struct{}
	releaseOnce     sync.Once
	releaseErr      error
}

// newHeld wraps already-acquired locks and starts the goroutine that fans
// their individual Compromised channels into Held's aggregate one. scope is
// the already-resolved path these locks cover, resolved once by the caller;
// see Held.Scope.
func newHeld(locks []*Lock, scope string) *Held {
	h := &Held{
		locks:       locks,
		scope:       scope,
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

// Scope is the already-resolved path this Held's locks actually cover: the
// credential home for AcquireCredentials, the global config FILE for
// AcquireGlobalConfig. It is the value resolved at the moment of acquisition,
// never a fresh re-resolution.
//
// A caller about to write under this Held must derive the target from THIS
// value rather than calling ccpath again: the environment variables ccpath
// reads are exactly the ones ccdad itself mutates for per-account credential
// scoping, so a change between resolutions would lock one path and write
// another.
func (h *Held) Scope() string {
	if h == nil {
		return ""
	}
	return h.scope
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
	home, err := ccpath.CredentialHome()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("creating credential home: %w", err)
	}
	return acquireAll(credentialLockOrder(home), home, timeout)
}

// AcquireGlobalConfig takes the single lock guarding ~/.claude.json.
//
// It is deliberately NOT folded into AcquireCredentials. The two files have
// different locks, different stale windows and different writers, and a caller
// that needs both must take the credential locks first and this one second --
// which is the order Claude Code itself uses, because its credential save runs
// under the refresh locks and only then reaches the config writer. Taking this
// one first would put a ccdad holding it in front of a Claude Code holding the
// refresh locks and waiting for it.
//
// The parent directory is created but the config FILE is not. Claude Code's own
// acquisition passes proper-lockfile realpath:true against the config path, so
// on a machine where ~/.claude.json does not exist yet its lock attempt fails
// ENOENT and it falls back to an unlocked write. ccdad does not copy that
// fallback: our lock directory is a mkdir of a path that needs no existing
// file, so we can lock a not-yet-created config correctly.
func AcquireGlobalConfig(timeout time.Duration) (*Held, error) {
	path, err := ccpath.GlobalConfigPath()
	if err != nil {
		return nil, err
	}
	return AcquireGlobalConfigAt(path, timeout)
}

// AcquireGlobalConfigAt is AcquireGlobalConfig against a named config file.
//
// Nothing about the lock changes with the path, and that is worth stating
// because it looks like it should: the lock's NAME is `${configPath}.lock`, so
// locking a `ccdad run --full-profile` profile's own config locks exactly the
// file the Claude Code inside that profile would lock, with the same stale
// window. The profile is not a file only one process touches -- Claude Code
// rewrites its config constantly, on every startup counter and every project
// entry -- so the lock means there what it means anywhere.
func AcquireGlobalConfigAt(path string, timeout time.Duration) (*Held, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating the directory for %s: %w", filepath.Base(path), err)
	}
	steps := []lockStep{{globalConfigLockDir(path), GlobalConfigStale}}
	return acquireAll(steps, path, timeout)
}

// acquireAll takes every step in order and gives back what it already holds if
// any one of them fails.
func acquireAll(steps []lockStep, scope string, timeout time.Duration) (*Held, error) {
	var locks []*Lock
	for _, step := range steps {
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
	return newHeld(locks, scope), nil
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

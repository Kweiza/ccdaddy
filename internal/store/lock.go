package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// The store's cross-process lock.
//
// Every Open -> mutate -> Save cycle is a read-modify-write that spans the
// atomic rename rather than being covered by it: two processes that each read
// accounts.toml, add an account and write it back lose one of the two
// accounts, because the second rename replaces a document the first one's
// reader never saw. That was harmless while only interactive commands wrote,
// and stops being harmless the moment the daemon calls SetActive on every
// switch.
//
// WHY flock AND NOT cclock. ccdad already ships a working cross-process mutex
// in internal/cclock, so this is a choice rather than a default:
//
//   - cclock exists to INTEROPERATE. It is proper-lockfile's mkdir protocol,
//     mtime toucher and staleness stealing included, because Claude Code holds
//     the same locks from Node and the two have to agree. Nothing outside ccdad
//     ever opens accounts.toml, so there is no protocol to agree with here.
//   - A mkdir lock needs a staleness heuristic to survive a crash, and stealing
//     on stale is actively wrong for a lock a live daemon legitimately holds
//     across a tick: the steal is decided from an mtime, and the daemon that
//     gets stolen from goes on writing. flock has no stale class at all — the
//     kernel drops the lock when the holder's last descriptor closes, which
//     includes the process dying — so there is nothing to tune and nothing to
//     get wrong.
//   - It is not a new dependency. The daemon singleton already takes
//     gofrs/flock on ccdad.lock in this same directory.
//
// The two mechanisms do not exclude each other, so a file guarded by one must
// never also be guarded by the other. They are split by FILE, and each file has
// exactly one owner: accounts.toml is this lock's, usage.json is cclock's (see
// usage.WithCache), and Claude Code's .credentials.json is cclock's because
// Claude Code is the other party on it.
//
// The lock file itself is never written and never read — it is zero bytes
// forever, and only ever try-locked — for the same reason daemon's is, and it
// is NEVER unlinked: flock is per-inode, so delete-and-recreate lets two
// processes each hold "the" lock on a different inode.
//
// LOCK ORDERING, in one direction, everywhere: the store lock is the OUTER
// lock. A caller may take Claude Code's three credential locks (cclink.Activate
// via cclock.AcquireCredentials) while holding it; nothing may take the store
// lock while holding a credential lock.
//
// No caller nests them today — `ccdad switch` calls Activate, which takes and
// releases the credential locks, and only then calls SetActive — and the rule
// is written down because that is exactly the kind of sequence an extraction
// tidies into a single held region. Two callers that pick opposite orders
// deadlock against each other, and the failure needs a daemon and a CLI command
// racing to reproduce.
//
// This is deliberately NOT shared with internal/daemon's near-identical
// primitive. Each package owns its own lock file, and coupling store to daemon
// in either direction to save twenty lines would put the account database
// behind the background process's build graph.
const lockFileName = "store.lock"

// LockTimeout bounds how long a caller waits for the store lock.
//
// The longest legitimate hold is a switch, which takes Claude Code's three
// credential locks inside this one; cclink.LockTimeout bounds each of those, so
// the worst case there is roughly three times that. This is deliberately
// shorter: a CLI command that cannot get in should say so rather than stall for
// half a minute, and the caller who lost is the one who can retry cheaply.
//
// It is a var so a test can shrink it and reach the timeout path without an
// unbounded contention test.
var LockTimeout = 5 * time.Second

// lockRetryDelay is how often a waiter re-tries. flock offers no "wait with a
// deadline" primitive that is portable across the blocking and non-blocking
// paths, so the wait is a poll.
const lockRetryDelay = 25 * time.Millisecond

var (
	// ErrLockBusy means another process held the store lock for longer than
	// LockTimeout. It is a sentinel so a caller can tell contention — retry
	// later — from a filesystem that cannot lock at all.
	ErrLockBusy = errors.New("another process is holding the ccdad store")
	// ErrLocksUnsupported means this filesystem cannot do locks: ENOLCK on an
	// NFS or CIFS mount with no lock daemon, or a GOOS gofrs/flock has no
	// implementation for.
	//
	// It is a refusal, never a silent proceed. Writing anyway would be exactly
	// the lost-account race this lock exists to close, silently, on the one
	// class of machine where it is most likely to happen — a home directory on
	// a network mount is shared, so it is the case with the most writers.
	ErrLocksUnsupported = errors.New("this filesystem does not support locks")
)

// tryLockMu guards tryLockImpl. The daemon's tick loop reaches store mutators
// from a background goroutine, and swapping a package-level function value out
// from under a concurrent reader is a data race under the Go memory model no
// matter how much time separates the two.
var (
	tryLockMu   sync.RWMutex
	tryLockImpl = defaultTryLock
)

// LockPath is the store lock's path. `ccdad doctor` reports the layout, so the
// name lives in one place rather than in a string literal per reader.
func LockPath() string { return filepath.Join(ccpath.StoreHome(), lockFileName) }

// defaultTryLock takes the store lock at path without blocking.
//
// The three return values are the contract, and the middle one is why: gofrs
// reports CONTENTION as (false, nil) and an I/O failure as (false, err), so a
// gate written as `if err != nil { fail }` reads "another writer holds it" as
// success and proceeds to lose an account. Never (false, nil, nil) for a
// failure; never (true, ...) without having actually taken it.
//
// The lock is exclusive and the file is created if missing — unlike the
// daemon's probe, which must not create ccdad.lock because a missing file is
// its evidence. There is no evidence in store.lock: nothing reads it.
//
// On Windows gofrs locks the single byte at offset 0, which is PAST EOF on a
// zero-byte file. That is legal — LockFileEx explicitly allows locking a range
// beyond the end of a file, and it is the reason a never-written lock file
// works at all. Do not "fix" this by writing a byte into the file.
//
// On success the returned closure is the only thing keeping the *flock.Flock
// reachable, which matters: os.File carries a finalizer that closes the fd,
// flock(2) releases on last close of the open file description, and a *Flock
// that becomes unreachable therefore drops the lock with no error anywhere.
func defaultTryLock(path string) (locked bool, release func() error, err error) {
	fl := flock.New(path)
	ok, err := fl.TryLock()
	if err != nil {
		return false, nil, err
	}
	if !ok {
		return false, nil, nil
	}
	// A method value, so the closure holds the *Flock as long as the caller
	// holds the closure.
	return true, fl.Unlock, nil
}

func tryLock(path string) (bool, func() error, error) {
	tryLockMu.RLock()
	impl := tryLockImpl
	tryLockMu.RUnlock()
	return impl(path)
}

// setTryLockForTest replaces the primitive tryLock calls and returns a function
// restoring the previous one. It exists so a test can reach the
// locks-unsupported branch, which is otherwise only reachable on a filesystem
// where locking is broken. Production code must never call it.
func setTryLockForTest(fn func(string) (bool, func() error, error)) (restore func()) {
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

// classifyLockError turns a lock failure into the error the caller has to act
// on, leaving everything else as itself.
//
// Both verbs are %w: classifying must not consume the cause, because `ccdad
// doctor` wants to print the errno it actually got.
func classifyLockError(err error) error {
	if errors.Is(err, errors.ErrUnsupported) || locksUnsupported(err) {
		return fmt.Errorf("%w: %w", ErrLocksUnsupported, err)
	}
	return err
}

// acquireLock takes the store lock, waiting up to LockTimeout.
//
// The wait is bounded and then it gives up, rather than blocking forever: the
// daemon holds this lock across a switch, and a `ccdad list --refresh` that
// blocked indefinitely behind it would look like a hang.
func acquireLock(path string) (release func() error, err error) {
	deadline := time.Now().Add(LockTimeout)
	for {
		locked, release, err := tryLock(path)
		if err != nil {
			return nil, classifyLockError(err)
		}
		if locked {
			return release, nil
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%w: %s did not become free within %s. "+
				"The ccdad daemon holds it while it switches accounts; try again, or stop it with 'ccdad daemon stop'",
				ErrLockBusy, path, LockTimeout)
		}
		time.Sleep(lockRetryDelay)
	}
}

// mutate runs fn under the store lock, against state re-read inside the lock,
// and writes the result exactly once.
//
// Re-reading is half the point. A Store opened before the lock was granted
// holds whatever accounts.toml said then, and the process that held the lock in
// between is precisely the one whose write would be lost — so the in-memory
// copy is refreshed after the lock is granted and before fn touches it. Every
// mutator is keyed by uuid rather than by index for this reason: a caller that
// resolved `2` from a listing still names the same account after the reload,
// which a positional handle would not.
//
// A NESTED call — a mutator invoked from inside a WithStore callback — runs in
// the caller's transaction: it neither re-acquires nor saves. Re-acquiring
// would DEADLOCK, not merely be wasteful: flock is per open file description,
// so a second acquisition from the same process blocks against the first
// exactly as another process's would. Not saving is what makes a multi-step
// callback land as one write.
func (s *Store) mutate(fn func() error) (err error) {
	if s.inTx {
		return fn()
	}
	release, err := acquireLock(filepath.Join(s.root, lockFileName))
	if err != nil {
		return err
	}
	// The release error is part of the answer. A lock that could not be given
	// back stays held for the life of the process and locks out every other
	// writer, so reporting success for a write whose lock leaked would hide the
	// cause of every failure after it.
	defer func() { err = errors.Join(err, release()) }()

	if err := s.load(); err != nil {
		return err
	}
	s.inTx = true
	defer func() { s.inTx = false }()

	if err := fn(); err != nil {
		return err
	}
	return s.save()
}

// WithStore runs fn against the store under the cross-process lock and writes
// what it changed, once.
//
// This is how a caller makes SEVERAL changes land together. The individual
// mutators each take the lock themselves, so a caller changing one thing does
// not need this; a caller importing five accounts does, or an alias collision
// on the fourth leaves three written and two not, with no rollback.
//
// fn returning an error leaves accounts.toml exactly as it was. It does NOT
// undo anything fn did outside the file — Add writes a credential file before
// touching memory — so a failed callback can leave a credential file with no
// account naming it. That is the same direction Add already fails in on its
// own, and it is the safe one: an orphan credential file is inert, while an
// account with no credentials is a switch that logs the user out.
func WithStore(fn func(*Store) error) error {
	s, err := Open()
	if err != nil {
		return err
	}
	return s.mutate(func() error { return fn(s) })
}

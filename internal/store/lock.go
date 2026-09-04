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
func LockPath() (string, error) {
	root, err := ccpath.StoreHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, lockFileName), nil
}

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
// daemon holds this lock across a switch, and a `ccdad status --refresh` that
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
//
// A FAILURE — from fn or from the save — is reversed rather than merely not
// written. Not writing accounts.toml restores the document, and the document
// is not the whole store: Add puts a credential file down before it touches
// memory, so the credentials directory has to be walked back by hand. That is
// Store.rollback, which also states the one change it deliberately leaves.
//
// The reversal runs from LEAVING this function rather than from the two error
// returns that used to carry it, and that is not tidiness. A return is one way
// out of a transaction and never was the only one: a panic in fn unwinds past
// an error return and left the credential file on disk, and so would any early
// return a later edit adds without noticing that it owes rollback a call. The
// journal now empties itself the same way it is cleared, which is the property
// the old shape had to be remembered to keep.
//
// A SIGNAL is the way out that a defer cannot reach on its own, and it is the
// one users actually hit — Ctrl-C partway through a multi-account `ccdad
// import` — so the transaction holds SIGINT for its own span. interrupt.go
// carries the whole argument: why the span is this small, why SIGTERM and
// SIGHUP are deliberately left, and why root.go's refusal to trap SIGINT
// process-wide is not contradicted by this. Three things about it are this
// function's own, and they are visible in the order the defers are registered
// rather than in any comment:
//
//   - The watch is taken BEFORE the load, so no part of the transaction runs
//     with the signal at its default disposition.
//   - It is closed AFTER the reversal, because a process killed halfway through
//     rollback leaves exactly the half-reversed credentials directory the
//     reversal exists to prevent. Defers run last-registered-first, so the
//     watch's close is registered before the reversal's defer and therefore
//     runs after it.
//   - The signal is checked between fn and the save, never after a save that
//     succeeded. Once the document is written the transaction is committed and
//     consistent, and reversing it then would delete credential files the
//     document now names.
//
// What cannot be closed at any price is still open: SIGKILL and a power cut
// take the process out between any two instructions. doctor's credential-files
// row reports the residue, and says so in its own comment.
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

	committed, reported := false, false
	w := watchInterrupt()
	// The backstop for the one rule interrupt.go states: a signal whose default
	// disposition this function removed is always reported, never dropped. The
	// two ordinary outcomes report themselves below and set reported; this
	// covers the path where the transaction was already failing for its own
	// reason, so that a run the user stopped never exits saying only "I/O
	// error" about a write they cancelled.
	defer func() {
		if w.close() && !reported {
			err = errors.Join(err, interruptedErr(committed))
		}
	}()

	if err := s.load(); err != nil {
		return err
	}
	s.inTx = true
	// The journal is cleared on the way OUT rather than on the way in: every
	// path that sets inTx registers this defer with it, so a transaction never
	// starts holding the previous one's entries and there is no second, dead
	// assignment here pretending to guard against it.
	//
	// And the reversal is here rather than at the returns, so that it also runs
	// for a panic and for whatever exit the next edit invents. Assigning to err
	// during a panic changes nothing — the panic carries on past it — but the
	// credential file still comes back off the disk, which is the half that
	// matters.
	defer func() {
		s.inTx = false
		if !committed {
			err = errors.Join(err, s.rollback())
		}
		s.undo = nil
	}()

	if err := fn(); err != nil {
		return err
	}
	if w.seen() {
		reported = true
		return interruptedErr(false)
	}
	if err := s.save(); err != nil {
		return err
	}
	committed = true
	// Closed here rather than left to the defer because its answer is the
	// return value: a signal that landed inside the save is on the far side of
	// the commit, and the user has to be told the change stands.
	if w.close() {
		reported = true
		return interruptedErr(true)
	}
	return nil
}

// WithStore runs fn against the store under the cross-process lock and writes
// what it changed, once.
//
// This is how a caller makes SEVERAL changes land together. The individual
// mutators each take the lock themselves, so a caller changing one thing does
// not need this; a caller importing five accounts does, or an alias collision
// on the fourth leaves three written and two not, with no rollback.
//
// fn returning an error leaves accounts.toml exactly as it was, and now the
// credentials directory with it: mutate replays what the transaction wrote
// there, backwards, through Store.rollback. A batch that fails on the fourth
// of five accounts — an I/O failure on that credential write, which is what
// remains once a caller has judged its document up front — leaves neither
// three accounts in the file nor three credential files beside it. The second
// was the worse half: a live refresh token that `ccdad status`, `ccdad remove`
// and `ccdad doctor` could not see, because all three read the file.
//
// One change is deliberately NOT reversed: the content of a credential file
// the transaction only overwrote. rollback says why, and the cost is real —
// an account that was already stored can come out of a refused transaction
// holding credentials it did not have going in.
//
// A batch STOPPED rather than refused is reversed the same way. Ctrl-C partway
// through `ccdad import` used to be the ordinary way to leave a live refresh
// token nothing on the machine could find; mutate holds SIGINT for the span of
// the write, and the error it returns wraps ErrInterrupted so the command exits
// 130 rather than reporting an I/O failure for something the user cancelled.
func WithStore(fn func(*Store) error) error {
	s, err := Open()
	if err != nil {
		return err
	}
	return s.mutate(func() error { return fn(s) })
}

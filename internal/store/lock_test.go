package store

import (
	"errors"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestConcurrentAddsLoseNoAccount is the defect this lock exists to close.
//
// Each goroutine runs the whole Open -> mutate -> Save cycle, which is the
// shape a second `ccdad add` process has. Without the lock the two reads both
// see an empty store and the second rename replaces the first one's document,
// so one account is simply gone. Separate Store values are essential: sharing
// one would test a mutex this type does not claim to have.
func TestConcurrentAddsLoseNoAccount(t *testing.T) {
	withStore(t)

	const writers = 8
	var wg sync.WaitGroup
	errs := make([]error, writers)
	start := make(chan struct{})
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Open()
			if err != nil {
				errs[i] = err
				return
			}
			<-start
			errs[i] = s.Add(Account{UUID: uuidFor(i), Email: "a@example.com"}, sampleCreds("AT"))
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: Add() = %v, want nil", i, err)
		}
	}

	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(s.Accounts()); got != writers {
		t.Fatalf("after %d concurrent Adds the store holds %d accounts, want %d", writers, got, writers)
	}
	for i := range writers {
		if _, ok := s.Get(uuidFor(i)); !ok {
			t.Errorf("account %q was lost", uuidFor(i))
		}
	}
}

func uuidFor(i int) string { return "u-" + string(rune('a'+i)) }

// TestMutateRereadsInsideTheLock pins the other half of the fix. Holding the
// lock is not enough on its own: a writer that waited for the lock and then
// wrote the copy it read BEFORE waiting still destroys whatever the previous
// holder wrote. The stale Store below is opened first and written last.
func TestMutateRereadsInsideTheLock(t *testing.T) {
	withStore(t)

	stale, err := Open()
	if err != nil {
		t.Fatal(err)
	}

	fresh, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.Add(Account{UUID: "u-first", Email: "first@example.com"}, sampleCreds("AT")); err != nil {
		t.Fatal(err)
	}

	if err := stale.Add(Account{UUID: "u-second", Email: "second@example.com"}, sampleCreds("AT")); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Get("u-first"); !ok {
		t.Error("the account written between Open and Add was lost; mutate did not re-read inside the lock")
	}
	if _, ok := reopened.Get("u-second"); !ok {
		t.Error("u-second was not written")
	}
}

// TestLockBusyGivesUpAfterTimeout pins the bounded wait. A CLI command behind a
// daemon that is holding the lock has to fail with something a caller can act
// on rather than blocking forever.
func TestLockBusyGivesUpAfterTimeout(t *testing.T) {
	root := withStore(t)

	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}

	// Another process's hold, standing in as a lock this one did not take
	// through mutate.
	release, err := acquireLock(filepath.Join(root, lockFileName))
	if err != nil {
		t.Fatal(err)
	}

	restore := setTryLockForTest(func(string) (bool, func() error, error) {
		// Contention is (false, nil, nil) — never an error. A gate that only
		// checks err would read this as "took it" and write anyway.
		return false, nil, nil
	})
	defer restore()

	saved := LockTimeout
	LockTimeout = 60 * time.Millisecond
	defer func() { LockTimeout = saved }()

	started := time.Now()
	err = s.Add(Account{UUID: "u-1", Email: "a@example.com"}, sampleCreds("AT"))
	elapsed := time.Since(started)

	if !errors.Is(err, ErrLockBusy) {
		t.Fatalf("Add() under contention = %v, want ErrLockBusy", err)
	}
	if elapsed < LockTimeout {
		t.Errorf("gave up after %s, want at least the %s timeout", elapsed, LockTimeout)
	}
	if !strings.Contains(err.Error(), "daemon") {
		t.Errorf("the contention error does not name the daemon: %q", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}

// TestLocksUnsupportedIsRefusedNotIgnored covers ENOLCK — an NFS or CIFS mount
// with no lock daemon. Proceeding unguarded there would reintroduce the lost
// account silently, on the class of machine most likely to have two writers.

// unlockableFilesystem is what a filesystem that cannot lock at all answers
// with on this platform. ENOLCK is the unix one -- an NFS or CIFS mount with
// no lock daemon -- and locksUnsupported recognises it only there; off unix
// that file has no errno cases and the condition arrives as
// errors.ErrUnsupported instead. Injecting the wrong one asserts that
// classifyLockError does something it was never asked to do.
var unlockableFilesystem = func() error {
	if runtime.GOOS == "windows" {
		return errors.ErrUnsupported
	}
	return syscall.ENOLCK
}()

func TestLocksUnsupportedIsRefusedNotIgnored(t *testing.T) {
	withStore(t)

	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}

	restore := setTryLockForTest(func(string) (bool, func() error, error) {
		return false, nil, &os.PathError{Op: "flock", Path: "store.lock", Err: unlockableFilesystem}
	})
	defer restore()

	err = s.Add(Account{UUID: "u-1", Email: "a@example.com"}, sampleCreds("AT"))
	if !errors.Is(err, ErrLocksUnsupported) {
		t.Fatalf("Add() on a filesystem without locks = %v, want ErrLocksUnsupported", err)
	}
	// Classifying must not consume the cause: doctor prints the errno.
	if !errors.Is(err, unlockableFilesystem) {
		t.Errorf("the %v cause was consumed by classification: %v", unlockableFilesystem, err)
	}
	if _, statErr := os.Stat(filepath.Join(s.root, accountsFile)); !os.IsNotExist(statErr) {
		t.Error("accounts.toml was written despite the lock being unavailable")
	}
}

// TestUnsupportedPlatformIsRefused is the same refusal for a GOOS gofrs/flock
// has no implementation for, which reports errors.ErrUnsupported rather than an
// errno. syscall does not define ENOLCK everywhere, so the two cases cannot be
// one check.
func TestUnsupportedPlatformIsRefused(t *testing.T) {
	withStore(t)

	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	restore := setTryLockForTest(func(string) (bool, func() error, error) {
		return false, nil, errors.ErrUnsupported
	})
	defer restore()

	if err := s.SetActive("u-1"); !errors.Is(err, ErrLocksUnsupported) {
		t.Fatalf("SetActive() on an unsupported platform = %v, want ErrLocksUnsupported", err)
	}
}

// TestWithStoreIsOneWrite pins the transaction. Import needs several changes to
// land together, and a nested mutator must not re-acquire — flock is per open
// file description, so a second acquisition from this same process would block
// against the first exactly as another process's would, and this test would
// hang rather than fail.
func TestWithStoreIsOneWrite(t *testing.T) {
	root := withStore(t)

	saved := LockTimeout
	LockTimeout = 250 * time.Millisecond
	defer func() { LockTimeout = saved }()

	err := WithStore(func(s *Store) error {
		if err := s.Add(Account{UUID: "u-1", Email: "a@example.com"}, sampleCreds("AT")); err != nil {
			return err
		}
		if err := s.Add(Account{UUID: "u-2", Email: "b@example.com"}, sampleCreds("AT")); err != nil {
			return err
		}
		if err := s.SetAlias("u-2", "work"); err != nil {
			return err
		}
		// Nothing is on disk yet: the transaction saves once, at the end.
		if _, statErr := os.Stat(filepath.Join(root, accountsFile)); !os.IsNotExist(statErr) {
			t.Error("accounts.toml exists mid-transaction; the callback's steps are not landing as one write")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithStore() = %v, want nil", err)
	}

	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Accounts()) != 2 {
		t.Fatalf("Accounts() = %v, want two", s.Accounts())
	}
	got, ok := s.Get("u-2")
	if !ok || got.Alias != "work" {
		t.Fatalf("Get(u-2) = %+v, %v; want the alias set", got, ok)
	}
}

// TestWithStoreLeavesTheFileAloneOnError: a callback that fails halfway must
// not persist half of itself.
func TestWithStoreLeavesTheFileAloneOnError(t *testing.T) {
	withStore(t)

	seed, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Add(Account{UUID: "u-1", Email: "a@example.com"}, sampleCreds("AT")); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("boom")
	err = WithStore(func(s *Store) error {
		if err := s.Add(Account{UUID: "u-2", Email: "b@example.com"}, sampleCreds("AT")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithStore() = %v, want the callback's error", err)
	}

	reopened, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Get("u-2"); ok {
		t.Error("a failed transaction persisted its partial work")
	}
	if _, ok := reopened.Get("u-1"); !ok {
		t.Error("a failed transaction destroyed what was already there")
	}
}

// TestLockFileIsNeverWrittenOrUnlinked. Both properties are load-bearing: flock
// is per-inode, so delete-and-recreate lets two processes each hold "the" lock
// on a different inode, and the zero-byte file is what proves the lock is only
// ever try-locked rather than read for content.
func TestLockFileIsNeverWrittenOrUnlinked(t *testing.T) {
	root := withStore(t)

	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Account{UUID: "u-1", Email: "a@example.com", Kind: identity.KindSubscription}, sampleCreds("AT")); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("u-1"); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(root, lockFileName))
	if err != nil {
		t.Fatalf("the lock file is gone after a mutation: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("the lock file holds %d bytes; it must stay empty and be try-locked only", info.Size())
	}
}

// TestAccountsFileIsNotItselfLocked. Locking the data file is the trap: Save
// renames a new inode over the old one, so the descriptor holding the lock
// refers to a file nobody will ever see again — and on Windows, where locks are
// mandatory, it would make `ccdad list` fail with ERROR_LOCK_VIOLATION while
// the daemon writes. The evidence is that the lock lives on its own path.
func TestAccountsFileIsNotItselfLocked(t *testing.T) {
	root := withStore(t)
	if mustPath(LockPath()) != filepath.Join(root, lockFileName) {
		t.Fatalf("mustPath(LockPath()) = %q, want %q", mustPath(LockPath()), filepath.Join(root, lockFileName))
	}
	if lockFileName == accountsFile {
		t.Fatal("the lock is on accounts.toml itself; an atomic rename would move the file out from under it")
	}
}

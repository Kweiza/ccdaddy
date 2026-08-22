package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// A probe must not manufacture the evidence it was sent to read. gofrs/flock
// opens with os.O_CREATE by default, so this is one SetFlag away from being
// wrong, and being wrong is invisible: the probe still answers "not running",
// it just destroys the missing-file evidence for every probe afterwards.
func TestSingletonHeldDoesNotCreateTheLockFile(t *testing.T) {
	isolate(t)
	held, err := SingletonHeld()
	if err != nil || held {
		t.Fatalf("SingletonHeld() = (%v, %v) on an empty store, want (false, nil)", held, err)
	}
	if _, err := os.Stat(LockPath()); !os.IsNotExist(err) {
		t.Errorf("probing created %s — a missing lock file is the only evidence that no daemon has ever started here, and this erases it", LockPath())
	}
}

// The same rule one level up. A probe that creates the store directory
// manufactures a different piece of the same evidence, and it is easy to add by
// accident: the acquire path legitimately needs the MkdirAll, and lifting it
// into a shared prologue would put it here too.
func TestSingletonHeldDoesNotCreateTheStore(t *testing.T) {
	store := filepath.Join(t.TempDir(), "not", "created", "yet")
	t.Setenv("CCDAD_HOME", store)
	if _, err := SingletonHeld(); err != nil {
		t.Fatalf("SingletonHeld: %v", err)
	}
	if _, err := os.Stat(store); !os.IsNotExist(err) {
		t.Errorf("probing created the store at %s", store)
	}
}

// A store directory that does not exist has no daemon in it. That it is
// indistinguishable from a mistyped CCDAD_HOME is deliberate: both are
// *fs.PathError satisfying os.ErrNotExist, and answering "cannot determine" for
// a fresh install would be worse than answering "not running" for a typo.
func TestSingletonHeldSaysNotRunningWhenTheStoreDoesNotExist(t *testing.T) {
	t.Setenv("CCDAD_HOME", filepath.Join(t.TempDir(), "never", "created"))
	held, err := SingletonHeld()
	if err != nil || held {
		t.Fatalf("SingletonHeld() = (%v, %v) with no store at all, want (false, nil)", held, err)
	}
}

func TestAcquireSingletonMakesTheProbeSayRunning(t *testing.T) {
	isolate(t)
	s, err := AcquireSingleton()
	if err != nil {
		t.Fatalf("AcquireSingleton: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	held, err := SingletonHeld()
	if err != nil || !held {
		t.Fatalf("SingletonHeld() = (%v, %v) while the singleton is held, want (true, nil)", held, err)
	}
	info, err := os.Stat(LockPath())
	if err != nil {
		t.Fatalf("the lock file was not created by acquiring: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("the lock file is %d bytes, want 0 — it is locked, never written and never read", info.Size())
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("the lock file is mode %04o, want 0600", perm)
		}
	}
}

func TestReleasingTheSingletonMakesTheProbeSayNotRunningAgain(t *testing.T) {
	isolate(t)
	s, err := AcquireSingleton()
	if err != nil {
		t.Fatalf("AcquireSingleton: %v", err)
	}
	if err := s.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	held, err := SingletonHeld()
	if err != nil || held {
		t.Fatalf("SingletonHeld() = (%v, %v) after release, want (false, nil)", held, err)
	}
	// Never unlinked. flock is per-inode, so a release that removed the file
	// would let two daemons each hold "the" lock on a different inode.
	if _, err := os.Stat(LockPath()); err != nil {
		t.Errorf("Release removed the lock file: %v", err)
	}
}

// The singleton only means anything across processes, so this is the test that
// actually exercises it. A second process takes it and holds it; this one must
// see that and must lose the race for it.
func TestASecondProcessHoldingTheSingletonIsSeenAndLocksUsOut(t *testing.T) {
	store := isolate(t)
	signals := t.TempDir()
	ready := filepath.Join(signals, "ready")
	release := filepath.Join(signals, "release")

	holder := exec.Command(os.Args[0])
	holder.Env = append(os.Environ(),
		roleEnv+"="+roleHolder,
		readyEnv+"="+ready,
		releaseEnv+"="+release,
		"CCDAD_HOME="+store,
	)
	holder.Stderr = os.Stderr
	if err := holder.Start(); err != nil {
		t.Fatalf("starting the holder: %v", err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o600)
		_ = holder.Wait()
	})
	waitFor(t, ready, 10*time.Second)

	held, err := SingletonHeld()
	if err != nil || !held {
		t.Fatalf("SingletonHeld() = (%v, %v) while another process holds it, want (true, nil)", held, err)
	}

	start := time.Now()
	if _, err := AcquireSingleton(); !errors.Is(err, ErrSingletonHeld) {
		t.Fatalf("AcquireSingleton() = %v, want ErrSingletonHeld — a lost race has to be tellable "+
			"from a filesystem that cannot lock, or `auto` cannot refuse and `daemon status` cannot pick an exit code", err)
	}
	// Three attempts a hundred milliseconds apart means two waits before the
	// verdict.
	if elapsed := time.Since(start); elapsed < 2*acquireRetryDelay {
		t.Errorf("AcquireSingleton gave up after %s, want at least %s — a probe momentarily OWNS the lock it reads, "+
			"so a daemon starting alongside one loses a race it should win", elapsed, 2*acquireRetryDelay)
	}
}

// The retry exists for a race that is inherently rare, so it is pinned at the
// seam rather than by trying to schedule one.
func TestAcquireSingletonRetriesExactlyThreeTimes(t *testing.T) {
	isolate(t)
	calls := 0
	restore := setTryLockForTest(func(path string, create bool) (bool, func() error, error) {
		calls++
		if calls < acquireAttempts {
			return false, nil, nil
		}
		return true, func() error { return nil }, nil
	})
	defer restore()

	s, err := AcquireSingleton()
	if err != nil {
		t.Fatalf("AcquireSingleton gave up while the lock was still clearing: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })
	if calls != acquireAttempts {
		t.Errorf("AcquireSingleton tried %d times, want %d", calls, acquireAttempts)
	}
}

func TestAcquireSingletonGivesUpAfterTheLastAttempt(t *testing.T) {
	isolate(t)
	calls := 0
	restore := setTryLockForTest(func(string, bool) (bool, func() error, error) {
		calls++
		return false, nil, nil
	})
	defer restore()

	if _, err := AcquireSingleton(); !errors.Is(err, ErrSingletonHeld) {
		t.Fatalf("AcquireSingleton() = %v, want ErrSingletonHeld", err)
	}
	if calls != acquireAttempts {
		t.Errorf("AcquireSingleton tried %d times, want %d", calls, acquireAttempts)
	}
}

// The whole reason the probe returns two values. A supervisor that reads any of
// these as "not running" respawns forever.
func TestSingletonHeldNeverReportsNotRunningOnAFailure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		err         error
		unsupported bool
	}{
		{name: "the filesystem has no lock daemon", err: syscall.ENOLCK, unsupported: true},
		{name: "the kernel has no such call", err: syscall.ENOSYS, unsupported: true},
		{name: "this GOOS has no implementation", err: errors.ErrUnsupported, unsupported: true},
		{name: "permission denied", err: os.ErrPermission},
		{name: "something else entirely", err: errors.New("disk on fire")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			restore := setTryLockForTest(func(string, bool) (bool, func() error, error) {
				return false, nil, tc.err
			})
			defer restore()

			held, err := SingletonHeld()
			if err == nil {
				t.Fatalf("SingletonHeld() = (%v, nil) for %v — this is the (false, nil) the contract forbids, "+
					"and a supervisor gating on it respawns forever", held, tc.err)
			}
			if held {
				t.Errorf("SingletonHeld() reported held=true alongside an error")
			}
			if got := errors.Is(err, ErrLocksUnsupported); got != tc.unsupported {
				t.Errorf("errors.Is(err, ErrLocksUnsupported) = %v, want %v — `ccdad doctor` tells "+
					"'locks do not work here' from 'something went wrong' on this axis", got, tc.unsupported)
			}
			// The underlying cause must survive the classification.
			if !errors.Is(err, tc.err) {
				t.Errorf("the original error %v was lost: %v", tc.err, err)
			}
		})
	}
}

func TestAcquireSingletonReportsAFilesystemThatCannotLock(t *testing.T) {
	isolate(t)
	restore := setTryLockForTest(func(string, bool) (bool, func() error, error) {
		return false, nil, syscall.ENOLCK
	})
	defer restore()

	_, err := AcquireSingleton()
	if !errors.Is(err, ErrLocksUnsupported) {
		t.Fatalf("AcquireSingleton() = %v, want ErrLocksUnsupported", err)
	}
	if errors.Is(err, ErrSingletonHeld) {
		t.Error("a filesystem that cannot lock was reported as another daemon holding the singleton")
	}
}

// os.File carries a finalizer that closes the descriptor, and flock releases on
// last close of the open file description — so a *flock.Flock that becomes
// unreachable drops the singleton silently, with no error anywhere, under
// nothing more exotic than ordinary allocation pressure. Reproduced on this
// machine in three GC cycles.
func TestTheHeldSingletonSurvivesGarbageCollection(t *testing.T) {
	isolate(t)
	s, err := AcquireSingleton()
	if err != nil {
		t.Fatalf("AcquireSingleton: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	s = nil //nolint:staticcheck // the point is to drop the caller's reference
	for i := 0; i < 5; i++ {
		runtime.GC()
	}
	held, err := SingletonHeld()
	if err != nil {
		t.Fatalf("SingletonHeld: %v", err)
	}
	if !held {
		t.Fatal("the singleton evaporated once the caller dropped its reference — nothing in the " +
			"package is keeping the lock reachable, so two daemons can end up running")
	}
	// Put the reference back so the cleanup can release it.
	heldMu.Lock()
	s = heldSingleton
	heldMu.Unlock()
}

func TestASecondAcquireInTheSameProcessIsRefused(t *testing.T) {
	isolate(t)
	s, err := AcquireSingleton()
	if err != nil {
		t.Fatalf("AcquireSingleton: %v", err)
	}
	t.Cleanup(func() { _ = s.Release() })

	start := time.Now()
	if _, err := AcquireSingleton(); !errors.Is(err, ErrSingletonHeld) {
		t.Fatalf("the second AcquireSingleton() = %v, want ErrSingletonHeld", err)
	}
	// The kernel would refuse this anyway — flock is per open file
	// description, so a second descriptor in the same process contends with
	// the first exactly as another process would. What the in-process guard
	// adds is that it says so IMMEDIATELY, instead of spending the lost-race
	// retry budget waiting for a holder that is us.
	if elapsed := time.Since(start); elapsed >= acquireRetryDelay {
		t.Errorf("the second AcquireSingleton took %s; a process that already holds the singleton "+
			"should be told so at once rather than retrying against itself for %s", elapsed, 2*acquireRetryDelay)
	}
}

// Release must not repeat the underlying unlock. gofrs/flock happens to
// tolerate a second Unlock today, so nothing about the observable behaviour
// says whether the guard is there — but the guard is what makes that
// independent of the lock library, and a release closure that is not
// idempotent would otherwise be called twice on a shutdown path that ran
// twice.
func TestReleaseUnlocksExactlyOnce(t *testing.T) {
	isolate(t)
	unlocks := 0
	restore := setTryLockForTest(func(string, bool) (bool, func() error, error) {
		return true, func() error { unlocks++; return nil }, nil
	})
	defer restore()

	s, err := AcquireSingleton()
	if err != nil {
		t.Fatalf("AcquireSingleton: %v", err)
	}
	if err := s.Release(); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if err := s.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	if unlocks != 1 {
		t.Errorf("the underlying unlock ran %d times across two Release calls, want 1", unlocks)
	}
}

func TestReleaseIsIdempotentAndNilSafe(t *testing.T) {
	isolate(t)
	var absent *Singleton
	if err := absent.Release(); err != nil {
		t.Errorf("(*Singleton)(nil).Release() = %v, want nil", err)
	}
	s, err := AcquireSingleton()
	if err != nil {
		t.Fatalf("AcquireSingleton: %v", err)
	}
	if err := s.Release(); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if err := s.Release(); err != nil {
		t.Errorf("second Release() = %v, want nil — a shutdown path should not have to track whether some earlier path already released", err)
	}
	// And the second release must not have taken the singleton away from a
	// daemon that acquired it in between.
	other, err := AcquireSingleton()
	if err != nil {
		t.Fatalf("re-acquiring after a double release: %v", err)
	}
	_ = other.Release()
}

func TestAProbeThatCannotGiveTheLockBackIsAFailure(t *testing.T) {
	isolate(t)
	restore := setTryLockForTest(func(string, bool) (bool, func() error, error) {
		return true, func() error { return errors.New("unlock refused") }, nil
	})
	defer restore()

	held, err := SingletonHeld()
	if err == nil {
		t.Fatalf("SingletonHeld() = (%v, nil), want an error — a probe that keeps the singleton locks "+
			"out the daemon it just reported was not running", held)
	}
	if held {
		t.Error("SingletonHeld() reported held=true alongside an error")
	}
}

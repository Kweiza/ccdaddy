package cclock

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testOpts() Options {
	return Options{Stale: time.Minute, Timeout: 500 * time.Millisecond, TouchInterval: 20 * time.Millisecond}
}

func TestAcquireCreatesAndReleaseRemoves(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x.lock")

	lk, err := Acquire(dir, testOpts())
	if err != nil {
		t.Fatalf("Acquire() = %v, want nil", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("lock directory missing after Acquire: %v", err)
	}
	if err := lk.Release(); err != nil {
		t.Fatalf("Release() = %v, want nil", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("lock directory still present after Release: %v", err)
	}
}

// The mutex is mkdir atomicity: a second Acquire on a live lock must block and
// then time out, never succeed.
func TestAcquireTimesOutWhenHeld(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x.lock")

	first, err := Acquire(dir, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	start := time.Now()
	_, err = Acquire(dir, testOpts())
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("second Acquire() = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond {
		t.Fatalf("second Acquire returned after %v, want it to wait out the timeout", elapsed)
	}
}

// A lock whose mtime is older than Stale belongs to a dead holder and may be
// taken over. This is the only circumstance in which stealing is allowed.
func TestAcquireStealsStaleLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x.lock")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}

	lk, err := Acquire(dir, Options{Stale: time.Minute, Timeout: time.Second, TouchInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("Acquire() over stale lock = %v, want nil", err)
	}
	if err := lk.Release(); err != nil {
		t.Fatal(err)
	}
}

// A held lock must not be deemed stale by a waiter: the holder advances the
// directory mtime on a timer.
func TestHeldLockIsTouched(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x.lock")

	lk, err := Acquire(dir, Options{Stale: time.Minute, Timeout: time.Second, TouchInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()

	first, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	second, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !second.ModTime().After(first.ModTime()) {
		t.Fatalf("mtime did not advance while held: %v then %v", first.ModTime(), second.ModTime())
	}
}

func TestAcquireCreatesParentDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "deeper", "x.lock")

	lk, err := Acquire(dir, testOpts())
	if err != nil {
		t.Fatalf("Acquire() with missing parent = %v, want nil", err)
	}
	if err := lk.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x.lock")

	lk, err := Acquire(dir, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	if err := lk.Release(); err != nil {
		t.Fatal(err)
	}
	if err := lk.Release(); err != nil {
		t.Fatalf("second Release() = %v, want nil", err)
	}
}

// A holder must notice when a waiter takes over its lock (rmdir + mkdir)
// while its own toucher was stalled. Chtimes on the same path would
// otherwise succeed silently against the new owner's directory, leaving two
// processes each believing they hold the lock.
func TestTouchDetectsTakeover(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x.lock")

	lk, err := Acquire(dir, Options{Stale: time.Minute, Timeout: time.Second, TouchInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a waiter stealing the lock: remove the directory this holder
	// owns and recreate it as if a new holder had just won it. The short
	// sleep guarantees the recreated directory gets a distinguishable mtime:
	// on this filesystem, directory mtimes advance in coarse (multi-
	// millisecond) steps, so an instantaneous remove+recreate can otherwise
	// land on the same mtime tick as the original -- something that cannot
	// happen for a real takeover, which only follows the Stale threshold
	// elapsing (seconds to minutes, far larger than that granularity).
	time.Sleep(50 * time.Millisecond)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	select {
	case <-lk.Compromised():
	case <-time.After(time.Second):
		t.Fatal("Compromised() did not close after the lock directory was taken over")
	}

	if err := lk.Release(); !errors.Is(err, ErrCompromised) {
		t.Fatalf("Release() = %v, want ErrCompromised", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("lock directory missing after a compromised Release(), want it left for the new owner: %v", err)
	}
}

// The ownership check must not misfire on the normal path: a holder that was
// never stolen from stays exclusive across many touches.
func TestTouchNormalPathUnaffected(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x.lock")

	lk, err := Acquire(dir, Options{Stale: time.Minute, Timeout: time.Second, TouchInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(150 * time.Millisecond) // several touch intervals

	select {
	case <-lk.Compromised():
		t.Fatal("Compromised() closed on a lock that was never taken over")
	default:
	}

	if err := lk.Release(); err != nil {
		t.Fatalf("Release() = %v, want nil", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("lock directory still present after Release: %v", err)
	}
}

// TouchInterval must never exceed half of Stale, or a holder's own lock can
// go stale by its own definition between two touches.
func TestAcquireRejectsContradictoryOptions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x.lock")

	_, err := Acquire(dir, Options{Stale: 2 * time.Second, Timeout: time.Second, TouchInterval: 3 * time.Second})
	if err == nil {
		t.Fatal("Acquire() with TouchInterval > Stale/2 = nil, want an error")
	}
}

// Acquire must not orphan the directory it just created: if the baseline
// stat that follows a successful Mkdir cannot be read, nothing is ever
// returned to Release it, so the directory would otherwise sit there
// blocking every other acquirer until Stale elapses.
func TestAcquireRemovesDirectoryWhenBaselineStatFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x.lock")

	t.Cleanup(setStatLockForTest(func(string) (os.FileInfo, error) {
		return nil, errors.New("simulated stat failure")
	}))

	if _, err := Acquire(dir, testOpts()); err == nil {
		t.Fatal("Acquire() with a failing baseline stat = nil error, want an error")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("lock directory still present after a failed baseline stat: %v", err)
	}
}

// A holder that cannot verify what its own Chtimes wrote must not keep
// asserting ownership on unverified state: silently keeping the old in-memory
// mtime would make the next tick's comparison fail against the (genuinely
// changed, just unread) on-disk value and manufacture a false compromise for
// a lock nobody actually stole. This pins the fail-closed choice so a future
// change cannot quietly go back to swallowing the re-stat error.
//
// statLock is swapped out only after the lock is already held, so this
// specifically targets the re-stat that follows a successful Chtimes rather
// than the ownership-check stat that precedes it: the first call after the
// swap is allowed through to the real implementation (it is that first
// tick's ownership check, which must see the mtime Acquire already
// recorded), and only the second call onward -- the post-Chtimes re-stat --
// is made to fail.
func TestReStatFailureAfterTouchMarksLockLost(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x.lock")

	lk, err := Acquire(dir, Options{Stale: time.Minute, Timeout: time.Second, TouchInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lk.Release() })

	var mu sync.Mutex
	calls := 0
	t.Cleanup(setStatLockForTest(func(dir string) (os.FileInfo, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n <= 1 {
			return defaultStatLock(dir)
		}
		return nil, errors.New("simulated stat failure")
	}))

	select {
	case <-lk.Compromised():
	case <-time.After(time.Second):
		t.Fatal("Compromised() did not close after the re-stat started failing")
	}
}

package cclock

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// stealLock simulates a waiter taking a lock over the way a real one does:
// remove the holder's directory and recreate it as the new owner.
//
// It exists because the obvious two lines are wrong on Windows, in two
// independent ways.
//
// The holder's own toucher has a handle open on that directory every
// TouchInterval — os.Chtimes opens with FILE_WRITE_ATTRIBUTES — and
// RemoveDirectory fails with a sharing violation while a handle is open. The
// removal is therefore retried, so a collision is a slower simulation rather
// than a failed one. Retrying the SIMULATION is not retrying the code under
// test: what is being observed is still one takeover.
//
// And takeover is detected by comparing mtimes (lock.go's `owned :=
// info.ModTime().Equal(l.mtime)`), so the recreated directory has to carry an
// mtime the holder cannot already have written. Letting mkdir stamp it with
// `now` is not enough: the system clock granularity is about 15.6 ms on
// Windows while the toucher runs every 20 ms, so a fresh directory routinely
// lands on the same tick as the touch that preceded it, the comparison says
// "still mine", and the takeover goes unnoticed. Observed on windows-latest as
// "Compromised() did not close after the lock directory was taken over".
// Backdating is unambiguous, and it is also what a real takeover looks like: a
// waiter only steals a lock whose mtime is already older than Stale.
func stealLock(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := os.RemoveAll(dir)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("could not remove %s to simulate a takeover: %v", dir, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stolen := time.Now().Add(-time.Second)
	if err := os.Chtimes(dir, stolen, stolen); err != nil {
		t.Fatal(err)
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

	// The sleep lets the toucher run at least once, so this is a takeover of a
	// lock that has been held and refreshed rather than of a brand-new one.
	// stealLock does the rest; see its comment for why the two obvious lines
	// are not enough.
	time.Sleep(50 * time.Millisecond)
	stealLock(t, dir)

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

// A second Release call, after the first found the lock compromised, must
// report the SAME outcome -- not silently claim success. This is the bug
// F4-1 named: storing the outcome in a local variable meant every caller
// after the first (concurrent or sequential) read Go's zero value instead.
func TestReleaseIsIdempotentAfterCompromise(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x.lock")

	lk, err := Acquire(dir, Options{Stale: time.Minute, Timeout: time.Second, TouchInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	// Held and refreshed first, then stolen through stealLock. The bare
	// remove+recreate that stood here is what turned main red on
	// windows-latest: with a 20 ms toucher against a ~15.6 ms clock the
	// recreated directory kept landing on the same mtime tick as the touch
	// before it, so lock.go's `owned` comparison answered "still mine" and no
	// takeover was ever seen. The sleep separates acquire from the takeover;
	// only stealLock's backdating separates it from the LAST TOUCH, which is
	// the gap that decides this.
	time.Sleep(50 * time.Millisecond)
	stealLock(t, dir)

	select {
	case <-lk.Compromised():
	case <-time.After(time.Second):
		t.Fatal("Compromised() did not close after the lock directory was taken over")
	}

	first := lk.Release()
	second := lk.Release()
	if !errors.Is(first, ErrCompromised) {
		t.Fatalf("first Release() = %v, want ErrCompromised", first)
	}
	if second != first {
		t.Fatalf("second Release() = %v, want the identical value as the first (%v)", second, first)
	}
}

// A stale lock directory whose removal keeps failing must not hang Acquire
// forever: F4-3 found that the stale-steal path, and the "released between
// mkdir and stat" path, looped back to the top with no deadline check at
// all. A non-empty directory makes os.Remove fail deterministically and
// portably (ENOTEMPTY), the same way a read-only mount or a sticky-bit
// parent would in production.
func TestAcquireTimesOutWhenStaleRemovalKeepsFailing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x.lock")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stray"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err := Acquire(dir, Options{Stale: time.Minute, Timeout: 300 * time.Millisecond, TouchInterval: 20 * time.Millisecond})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Acquire() over an unremovable stale lock = %v, want ErrTimeout", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Acquire() took %v to time out with a 300ms Timeout, want it bounded", elapsed)
	}
}

// A takeover that happens AFTER the touch goroutine's last check but BEFORE
// Release is called must still be caught: F4-4 found that Release trusted
// l.lost alone, which only the touch goroutine's own ticker sets. A touch
// interval far longer than the test guarantees the ticker never fires, so
// any detection observed here comes from Release's own final synchronous
// check, not from touch. It must also close Compromised(): a caller
// watching that channel separately from the one calling Release must see
// the same event, not silence.
func TestReleaseDetectsTakeoverTouchHasNotYetNoticed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x.lock")

	lk, err := Acquire(dir, Options{Stale: time.Hour, Timeout: time.Second, TouchInterval: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-lk.Compromised():
		t.Fatal("Compromised() closed before any takeover")
	default:
	}

	// stealLock, not a bare remove+recreate: directory mtimes advance in
	// coarse steps, so an instantaneous recreate can carry the very mtime the
	// holder recorded -- which cannot happen for a real takeover, separated
	// from the original by at least Stale. No toucher runs here (the interval
	// is ten minutes), so the sleep the other takeover tests use would guard
	// nothing; the backdating is what makes this unambiguous.
	stealLock(t, dir)

	if err := lk.Release(); !errors.Is(err, ErrCompromised) {
		t.Fatalf("Release() = %v, want ErrCompromised even though touch's own ticker never fired", err)
	}
	select {
	case <-lk.Compromised():
	default:
		t.Fatal("Compromised() did not close even though Release() reported ErrCompromised")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("lock directory missing after a compromised Release(), want it left for the new owner: %v", err)
	}
}

// Exported methods on a nil *Lock must not panic: a caller that skips the
// error check on a failed Acquire can plausibly hold one.
func TestLockNilReceiverIsSafe(t *testing.T) {
	var lk *Lock

	if err := lk.Release(); err != nil {
		t.Fatalf("nil Lock Release() = %v, want nil", err)
	}
	select {
	case <-lk.Compromised():
		t.Fatal("nil Lock Compromised() closed, want a channel that never fires")
	case <-time.After(20 * time.Millisecond):
	}
}

// The staleness rule has two halves and only the positive one was pinned: the
// two stealing tests sit at the extremes (a lock ~20ms old, and one 2 hours
// old), so the threshold itself could be re-based on any shorter duration with
// the suite green. That matters because Claude Code legitimately holds
// .oauth_refresh.lock across a full token round trip — which is why its own
// config sets a 60s stale window — and a shortened threshold makes ccdad steal
// a lock Claude Code is still using.
func TestAcquireDoesNotStealALockYoungerThanStale(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "held.lock")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const stale = 60 * time.Second
	// Well inside the stale window, but old enough that any threshold
	// meaningfully shorter than Stale would steal it.
	aged := time.Now().Add(-(stale - 2*time.Second))
	if err := os.Chtimes(dir, aged, aged); err != nil {
		t.Fatal(err)
	}

	lk, err := Acquire(dir, Options{Stale: stale, Timeout: 300 * time.Millisecond})
	if err == nil {
		_ = lk.Release()
		t.Fatal("Acquire() stole a lock that is younger than Stale")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Acquire() = %v, want ErrTimeout while the holder is still fresh", err)
	}
}

// A stale lock that cannot be removed at all — a read-only parent, a permission
// problem — is retried until Timeout. The timeout is the right outcome, but
// reporting only "held by another process" buries the one sentence that says
// what to do: the holder IS gone, the removal is what failed.
func TestAcquireReportsWhyAStaleLockCouldNotBeRemoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "stuck.lock")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(dir, aged, aged); err != nil {
		t.Fatal(err)
	}
	// Removing a directory needs write permission on its PARENT.
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	// Chmod REPORTS success on Windows and changes nothing but the read-only
	// attribute, so the setup above silently does not hold there and Acquire
	// removes the stale lock and succeeds. Probe rather than test GOOS: the
	// same thing is true of any filesystem that ignores the mode bits.
	if probe := filepath.Join(parent, "probe"); os.Mkdir(probe, 0o700) == nil {
		_ = os.Remove(probe)
		t.Skip("this filesystem ignores the mode bits, so the removal cannot be made to fail")
	}

	_, err := Acquire(dir, Options{Stale: time.Minute, Timeout: 300 * time.Millisecond})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Acquire() = %v, want ErrTimeout", err)
	}
	if !strings.Contains(err.Error(), "could not be removed") {
		t.Fatalf("error = %q, want it to name the removal failure rather than only the timeout", err)
	}
}

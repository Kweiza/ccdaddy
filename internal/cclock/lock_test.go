package cclock

import (
	"errors"
	"os"
	"path/filepath"
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

package cclock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func withCredentialHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	os.Unsetenv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLockPaths(t *testing.T) {
	dir := withCredentialHome(t)

	if got, want := StorageWriteLockDir(), filepath.Join(dir, ".storage-write.lock"); got != want {
		t.Fatalf("StorageWriteLockDir() = %q, want %q", got, want)
	}
	if got, want := OAuthRefreshLockDir(), filepath.Join(dir, ".oauth_refresh.lock"); got != want {
		t.Fatalf("OAuthRefreshLockDir() = %q, want %q", got, want)
	}
	// The legacy lock is a sibling of the directory, named after its REAL path.
	if got, want := LegacyRefreshLockDir(), dir+".lock"; got != want {
		t.Fatalf("LegacyRefreshLockDir() = %q, want %q", got, want)
	}
}

func TestAcquireCredentialsTakesAllThree(t *testing.T) {
	withCredentialHome(t)

	held, err := AcquireCredentials(time.Second)
	if err != nil {
		t.Fatalf("AcquireCredentials() = %v, want nil", err)
	}
	for _, d := range []string{OAuthRefreshLockDir(), LegacyRefreshLockDir(), StorageWriteLockDir()} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("lock %s not held: %v", filepath.Base(d), err)
		}
	}
	if err := held.Release(); err != nil {
		t.Fatalf("Release() = %v, want nil", err)
	}
	for _, d := range []string{OAuthRefreshLockDir(), LegacyRefreshLockDir(), StorageWriteLockDir()} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Fatalf("lock %s still held after Release", filepath.Base(d))
		}
	}
}

// A partial acquisition must not leak the locks it did take.
func TestAcquireCredentialsRollsBackOnContention(t *testing.T) {
	withCredentialHome(t)

	// Occupy the SECOND lock in the order, so the first is taken and must be
	// given back when the second fails.
	blocker, err := Acquire(LegacyRefreshLockDir(), Options{Stale: time.Minute, Timeout: time.Second, TouchInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()

	if _, err := AcquireCredentials(200 * time.Millisecond); err == nil {
		t.Fatal("AcquireCredentials() = nil, want a timeout error")
	}
	if _, err := os.Stat(OAuthRefreshLockDir()); !os.IsNotExist(err) {
		t.Fatal("primary refresh lock leaked after a failed acquisition")
	}
	if _, err := os.Stat(StorageWriteLockDir()); !os.IsNotExist(err) {
		t.Fatal("storage-write lock was taken despite the failure")
	}
}

// The acquisition order and the stale windows are load-bearing but not
// observable from the outcome of a successful or failed acquisition:
// reversing the order, or shortening a stale window, leaves every
// behavioural test in this package green. Pin the exact sequence directly.
func TestCredentialLockOrder(t *testing.T) {
	dir := withCredentialHome(t)

	steps := credentialLockOrder()
	if len(steps) != 3 {
		t.Fatalf("credentialLockOrder() has %d steps, want 3", len(steps))
	}

	// 60s, 60s, 15s are Claude Code's own stale windows, read directly from
	// the 2.1.238 binary. They are not tuning knobs: a shorter one here
	// would let ccdad steal a lock Claude Code still legitimately holds.
	want := []lockStep{
		{filepath.Join(dir, ".oauth_refresh.lock"), 60 * time.Second},
		{dir + ".lock", 60 * time.Second},
		{filepath.Join(dir, ".storage-write.lock"), 15 * time.Second},
	}
	for i, w := range want {
		if steps[i] != w {
			t.Fatalf("step %d = %+v, want %+v", i, steps[i], w)
		}
	}
}

// Held.Compromised() must fire as soon as any ONE of its member locks is
// taken over, not just be discoverable after the fact through Release.
func TestHeldCompromisedFiresOnMemberTakeover(t *testing.T) {
	withCredentialHome(t)

	opts := Options{Stale: time.Minute, Timeout: time.Second, TouchInterval: 20 * time.Millisecond}
	oauth, err := Acquire(OAuthRefreshLockDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := Acquire(LegacyRefreshLockDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := Acquire(StorageWriteLockDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	held := newHeld([]*Lock{oauth, legacy, storage})

	select {
	case <-held.Compromised():
		t.Fatal("Held.Compromised() closed before any member was taken over")
	default:
	}

	// Simulate a taker stealing the legacy lock out from under this holder,
	// the same way TestTouchDetectsTakeover simulates a single Lock's
	// takeover: remove and recreate the directory as a new owner would.
	time.Sleep(50 * time.Millisecond)
	legacyDir := LegacyRefreshLockDir()
	if err := os.RemoveAll(legacyDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}

	select {
	case <-held.Compromised():
	case <-time.After(time.Second):
		t.Fatal("Held.Compromised() did not close after a member lock was taken over")
	}

	if err := held.Release(); !errors.Is(err, ErrCompromised) {
		t.Fatalf("Release() = %v, want an error satisfying errors.Is(err, ErrCompromised)", err)
	}
}

// Release must not let a compromise on one lock be masked by a mundane,
// unrelated failure on another. This pins the exact bug being fixed: a
// "first error found in iteration order" Release would report whichever
// lock happens to fail first in reverse order and silently drop the
// compromise if it belongs to a different lock.
func TestHeldReleaseDoesNotMaskCompromise(t *testing.T) {
	withCredentialHome(t)

	opts := Options{Stale: time.Minute, Timeout: time.Second, TouchInterval: 20 * time.Millisecond}
	oauth, err := Acquire(OAuthRefreshLockDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := Acquire(LegacyRefreshLockDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := Acquire(StorageWriteLockDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	held := newHeld([]*Lock{oauth, legacy, storage})

	// Force a mundane, unrelated failure on the LAST lock in acquisition
	// order (storage-write), which Release visits FIRST since it releases
	// in reverse order. os.Remove requires an empty directory, so leaving a
	// stray file inside makes its own release fail deterministically and
	// portably, without depending on privilege.
	storageDir := StorageWriteLockDir()
	if err := os.WriteFile(filepath.Join(storageDir, "stray"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Simulate a taker stealing the FIRST lock in acquisition order (the
	// primary refresh lock), which Release visits LAST.
	time.Sleep(50 * time.Millisecond)
	oauthDir := OAuthRefreshLockDir()
	if err := os.RemoveAll(oauthDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(oauthDir, 0o700); err != nil {
		t.Fatal(err)
	}
	select {
	case <-oauth.Compromised():
	case <-time.After(time.Second):
		t.Fatal("member lock did not detect its own takeover")
	}

	err = held.Release()
	if !errors.Is(err, ErrCompromised) {
		t.Fatalf("Release() = %v, want an error satisfying errors.Is(err, ErrCompromised) even though the unrelated storage-write release also failed", err)
	}
}

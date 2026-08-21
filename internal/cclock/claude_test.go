package cclock

import (
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

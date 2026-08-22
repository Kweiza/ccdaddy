package cclock

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// withCredentialHome points every credential path at a temp directory.
//
// It sets CLAUDE_SECURESTORAGE_CONFIG_DIR rather than relying on $HOME, because
// ccpath.homeDir goes through os.UserHomeDir — which reads %USERPROFILE% on
// Windows and ignores $HOME entirely. A HOME-only sandbox therefore does not
// sandbox on Windows at all: these tests acquire and mutate real credential
// locks, and .storage-write.lock is left behind in the developer's live
// ~/.claude. The assertion at the end is what makes that failure loud rather
// than silent if the resolution rules ever change again.
func withCredentialHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir reads this one on Windows
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", dir)

	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := filepath.EvalSymlinks(mustPath(ccpath.CredentialHome())); got != resolved {
		t.Fatalf("credential home is %q, not the sandbox %q — these tests would touch the real one", got, resolved)
	}
	return dir
}

func TestLockPaths(t *testing.T) {
	dir := withCredentialHome(t)

	if got, want := mustPath(StorageWriteLockDir()), filepath.Join(dir, ".storage-write.lock"); got != want {
		t.Fatalf("mustPath(StorageWriteLockDir()) = %q, want %q", got, want)
	}
	if got, want := mustPath(OAuthRefreshLockDir()), filepath.Join(dir, ".oauth_refresh.lock"); got != want {
		t.Fatalf("mustPath(OAuthRefreshLockDir()) = %q, want %q", got, want)
	}
	// The legacy lock is a sibling of the directory, named after its REAL path —
	// resolved, which is what Claude Code computes. Asserting the unresolved
	// form passes on Linux only by accident and fails on macOS, where the temp
	// dir sits behind /private.
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := mustPath(LegacyRefreshLockDir()), resolvedDir+".lock"; got != want {
		t.Fatalf("mustPath(LegacyRefreshLockDir()) = %q, want %q", got, want)
	}
}

func TestAcquireCredentialsTakesAllThree(t *testing.T) {
	dir := withCredentialHome(t)

	held, err := AcquireCredentials(time.Second)
	if err != nil {
		t.Fatalf("AcquireCredentials() = %v, want nil", err)
	}
	if got := held.Scope(); got != dir {
		t.Fatalf("held.Scope() = %q, want %q", got, dir)
	}
	for _, d := range []string{mustPath(OAuthRefreshLockDir()), mustPath(LegacyRefreshLockDir()), mustPath(StorageWriteLockDir())} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("lock %s not held: %v", filepath.Base(d), err)
		}
	}
	if err := held.Release(); err != nil {
		t.Fatalf("Release() = %v, want nil", err)
	}
	for _, d := range []string{mustPath(OAuthRefreshLockDir()), mustPath(LegacyRefreshLockDir()), mustPath(StorageWriteLockDir())} {
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
	blocker, err := Acquire(mustPath(LegacyRefreshLockDir()), Options{Stale: time.Minute, Timeout: time.Second, TouchInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()

	_, err = AcquireCredentials(200 * time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("AcquireCredentials() = %v, want an error satisfying errors.Is(err, ErrTimeout)", err)
	}
	if _, err := os.Stat(mustPath(OAuthRefreshLockDir())); !os.IsNotExist(err) {
		t.Fatal("primary refresh lock leaked after a failed acquisition")
	}
	if _, err := os.Stat(mustPath(StorageWriteLockDir())); !os.IsNotExist(err) {
		t.Fatal("storage-write lock was taken despite the failure")
	}
}

// The acquisition order and the stale windows are load-bearing but not
// observable from the outcome of a successful or failed acquisition:
// reversing the order, or shortening a stale window, leaves every
// behavioural test in this package green. Pin the exact sequence directly.
func TestCredentialLockOrder(t *testing.T) {
	// A literal, non-existent path -- not withCredentialHome's real
	// directory -- deliberately, so this pins credentialLockOrder's output
	// against the home it is GIVEN rather than depending on env-var
	// resolution or filesystem state. (A non-existent home just makes
	// legacyRefreshLockDir's EvalSymlinks fail and fall back to the
	// unresolved path, which is exactly what "want" below expects.)
	const home = "/nonexistent/credential/home/.claude"

	steps := credentialLockOrder(home)
	if len(steps) != 3 {
		t.Fatalf("credentialLockOrder() has %d steps, want 3", len(steps))
	}

	// 60s, 60s, 15s are Claude Code's own stale windows, read directly from
	// the 2.1.238 binary. They are not tuning knobs: a shorter one here
	// would let ccdad steal a lock Claude Code still legitimately holds.
	want := []lockStep{
		{filepath.Join(home, ".oauth_refresh.lock"), 60 * time.Second},
		{home + ".lock", 60 * time.Second},
		{filepath.Join(home, ".storage-write.lock"), 15 * time.Second},
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
	dir := withCredentialHome(t)

	opts := Options{Stale: time.Minute, Timeout: time.Second, TouchInterval: 20 * time.Millisecond}
	oauth, err := Acquire(mustPath(OAuthRefreshLockDir()), opts)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := Acquire(mustPath(LegacyRefreshLockDir()), opts)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := Acquire(mustPath(StorageWriteLockDir()), opts)
	if err != nil {
		t.Fatal(err)
	}
	held := newHeld([]*Lock{oauth, legacy, storage}, dir)

	select {
	case <-held.Compromised():
		t.Fatal("Held.Compromised() closed before any member was taken over")
	default:
	}

	// Simulate a taker stealing the legacy lock out from under this holder,
	// the same way TestTouchDetectsTakeover simulates a single Lock's
	// takeover: remove and recreate the directory as a new owner would.
	time.Sleep(50 * time.Millisecond)
	legacyDir := mustPath(LegacyRefreshLockDir())
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
	dir := withCredentialHome(t)

	opts := Options{Stale: time.Minute, Timeout: time.Second, TouchInterval: 20 * time.Millisecond}
	oauth, err := Acquire(mustPath(OAuthRefreshLockDir()), opts)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := Acquire(mustPath(LegacyRefreshLockDir()), opts)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := Acquire(mustPath(StorageWriteLockDir()), opts)
	if err != nil {
		t.Fatal(err)
	}
	held := newHeld([]*Lock{oauth, legacy, storage}, dir)

	// Force a mundane, unrelated failure on the LAST lock in acquisition
	// order (storage-write), which Release visits FIRST since it releases
	// in reverse order. os.Remove requires an empty directory, so leaving a
	// stray file inside makes its own release fail deterministically and
	// portably, without depending on privilege.
	storageDir := mustPath(StorageWriteLockDir())
	if err := os.WriteFile(filepath.Join(storageDir, "stray"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Simulate a taker stealing the FIRST lock in acquisition order (the
	// primary refresh lock), which Release visits LAST.
	time.Sleep(50 * time.Millisecond)
	oauthDir := mustPath(OAuthRefreshLockDir())
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

// F4-11: releaseOnce is the only thing making a second Release call safe
// rather than a double-close panic on watchStop. Unpin it and this test
// starts panicking instead of failing cleanly.
func TestHeldReleaseIsIdempotent(t *testing.T) {
	dir := withCredentialHome(t)

	opts := Options{Stale: time.Minute, Timeout: time.Second, TouchInterval: 20 * time.Millisecond}
	oauth, err := Acquire(mustPath(OAuthRefreshLockDir()), opts)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := Acquire(mustPath(LegacyRefreshLockDir()), opts)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := Acquire(mustPath(StorageWriteLockDir()), opts)
	if err != nil {
		t.Fatal(err)
	}
	held := newHeld([]*Lock{oauth, legacy, storage}, dir)

	// Give the first Release() call something non-nil to return, so a
	// second call silently reporting nil (F4-1's exact bug) is visible
	// rather than trivially "nil equals nil".
	time.Sleep(50 * time.Millisecond)
	legacyDir := mustPath(LegacyRefreshLockDir())
	if err := os.RemoveAll(legacyDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	select {
	case <-held.Compromised():
	case <-time.After(time.Second):
		t.Fatal("member lock did not detect its own takeover")
	}

	first := held.Release()
	second := held.Release()
	if !errors.Is(first, ErrCompromised) {
		t.Fatalf("first Release() = %v, want an error satisfying errors.Is(err, ErrCompromised)", first)
	}
	if second != first {
		t.Fatalf("second Release() = %v, want the identical value as the first (%v); a second call must not silently report success", second, first)
	}
}

// sync.Once is the only thing that makes a concurrent second Release call
// safe. Run under -race: without the guard this is a data race on
// h.releaseErr and a double-close panic on watchStop, not just a logic bug.
func TestHeldReleaseIsSafeForConcurrentCalls(t *testing.T) {
	withCredentialHome(t)

	held, err := AcquireCredentials(time.Second)
	if err != nil {
		t.Fatal(err)
	}

	var first, second error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); first = held.Release() }()
	go func() { defer wg.Done(); second = held.Release() }()
	wg.Wait()

	if first != nil {
		t.Fatalf("first concurrent Release() = %v, want nil", first)
	}
	if second != nil {
		t.Fatalf("second concurrent Release() = %v, want nil", second)
	}
}

// If a lock AcquireCredentials already holds is compromised while it is
// still waiting on a later one, the failure path must not discard that: it
// must still surface as errors.Is(err, ErrCompromised), the same masking
// concern the successful-Held Release fixes. A TouchInterval far longer than
// the test guarantees the compromised lock's own touch goroutine cannot
// have noticed it either, so any detection is Lock.Release's own final
// synchronous check running on the rollback path.
func TestAcquireCredentialsRollbackSurfacesCompromise(t *testing.T) {
	withCredentialHome(t)

	// Occupy the SECOND lock so AcquireCredentials blocks there after taking
	// the first, giving the goroutine below time to steal the first.
	blocker, err := Acquire(mustPath(LegacyRefreshLockDir()), Options{Stale: time.Minute, Timeout: 2 * time.Second, TouchInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()

	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(150 * time.Millisecond)
		oauthDir := mustPath(OAuthRefreshLockDir())
		_ = os.RemoveAll(oauthDir)
		_ = os.Mkdir(oauthDir, 0o700)
	}()

	_, err = AcquireCredentials(700 * time.Millisecond)
	<-done
	if !errors.Is(err, ErrCompromised) {
		t.Fatalf("AcquireCredentials() = %v, want an error satisfying errors.Is(err, ErrCompromised)", err)
	}
}

// Exported methods on a nil *Held must not panic: a caller that ignores the
// error from a failed AcquireCredentials can plausibly hold one.
func TestHeldNilReceiverIsSafe(t *testing.T) {
	var held *Held

	if err := held.Release(); err != nil {
		t.Fatalf("nil Held Release() = %v, want nil", err)
	}
	if got := held.Scope(); got != "" {
		t.Fatalf("nil Held CredentialHome() = %q, want empty", got)
	}
	select {
	case <-held.Compromised():
		t.Fatal("nil Held Compromised() closed, want a channel that never fires")
	case <-time.After(20 * time.Millisecond):
	}
}

// The legacy lock name is derived from the RESOLVED credential home, so a home
// reached through a symlink must produce the same lock as the real path — that
// is what makes ccdad and Claude Code contend on one lock rather than two.
// Without a symlinked fixture, deleting the EvalSymlinks call leaves the suite
// green because the two forms coincide.
func TestLegacyRefreshLockDirResolvesSymlinks(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real", ".claude")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "real"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	viaLink := filepath.Join(link, ".claude")

	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", viaLink)
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}

	got := mustPath(LegacyRefreshLockDir())
	if got != resolved+".lock" {
		t.Fatalf("mustPath(LegacyRefreshLockDir()) = %q, want the resolved %q", got, resolved+".lock")
	}
	if got == viaLink+".lock" {
		t.Fatal("the lock name was built from the unresolved path, so ccdad and Claude Code would take different locks")
	}
}

// clearHomeAndConfigDirs removes every variable the credential and config paths
// resolve from, which is the only way to make ccpath report a failure.
func clearHomeAndConfigDirs(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", "")
	} else {
		t.Setenv("HOME", "")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	os.Unsetenv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
}

// Both acquisitions must refuse an unresolvable home, and the assertion that
// carries the weight is the second one: the working directory must be left
// untouched.
//
// The two fail differently if the check is dropped, which is why this is not
// one assertion. AcquireCredentials would call os.MkdirAll("") and get ENOENT,
// so it errors either way and only its DIAGNOSIS changes — "creating credential
// home" sends a user looking at permissions on a directory that was never
// named. AcquireGlobalConfig has no such accident to fall back on:
// filepath.Dir("") is ".", MkdirAll(".") succeeds, and it would take a lock
// called ".lock" in whatever directory ccdad was run from — silently, and
// excluding nothing, since Claude Code locks the real one.
func TestAcquireRefusesWhenTheHomeCannotBeResolved(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	clearHomeAndConfigDirs(t)

	if held, err := AcquireCredentials(time.Second); err == nil {
		_ = held.Release()
		t.Error("AcquireCredentials() = nil; want a refusal when the credential home cannot be resolved")
	} else if strings.Contains(err.Error(), "creating credential home") {
		t.Errorf("AcquireCredentials() = %q; want ccpath's diagnosis, not a mkdir failure on the empty path", err)
	}

	if held, err := AcquireGlobalConfig(time.Second); err == nil {
		_ = held.Release()
		t.Error("AcquireGlobalConfig() = nil; want a refusal when the config path cannot be resolved")
	}

	entries, err := os.ReadDir(work)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("a refused acquisition created %q in the working directory", e.Name())
	}
}

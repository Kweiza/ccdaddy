package cclink

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclock"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// withClaudeHome sandboxes Claude Code's credential home inside t.TempDir()
// and returns it. It scopes via CLAUDE_SECURESTORAGE_CONFIG_DIR rather than
// HOME alone: on Windows, ccpath.CredentialHome() resolves the home
// directory through %USERPROFILE%, not $HOME, so HOME-only scoping would
// leave ccpath pointing at the developer's REAL .claude directory, and this
// is the first test file in the package whose tests actually WRITE the
// credentials file -- overwriting it would log the developer out of Claude
// Code. CLAUDE_SECURESTORAGE_CONFIG_DIR works identically on every platform.
//
// It asserts its own sandboxing rather than trusting the reasoning above: if
// ccpath's env-var precedence ever changes, this fails loudly instead of
// quietly starting to touch a real home directory again.
func withClaudeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	creds := filepath.Join(home, ".claude")
	if err := os.MkdirAll(creds, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", creds)
	t.Setenv("HOME", home)        // still consulted by ccpath.ConfigHome, unrelated to this package
	t.Setenv("USERPROFILE", home) // the same variable, on Windows
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	// Sandbox the KEYCHAIN too, and for the same reason the file is sandboxed.
	// writeMerged now installs into the keychain item after writing the file,
	// and this package's tests run on the developer's own macOS -- where that
	// item is their live Claude Code login. A test that sandboxed the file and
	// left the item alone would write its fixture credentials straight into it.
	// "linux" is the off switch: runSecurity refuses to spawn anywhere but
	// darwin, so no `security` runs at all. A test that WANTS the spawn calls
	// fakeSecurity.install after this, which sets darwin back and points the
	// exec layer at the fixture.
	savedGOOS := keychainGOOS
	t.Cleanup(func() { keychainGOOS = savedGOOS })
	keychainGOOS = "linux"

	if got := mustPath(ccpath.CredentialHome()); got != creds {
		t.Fatalf("withClaudeHome: mustPath(ccpath.CredentialHome()) = %q, want %q -- refusing to run with unsandboxed credentials", got, creds)
	}
	return creds
}

// writeCreds writes body to the sandboxed credentials file. Callers must
// call withClaudeHome first: its self-assertion is what makes this safe.
func writeCreds(t *testing.T, body string) string {
	t.Helper()
	path := mustPath(ccpath.CredentialsPath())
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// readCreds reads back the sandboxed credentials file as decoded JSON.
func readCreds(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(mustPath(ccpath.CredentialsPath()))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	return got
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	withClaudeHome(t)

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v, want nil for a missing file", err)
	}
	if len(got) != 0 {
		t.Fatalf("Load() = %v, want an empty blob", got)
	}
}

func TestLoadReadsAllTopLevelKeys(t *testing.T) {
	withClaudeHome(t)
	writeCreds(t, `{"claudeAiOauth":{"accessToken":"a"},"mcpOAuth":{"s|1":{}},"futureKey":7}`)

	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"claudeAiOauth", "mcpOAuth", "futureKey"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("Load() dropped key %q", k)
		}
	}
}

func TestLoadRejectsOversizeFile(t *testing.T) {
	withClaudeHome(t)
	big := make([]byte, maxCredentialsSize+1)
	for i := range big {
		big[i] = ' '
	}
	big[0], big[len(big)-1] = '{', '}'
	writeCreds(t, string(big))

	if _, err := Load(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Load() = %v, want an error satisfying errors.Is(err, ErrTooLarge)", err)
	}
}

func TestLoadRefusesSymlink(t *testing.T) {
	dir := withClaudeHome(t)
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, ccpath.CredentialsFile)); err != nil {
		// Probed rather than skipped by OS name. Creating a symlink on Windows
		// needs Developer Mode or elevation, but store_windows.go's Lstat
		// refusal is the ONLY code that produces ErrSymlink there and this is
		// the only test in the tree that asserts it — so an unconditional skip
		// leaves that branch exercised on no platform at all. Where the
		// capability is present, including a runner with Developer Mode on,
		// the test now runs.
		t.Skipf("cannot create a symlink here: %v", err)
	}

	if _, err := Load(); !errors.Is(err, ErrSymlink) {
		t.Fatalf("Load() = %v, want an error satisfying errors.Is(err, ErrSymlink)", err)
	}
}

func TestActivateSwapsAccountAndPreservesMachineKeys(t *testing.T) {
	withClaudeHome(t)
	path := writeCreds(t, `{"claudeAiOauth":{"accessToken":"old"},"mcpOAuth":{"sentry|1":{"accessToken":"keep"}}}`)

	incoming := Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"new"}`)}
	if err := Activate(incoming); err != nil {
		t.Fatalf("Activate() = %v, want nil", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	// Activate encodes with json.MarshalIndent, so a byte-for-byte comparison
	// against compact JSON would fail on whitespace alone -- compare
	// semantically instead of coupling the test to the encoder's formatting.
	var acct struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(got["claudeAiOauth"], &acct); err != nil || acct.AccessToken != "new" {
		t.Fatalf("claudeAiOauth = %s, want accessToken \"new\" (err %v)", got["claudeAiOauth"], err)
	}
	if _, ok := got["mcpOAuth"]; !ok {
		t.Fatal("Activate() destroyed mcpOAuth")
	}
}

func TestActivateReleasesEveryLock(t *testing.T) {
	withClaudeHome(t)
	writeCreds(t, `{"claudeAiOauth":{"accessToken":"old"}}`)

	if err := Activate(Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"new"}`)}); err != nil {
		t.Fatal(err)
	}
	// Assert against cclock's own exported lock-path functions rather than
	// hand-building paths from dir: the legacy lock is named after
	// filepath.EvalSymlinks(CredentialHome()), which differs from a
	// naively-joined path on any platform where the temp dir sits behind a
	// symlink (e.g. macOS's /var -> /private/var), and a hand-built path
	// there would report "not found" no matter what Activate actually did.
	for _, d := range []string{mustPath(cclock.OAuthRefreshLockDir()), mustPath(cclock.LegacyRefreshLockDir()), mustPath(cclock.StorageWriteLockDir())} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Fatalf("%s was left behind after Activate", filepath.Base(d))
		}
	}
}

func TestActivateWritesModeSixHundred(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod is a no-op on Windows beyond the read-only bit")
	}
	withClaudeHome(t)
	path := writeCreds(t, `{"claudeAiOauth":{"accessToken":"old"}}`)

	if err := Activate(Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"new"}`)}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
}

// This is the test that catches two mutations that otherwise leave the whole
// suite green: deleting the locking entirely, and moving the re-read above
// the lock. It fails on the first mutation at the initial select (Activate
// never blocks), and on the second at the final assertion (Activate wrote
// from a pre-lock read instead of the write that landed while it waited).
func TestActivateWaitsForCredentialLocksAndRereadsUnderThem(t *testing.T) {
	withClaudeHome(t)
	writeCreds(t, `{"claudeAiOauth":{"accessToken":"old"},"mcpOAuth":{"early|1":{}}}`)

	held, err := cclock.AcquireCredentials(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- Activate(Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"new"}`)})
	}()

	select {
	case err := <-done:
		t.Fatalf("Activate() returned %v while the credential locks were held; it never took them", err)
	case <-time.After(300 * time.Millisecond):
	}

	// Only a read performed INSIDE the lock can observe this.
	writeCreds(t, `{"claudeAiOauth":{"accessToken":"old"},"mcpOAuth":{"late|1":{}}}`)
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Activate: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Activate did not return after the locks were released")
	}

	got := readCreds(t)
	if !bytes.Contains(got["mcpOAuth"], []byte("late|1")) {
		t.Fatalf("mcpOAuth = %s; Activate wrote from a pre-lock read", got["mcpOAuth"])
	}
}

// Activate must surface a lock takeover through its own return value rather
// than swallowing it -- the exact bug an unnamed result and a discarded
// deferred local produce. This steals the FIRST lock Activate acquires
// (oauth_refresh) while Activate is blocked waiting on the SECOND (legacy,
// held here), the way another process's takeover would look on disk: rmdir
// the directory and mkdir a fresh one. Activate's own Lock still remembers
// the OLD mtime, so this is invisible to it until Release re-stats -- which
// happens synchronously in Release, not on the touch goroutine's 3s tick, so
// this does not need to wait one out.
//
// The write must still go through: with a same-directory atomic rename there
// is no torn state to roll back, and rolling back on a post-write compromise
// risks clobbering a write Claude Code made after ours.
func TestActivateSurfacesCompromiseThroughRelease(t *testing.T) {
	withClaudeHome(t)
	writeCreds(t, `{"claudeAiOauth":{"accessToken":"old"}}`)

	blocker, err := cclock.Acquire(mustPath(cclock.LegacyRefreshLockDir()), cclock.Options{Stale: time.Minute, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- Activate(Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"new"}`)})
	}()

	select {
	case err := <-done:
		t.Fatalf("Activate() returned %v before the legacy lock was released; it never blocked", err)
	case <-time.After(300 * time.Millisecond):
	}

	oauthDir := mustPath(cclock.OAuthRefreshLockDir())
	if err := os.RemoveAll(oauthDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(oauthDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := blocker.Release(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, cclock.ErrCompromised) {
			t.Fatalf("Activate() = %v, want an error satisfying errors.Is(err, cclock.ErrCompromised)", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Activate did not return after the legacy lock was released")
	}

	got := readCreds(t)
	var acct struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(got["claudeAiOauth"], &acct); err != nil || acct.AccessToken != "new" {
		t.Fatalf("claudeAiOauth = %s, want the write to have gone through despite the compromise (no rollback on a post-write compromise)", got["claudeAiOauth"])
	}
}

func TestActivateTimesOutWhenLockIsHeld(t *testing.T) {
	withClaudeHome(t)
	path := writeCreds(t, `{"claudeAiOauth":{"accessToken":"old"}}`)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	orig := LockTimeout
	LockTimeout = 200 * time.Millisecond
	t.Cleanup(func() { LockTimeout = orig })

	blocker, err := cclock.Acquire(mustPath(cclock.OAuthRefreshLockDir()), cclock.Options{Stale: time.Minute, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()

	err = Activate(Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"new"}`)})
	if !errors.Is(err, cclock.ErrTimeout) {
		t.Fatalf("Activate() = %v, want an error satisfying errors.Is(err, cclock.ErrTimeout)", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("Activate modified the credentials file despite timing out acquiring the locks")
	}
}

func TestActivateRefusesSnapshotWithoutOAuth(t *testing.T) {
	withClaudeHome(t)
	path := writeCreds(t, `{"claudeAiOauth":{"accessToken":"old"},"mcpOAuth":{"s|1":{}}}`)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := Activate(Blob{}); err == nil {
		t.Fatal("Activate(Blob{}) = nil, want an error refusing to log the user out")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("Activate modified the credentials file despite refusing an OAuth-less snapshot")
	}
}

// Claude Code writes the credentials file with JSON.stringify(x, null, 2),
// which does not HTML-escape. Go's encoder rewrites '&', '<' and '>' into their
// \\u00xx escapes — inside a RawMessage it is only copying through, too — so a
// machine key holding a URL with a query string would come back byte-different
// from what Claude Code wrote. The values parse the same either way; matching
// the bytes is what keeps a diff of this file meaningful.
func TestActivateDoesNotHTMLEscapeTheCredentialsFile(t *testing.T) {
	withClaudeHome(t)
	// A MACHINE-scoped key, so Merge preserves it across the swap — an
	// account-scoped one would be deleted, which is correct and would test
	// nothing about encoding.
	path := writeCreds(t, `{"claudeAiOauth":{"accessToken":"old"},`+
		`"mcpOAuth":{"server":{"authorizationUrl":"https://idp.example.com/a?x=1&y=2<z>"}}}`)

	if err := Activate(Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"new"}`)}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, escaped := range []string{`\u0026`, `\u003c`, `\u003e`} {
		if bytes.Contains(raw, []byte(escaped)) {
			t.Fatalf("the credentials file carries the escape %s; Claude Code writes these characters literally:\n%s", escaped, raw)
		}
	}
	if !bytes.Contains(raw, []byte("x=1&y=2<z>")) {
		t.Fatalf("the preserved machine key lost its literal characters:\n%s", raw)
	}
}

// The WRITE — not only the read — has to happen while all three credential
// locks are held.
//
// TestActivateWaitsForCredentialLocksAndRereadsUnderThem already pins the
// re-read, and it is satisfied by an implementation that reads under the locks,
// releases them, and only then renames. That implementation is a plausible
// refactor (release as soon as the merged bytes are in hand, keep the critical
// section short) and it is exactly the bug the locks exist to prevent: Claude
// Code's own double-checked re-read happens under these locks, so a rename
// landing after ccdad let go is a rename Claude Code has already decided cannot
// have happened, and its refreshed token overwrites the freshly switched
// account.
//
// Lock-directory existence alone would be a weak assertion — the directories
// are ordinary mkdirs and could in principle be someone else's. So the hook
// also tries to ACQUIRE the storage-write lock from inside the rename and
// requires that to time out: that is the property a second process would
// actually observe.
func TestActivateWritesWhileTheCredentialLocksAreHeld(t *testing.T) {
	withClaudeHome(t)
	writeCreds(t, `{"claudeAiOauth":{"accessToken":"old"},"mcpOAuth":{"srv":1}}`)

	lockDirs := []string{
		mustPath(cclock.OAuthRefreshLockDir()),
		mustPath(cclock.LegacyRefreshLockDir()),
		mustPath(cclock.StorageWriteLockDir()),
	}

	var missing []string
	var contendErr error
	renamed := false

	orig := renameFile
	renameFile = func(from, to string) error {
		renamed = true
		for _, d := range lockDirs {
			if _, err := os.Stat(d); err != nil {
				missing = append(missing, filepath.Base(d))
			}
		}
		// Timeout 0 is a single attempt, so this cannot outlast the write it is
		// measuring. Stale is a minute because the lock was taken microseconds
		// ago and must not be deemed abandoned; a short window here would steal
		// the very lock the assertion is about.
		lk, err := cclock.Acquire(mustPath(cclock.StorageWriteLockDir()),
			cclock.Options{Stale: time.Minute, Timeout: 0})
		if err == nil {
			_ = lk.Release()
			contendErr = nil
		} else {
			contendErr = err
		}
		return orig(from, to)
	}
	t.Cleanup(func() { renameFile = orig })

	if err := Activate(Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"new"}`)}); err != nil {
		t.Fatalf("Activate() = %v", err)
	}
	if !renamed {
		t.Fatal("Activate never reached the rename, so this test proved nothing about the write")
	}
	if len(missing) > 0 {
		t.Errorf("the credentials file was renamed with %v not held; the write is outside the locks", missing)
	}
	if contendErr == nil {
		t.Error("the storage-write lock could be acquired during the rename; nothing was excluding a second writer")
	} else if !errors.Is(contendErr, cclock.ErrTimeout) {
		t.Errorf("acquiring the storage-write lock during the rename = %v, want cclock.ErrTimeout", contendErr)
	}
}

// ActivateWith exists for the one caller Activate cannot serve: an unattended
// executor whose DECISION -- switch, or stand down -- is only sound against the
// file as it is at the moment of the write. Activate re-reads under the lock to
// build its merge base, but it has already committed to writing; a daemon that
// decided to move away from an account the user has since changed by hand must
// be able to look and abandon.
//
// This is the property, and it is the same shape as the Activate test above:
// only a read taken INSIDE the lock can see the write that landed while the
// call was blocked on it.
func TestActivateWithDecidesFromTheFileAsItIsUnderTheLock(t *testing.T) {
	withClaudeHome(t)
	writeCreds(t, `{"claudeAiOauth":{"accessToken":"early"}}`)

	held, err := cclock.AcquireCredentials(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}

	seen := make(chan Blob, 1)
	done := make(chan error, 1)
	go func() {
		done <- ActivateWith(func(live Blob) (Blob, error) {
			seen <- live
			return Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"new"}`)}, nil
		})
	}()

	select {
	case err := <-done:
		t.Fatalf("ActivateWith() returned %v while the credential locks were held; it never took them", err)
	case <-time.After(300 * time.Millisecond):
	}

	writeCreds(t, `{"claudeAiOauth":{"accessToken":"late"}}`)
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ActivateWith: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("ActivateWith did not return after the locks were released")
	}

	live := <-seen
	if !bytes.Contains(live["claudeAiOauth"], []byte("late")) {
		t.Fatalf("decide saw claudeAiOauth = %s; it was handed a pre-lock read", live["claudeAiOauth"])
	}
}

// ErrNoChange is how the decision stands down. Returning it must leave the file
// untouched -- byte for byte, not merely equivalent -- because "I looked and
// decided not to" and "I rewrote it with the same content" are different facts
// to anything watching the file's mtime, and one of those watchers is Claude
// Code's own change detection.
func TestActivateWithErrNoChangeWritesNothing(t *testing.T) {
	withClaudeHome(t)
	path := writeCreds(t, `{"claudeAiOauth":{"accessToken":"old"}}`)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	err = ActivateWith(func(Blob) (Blob, error) {
		return Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"new"}`)}, ErrNoChange
	})
	if !errors.Is(err, ErrNoChange) {
		t.Fatalf("ActivateWith() = %v, want an error satisfying errors.Is(err, ErrNoChange)", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("the credentials file changed:\nbefore %s\nafter  %s", before, after)
	}
}

// The guard Activate applies to its argument applies to what decide returns,
// for the same reason: Merge deletes every account-scoped key and puts back
// only what the snapshot carries, so a snapshot with no login is a logout
// wearing a switch's clothing.
func TestActivateWithRefusesASnapshotWithNoLogin(t *testing.T) {
	withClaudeHome(t)
	path := writeCreds(t, `{"claudeAiOauth":{"accessToken":"old"}}`)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := ActivateWith(func(Blob) (Blob, error) { return Blob{}, nil }); err == nil {
		t.Fatal("ActivateWith() accepted a snapshot with no claudeAiOauth")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a refused snapshot still rewrote the credentials file")
	}
}

// A decide that fails is not a write. The error reaches the caller unchanged so
// an executor can tell its own refusal from a lock or I/O failure.
func TestActivateWithPropagatesADecideFailureWithoutWriting(t *testing.T) {
	withClaudeHome(t)
	path := writeCreds(t, `{"claudeAiOauth":{"accessToken":"old"}}`)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	boom := errors.New("the store went away")
	if err := ActivateWith(func(Blob) (Blob, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("ActivateWith() = %v, want the decide error", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a failed decision still rewrote the credentials file")
	}
}

// On macOS the keychain item is the store Claude Code consults FIRST, so a
// switch that wrote only the credentials file would leave every request
// authenticating as whoever the item names. An item that is already there is
// installed into as well.
//
// The fixture records only its LAST invocation, so "the install ran" is spelled
// as "the install is what ran last".
func TestActivateInstallsIntoAnExistingKeychainItem(t *testing.T) {
	withClaudeHome(t)
	writeCreds(t, `{"claudeAiOauth":{"accessToken":"old"}}`)
	// exit 0 for every spawn: the lookup finds the item and the install succeeds.
	argv := fakeSecurity{}.install(t)

	if err := Activate(Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"new"}`)}); err != nil {
		t.Fatalf("Activate() = %v, want nil", err)
	}

	got := recordedArgv(t, argv)
	if len(got) == 0 || got[0] != "add-generic-password" {
		t.Fatalf("last spawn = %q, want the keychain install", got)
	}
}

// A machine with no item has nothing shadowing the credentials file. Creating
// one would introduce a second store where there was one, and it would become
// the store Claude Code reads first -- a switch is not the place to decide a
// machine should start using the keychain.
func TestActivateDoesNotCreateAKeychainItemThatWasNotThere(t *testing.T) {
	withClaudeHome(t)
	writeCreds(t, `{"claudeAiOauth":{"accessToken":"old"}}`)
	argv := fakeSecurity{exit: securityNotFoundCode}.install(t)

	if err := Activate(Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"new"}`)}); err != nil {
		t.Fatalf("Activate() = %v, want nil", err)
	}

	got := recordedArgv(t, argv)
	for _, arg := range got {
		if arg == "add-generic-password" {
			t.Fatalf("Activate created an item that was not there: %q", got)
		}
	}
}

// Load answers "what is Claude Code authenticating as", and on macOS that is
// the keychain item whenever there is one: its combinator reads the item first
// and only falls back to the file. Reading the file past an item that exists
// reports a login nothing authenticates with -- which is what `ccdad which`
// did while an hour of work went to the account the item named.
func TestLoadPrefersTheKeychainItemOverTheFile(t *testing.T) {
	withClaudeHome(t)
	writeCreds(t, `{"claudeAiOauth":{"accessToken":"from-the-file"}}`)
	fakeSecurity{stdout: `{"claudeAiOauth":{"accessToken":"from-the-keychain"}}` + "\n"}.install(t)

	live, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	var acct struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(live["claudeAiOauth"], &acct); err != nil {
		t.Fatal(err)
	}
	if acct.AccessToken != "from-the-keychain" {
		t.Fatalf("accessToken = %q, want the keychain's -- Load read past the store Claude Code reads first", acct.AccessToken)
	}
}

// With no item the fallback is what Claude Code reads, so the file is the
// login and Load says so.
func TestLoadFallsBackToTheFileWhenNoItemIsThere(t *testing.T) {
	withClaudeHome(t)
	writeCreds(t, `{"claudeAiOauth":{"accessToken":"from-the-file"}}`)
	fakeSecurity{exit: securityNotFoundCode}.install(t)

	live, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	var acct struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(live["claudeAiOauth"], &acct); err != nil {
		t.Fatal(err)
	}
	if acct.AccessToken != "from-the-file" {
		t.Fatalf("accessToken = %q, want the file's", acct.AccessToken)
	}
}

// ActivateWith's decision is only sound against the store Claude Code reads,
// and on macOS that is the keychain item. The blob handed to decide is what
// switcher.Execute computes AlreadyOn from, so a base taken from the file makes
// a switch decline on exactly the machine that needs one: file and item naming
// different accounts is the shape this whole path exists to repair.
func TestActivateWithDecidesFromTheKeychainWhenAnItemIsThere(t *testing.T) {
	withClaudeHome(t)
	writeCreds(t, `{"claudeAiOauth":{"accessToken":"from-the-file"}}`)
	fakeSecurity{stdout: `{"claudeAiOauth":{"accessToken":"from-the-keychain"}}` + "\n"}.install(t)

	var seen string
	err := ActivateWith(func(live Blob) (Blob, error) {
		var acct struct {
			AccessToken string `json:"accessToken"`
		}
		if err := json.Unmarshal(live["claudeAiOauth"], &acct); err != nil {
			return nil, err
		}
		seen = acct.AccessToken
		return Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"new"}`)}, nil
	})
	if err != nil {
		t.Fatalf("ActivateWith() = %v, want nil", err)
	}
	if seen != "from-the-keychain" {
		t.Fatalf("decide saw %q, want the keychain's -- AlreadyOn is computed from this blob", seen)
	}
}

// The machine-scoped keys come from that same base, so they must come from the
// item too. Claude Code writes the primary and skips the fallback, which is how
// the file falls behind; merging onto the stale one would put back an mcpOAuth
// login the item had already replaced.
func TestActivateKeepsMachineKeysFromTheKeychainNotTheStaleFile(t *testing.T) {
	withClaudeHome(t)
	writeCreds(t, `{"claudeAiOauth":{"accessToken":"old"},"mcpOAuth":{"sentry|1":{"accessToken":"stale"}}}`)
	fakeSecurity{stdout: `{"claudeAiOauth":{"accessToken":"old"},"mcpOAuth":{"sentry|1":{"accessToken":"current"}}}` + "\n"}.install(t)

	if err := Activate(Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"new"}`)}); err != nil {
		t.Fatalf("Activate() = %v, want nil", err)
	}

	got := readCreds(t)
	var mcp map[string]struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(got["mcpOAuth"], &mcp); err != nil {
		t.Fatal(err)
	}
	if mcp["sentry|1"].AccessToken != "current" {
		t.Fatalf("mcpOAuth carried the stale file's value %q, want the item's", mcp["sentry|1"].AccessToken)
	}
}

// The item must hold COMPACT json, and this is not a style preference.
// `security -w` returns HEX for any value containing a newline, and both
// Claude Code's reader and ccdad's own LoadKeychainCredentials parse the
// output as JSON -- so an indented payload round-trips as hex and is
// unreadable to both. The credentials FILE is indented on purpose (it matches
// what Claude Code writes there); the item is the other shape.
//
// Measured before this test existed: writing the file's indented bytes into a
// real item made `security -w` return 5284 hex characters where the item
// Claude Code itself wrote had come back as plain JSON.
func TestActivateWritesCompactJSONIntoTheKeychainItem(t *testing.T) {
	withClaudeHome(t)
	writeCreds(t, `{"claudeAiOauth":{"accessToken":"old"}}`)
	argv := fakeSecurity{}.install(t)

	if err := Activate(Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"new"}`)}); err != nil {
		t.Fatalf("Activate() = %v, want nil", err)
	}

	got := recordedArgv(t, argv)
	if len(got) == 0 || got[0] != "add-generic-password" {
		t.Fatalf("last spawn = %q, want the keychain install", got)
	}
	payload := got[len(got)-1]
	raw, err := hex.DecodeString(payload)
	if err != nil {
		t.Fatalf("the -X payload is not hex: %v", err)
	}
	if bytes.ContainsRune(raw, '\n') {
		t.Fatalf("the item payload carries a newline, so `security -w` will hand it back as hex:\n%s", raw)
	}
	var back Blob
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("the item payload is not valid JSON: %v", err)
	}
}

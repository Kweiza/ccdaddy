package cclink

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
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
	t.Setenv("HOME", home) // still consulted by ccpath.ConfigHome, unrelated to this package
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if got := ccpath.CredentialHome(); got != creds {
		t.Fatalf("withClaudeHome: ccpath.CredentialHome() = %q, want %q -- refusing to run with unsandboxed credentials", got, creds)
	}
	return creds
}

// writeCreds writes body to the sandboxed credentials file. Callers must
// call withClaudeHome first: its self-assertion is what makes this safe.
func writeCreds(t *testing.T, body string) string {
	t.Helper()
	path := ccpath.CredentialsPath()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// readCreds reads back the sandboxed credentials file as decoded JSON.
func readCreds(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(ccpath.CredentialsPath())
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
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need Developer Mode on Windows")
	}
	dir := withClaudeHome(t)
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, ccpath.CredentialsFile)); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(); !errors.Is(err, ErrSymlink) {
		t.Fatalf("Load() = %v, want an error satisfying errors.Is(err, ErrSymlink)", err)
	}
}

func TestCaptureReturnsOnlyAccountScoped(t *testing.T) {
	withClaudeHome(t)
	writeCreds(t, `{"claudeAiOauth":{"accessToken":"a"},"mcpOAuth":{"s|1":{}}}`)

	got, err := Capture()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["claudeAiOauth"]; !ok {
		t.Fatal("Capture() dropped claudeAiOauth")
	}
	if _, ok := got["mcpOAuth"]; ok {
		t.Fatal("Capture() included mcpOAuth; machine-scoped keys must never enter a snapshot")
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
	for _, d := range []string{cclock.OAuthRefreshLockDir(), cclock.LegacyRefreshLockDir(), cclock.StorageWriteLockDir()} {
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

	blocker, err := cclock.Acquire(cclock.LegacyRefreshLockDir(), cclock.Options{Stale: time.Minute, Timeout: time.Second})
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

	oauthDir := cclock.OAuthRefreshLockDir()
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

	blocker, err := cclock.Acquire(cclock.OAuthRefreshLockDir(), cclock.Options{Stale: time.Minute, Timeout: time.Second})
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

// Claude Code's storage-V5 change probe watches dev:ino:size:mtimeNs, so a
// running session detects a swap by inode change even at an identical
// mtime. WriteFileAtomic's sibling-temp-file-then-rename is what produces a
// new inode; an in-place write would be invisible to that probe. This is
// also the test that catches "the atomic write replaced with os.WriteFile":
// the credentials file already exists with mode 0600 before Activate runs,
// so an in-place os.WriteFile would leave both content and mode looking
// correct while silently keeping the old inode.
func TestActivateWritesViaRenameSoTheInodeChanges(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("inode numbers are not meaningful on Windows")
	}
	withClaudeHome(t)
	path := writeCreds(t, `{"claudeAiOauth":{"accessToken":"old"}}`)

	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeIno := before.Sys().(*syscall.Stat_t).Ino

	if err := Activate(Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"new"}`)}); err != nil {
		t.Fatal(err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	afterIno := after.Sys().(*syscall.Stat_t).Ino
	if afterIno == beforeIno {
		t.Fatalf("inode did not change across Activate (stayed %d); an in-place write is invisible to Claude Code's storage-V5 change probe", beforeIno)
	}
}

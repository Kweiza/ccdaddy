package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// isolate points ccdad's store AND every Claude Code path this package can
// reach at temp directories, and returns the fake Claude config home.
//
// Every test that constructs a command calls it, including the ones that should
// not reach the filesystem at all: "should not" is a property of the code under
// test, which is exactly what is in flux while these tests are being made to
// pass. cclink.Activate WRITES ccpath.CredentialsPath(), so without this a
// switch test overwrites the developer's real login.
//
// CLAUDE_SECURESTORAGE_CONFIG_DIR must be set too, and to a non-empty value:
// ccpath.CredentialHome() prefers it over CLAUDE_CONFIG_DIR whenever it is
// DEFINED, and defined-but-empty falls back to the real ~/.claude.
func isolate(t *testing.T) string {
	t.Helper()
	claude := t.TempDir()
	t.Setenv("CCDAD_HOME", filepath.Join(t.TempDir(), "ccdad"))
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", claude)

	// The environment probes are stubbed for the same reason, and it is not
	// hypothetical: a test that leaves browserAvailable real passes or hangs
	// depending on whether the machine running it has a browser, and the hang
	// is a 300-second login deadline with a loopback listener bound. A test
	// that means to describe a machine with a browser calls stubEnvironment.
	stubEnvironment(t, false, false)

	// Auto-start is suppressed for the whole suite, and this is a hard
	// requirement rather than tidiness: nothing else stops a spawn, and an
	// unsuppressed one detaches a REAL daemon pinned to the t.TempDir() above,
	// which the framework then deletes underneath it — leaving a process
	// holding a lock in a directory that no longer exists, on the developer's
	// machine, after `go test` has printed ok. The tests that exercise the
	// policy put the real hook back by name.
	suppressAutoStart(t)

	// The network is isolated for the same reason the filesystem is. A test that
	// reaches api.anthropic.com is not testing ccdad: it depends on the machine
	// being online, on a real token being rejected, and it sends a value the
	// test made up to a live service. Any test that genuinely needs a profile
	// response calls stubProfile to say so out loud.
	stubProfile(t, func(http.ResponseWriter, *http.Request) {
		t.Error("this test reached the profile endpoint; call stubProfile if that is intended")
	})
	return claude
}

// suppressAutoStart replaces the auto-start hook with a no-op. See isolate.
func suppressAutoStart(t *testing.T) {
	t.Helper()
	saved := autoStart
	t.Cleanup(func() { autoStart = saved })
	autoStart = func(*cobra.Command) {}
}

// stubProfile points the profile client at a local server for the duration of
// one test.
func stubProfile(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	restore := profileBaseURL
	t.Cleanup(func() {
		profileBaseURL = restore
		srv.Close()
	})
	profileBaseURL = srv.URL
}

// profileJSON is a minimal well-formed profile response.
func profileJSON(uuid, email string) string {
	return `{"account":{"uuid":"` + uuid + `","email":"` + email + `"},` +
		`"organization":{"uuid":"org-1","organization_type":"claude_max","rate_limit_tier":"default_claude_max_20x","billing_type":"subscription"}}`
}

// runRoot drives the real command tree the way the binary does, so a test sees
// the same exit code a caller would. The fourth return value is what
// ExecuteWith itself printed, which is how a test tells a silent error from one
// that got re-reported on top of the command's own notice.
func runRoot(t *testing.T, args ...string) (code ExitCode, stdout, stderr, top string) {
	t.Helper()
	root := NewRootCmd()
	var out, errOut, topBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	code = ExecuteWith(root, &topBuf)
	return code, out.String(), errOut.String(), topBuf.String()
}

// runCmd drives a single command in isolation, for the cases that must not go
// through the root's flag normalization.
func runCmd(t *testing.T, cmd *cobra.Command, args ...string) (error, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	return cmd.Execute(), out.String(), errOut.String()
}

// stubEnvironment describes a machine the test is not running on. Both probes
// are package vars precisely so this is possible: a real TTY and a real browser
// are not things a test can arrange.
func stubEnvironment(t *testing.T, tty, browser bool) {
	t.Helper()
	saveTTY, saveBrowser := stdinIsTTY, browserAvailable
	t.Cleanup(func() { stdinIsTTY, browserAvailable = saveTTY, saveBrowser })
	stdinIsTTY = func() bool { return tty }
	browserAvailable = func() bool { return browser }
}

// assertNoLiveCredentials fails if anything wrote Claude Code's credentials
// file. Paired with isolate(t), it is what proves a command that must not
// switch really did not.
func assertNoLiveCredentials(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(ccpath.CredentialsPath()); !os.IsNotExist(err) {
		t.Fatalf("the live credentials file exists at %s; this path must not write it", ccpath.CredentialsPath())
	}
}

// credsFor builds a stored credential blob carrying a refresh token, which is
// what attribution anchors on.
func credsFor(refresh string) cclink.Blob {
	return cclink.Blob{"claudeAiOauth": json.RawMessage(
		`{"accessToken":"AT","refreshToken":"` + refresh + `"}`)}
}

func seedAccount(t *testing.T, uuid, email string) {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(store.Account{UUID: uuid, Email: email}, credsFor("RT-"+uuid)); err != nil {
		t.Fatal(err)
	}
}

// seedDisabledAccount stores an account already held out of rotation. It is a
// fresh insert rather than an update because store.Add deliberately preserves
// the stored Disabled flag over an incoming one.
func seedDisabledAccount(t *testing.T, uuid, email string) {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(store.Account{UUID: uuid, Email: email, Disabled: true}, credsFor("RT-"+uuid)); err != nil {
		t.Fatal(err)
	}
}

// seedCreditAccount stores an account metered in money. Kind is set explicitly
// because store.Add takes the caller's Kind and Save serializes it: a credit
// account is the one the engine may never reach without §7.3's two opt-ins.
func seedCreditAccount(t *testing.T, uuid, email string) {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(store.Account{UUID: uuid, Email: email, Kind: identity.KindCredit}, credsFor("RT-"+uuid)); err != nil {
		t.Fatal(err)
	}
}

// addLiveKey adds a top-level key to the live credentials file, standing in for
// something Claude Code earned during use rather than at login.
func addLiveKey(t *testing.T, key, rawValue string) {
	t.Helper()
	raw, err := os.ReadFile(ccpath.CredentialsPath())
	if err != nil {
		t.Fatal(err)
	}
	var live map[string]json.RawMessage
	if err := json.Unmarshal(raw, &live); err != nil {
		t.Fatal(err)
	}
	live[key] = json.RawMessage(rawValue)
	encoded, err := json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ccpath.CredentialsPath(), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeLiveFile replaces Claude Code's credentials file wholesale, standing in
// for something that changed it without ccdad being told — `/login` inside
// Claude Code, or a restore from a backup.
func writeLiveFile(t *testing.T, raw string) {
	t.Helper()
	if err := os.WriteFile(ccpath.CredentialsPath(), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

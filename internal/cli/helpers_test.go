package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"
	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/ccver"
	"github.com/Kweiza/ccdaddy/internal/codexauth"
	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/oauth"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/usage"
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
//
// The colour environment is sandboxed here too, and it is the same class of
// input as the paths: CLICOLOR_FORCE and NO_COLOR are read by colorprofile out
// of the environment colorWriter hands it, so a developer with either exported
// runs a different suite from the one CI runs.
func isolate(t *testing.T) string {
	t.Helper()
	claude := t.TempDir()
	t.Setenv("CCDAD_HOME", filepath.Join(t.TempDir(), "ccdad"))
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", claude)

	// The colour environment, cleared for the same reason the auth environment
	// below is. colorWriter hands colorprofile os.Environ(), and either of
	// these two variables moves the profile: CLICOLOR_FORCE=1 raises a
	// bytes.Buffer to ANSI256 and puts SGR bytes into every table this suite
	// asserts on, NO_COLOR=1 floors a forced one back down. Both directions are
	// a test whose answer depends on whose shell ran it.
	//
	// Empty rather than unset, because there is no t.Unsetenv -- and empty is
	// what colorprofile's strconv.ParseBool already reads as absent. The tests
	// that mean to describe a forced terminal set TTY_FORCE and CLICOLOR_FORCE
	// themselves, by name.
	t.Setenv("CLICOLOR_FORCE", "")
	t.Setenv("NO_COLOR", "")

	// The home directory is sandboxed HERE rather than in the tests that need
	// it, and that placement is the whole protection. `ccdad setup-path` writes
	// shell startup files under it and `ccdad uninstall` deletes blocks out of
	// them, so a single test that constructs a command without a fake home
	// edits the developer's real ~/.bashrc, ~/.profile or ~/.zshrc — including
	// tests whose whole point is that they write nothing, since "writes
	// nothing" is the property in flux while they are being made to pass.
	// BOTH variables: os.UserHomeDir reads $HOME on Unix and %USERPROFILE% on
	// Windows, so setting one sandboxes half the platforms.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("ZDOTDIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	// The environment probes are stubbed for the same reason, and it is not
	// hypothetical: a test that leaves browserAvailable real passes or hangs
	// depending on whether the machine running it has a browser, and the hang
	// is a 300-second login deadline with a loopback listener bound. A test
	// that means to describe a machine with a browser calls stubEnvironment.
	stubEnvironment(t, false, false)

	// Claude Code's auth environment is cleared for the same reason the
	// filesystem is sandboxed. ccdad's attribution reads every one of these to
	// decide which credential a session will use, so a developer who exports
	// ANTHROPIC_API_KEY in their shell would otherwise get answers out of this
	// suite that CI never sees — and the failure would look like a flake in
	// attribution rather than an unsandboxed input.
	for _, v := range identity.AuthEnvironmentVars() {
		t.Setenv(v, "")
	}

	// CCDAD_IMPORT is cleared for the same reason as the block above, and it is
	// ccdad's own. A developer with it exported would have `ccdad bootstrap`
	// read their real export document into a t.TempDir() store — so the test
	// that means to describe an UNSET variable would pass or fail depending on
	// whose machine ran it, which is the shape of an unsandboxed input.
	//
	// Empty rather than unset, because there is no t.Unsetenv and because empty
	// is what bootstrap already treats as "no document". A test that needs the
	// variable genuinely absent calls unsetForTest.
	t.Setenv("CCDAD_IMPORT", "")

	// CLAUDE_PLUGIN_ROOT is Claude Code's, and it is set for exactly one thing:
	// a server the PLUGIN launched. `ccdad doctor` reads it to say which
	// spelling this machine's MCP tools have, so a developer running the suite
	// from inside a plugin-launched shell would get a row CI never sees --
	// which is the same unsandboxed-input shape as the block above, arriving
	// through somebody else's variable. The two tests that care about it set it
	// themselves.
	t.Setenv("CLAUDE_PLUGIN_ROOT", "")

	// THE TWO PATHS NO t.Setenv CAN REACH. Claude Code compiles in
	// /home/claude/.claude/remote/.oauth_token and .api_key as literals: they
	// are absolute, outside the home directory, and read on every machine.
	// Without this the suite's answer depends on whether the box running it
	// happens to have a /home/claude, and that failure reads as a flake rather
	// than as an unsandboxed input.
	//
	// The directory is deliberately NOT created. A test that means to describe
	// an injected token writes the file itself, exactly as the settings tests
	// write a settings.json.
	savedHostToken, savedHostKey := identity.HostOAuthTokenFile, identity.HostAPIKeyFile
	t.Cleanup(func() {
		identity.HostOAuthTokenFile, identity.HostAPIKeyFile = savedHostToken, savedHostKey
	})
	hostRemote := filepath.Join(t.TempDir(), "remote")
	identity.HostOAuthTokenFile = filepath.Join(hostRemote, ".oauth_token")
	identity.HostAPIKeyFile = filepath.Join(hostRemote, ".api_key")

	// The project settings files resolve against the working directory, which
	// under `go test` is this package's source tree. Empty them so the answer
	// does not depend on what the repository happens to contain.
	savedSettings := projectSettingsFiles
	t.Cleanup(func() { projectSettingsFiles = savedSettings })
	projectSettingsFiles = func() []string { return nil }

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

	// The poller `list --refresh` borrows is the same axis as the profile
	// client above, and the more dangerous of the two: it spends an access
	// token against a live, rate-limited endpoint. A test that means to refresh
	// calls stubRefresh by name.
	savedEngine := newEngine
	t.Cleanup(func() { newEngine = savedEngine })
	newEngine = func() *daemon.Engine {
		e := daemon.NewEngine()
		e.AccessToken = func(context.Context, string) (string, error) {
			t.Error("this test asked for an access token; call stubRefresh if that is intended")
			return "", errors.New("the token source is not stubbed")
		}
		e.FetchUsage = func(context.Context, string) (*usage.Snapshot, error) {
			t.Error("this test reached the usage endpoint; call stubRefresh if that is intended")
			return nil, errors.New("the usage client is not stubbed")
		}
		return e
	}

	// The macOS Keychain is the third axis, and the only one that would be
	// invisible from here: on a Linux machine the real probe answers
	// "unsupported" without spawning anything, so a suite that left it alone
	// would look perfectly isolated and would shell out to /usr/bin/security in
	// every doctor test on a developer's Mac. Stubbed to the same answer on
	// every host, so the report does not depend on who ran it; a test that means
	// to describe a machine with a keychain calls stubKeychain by name.
	stubKeychain(t, false, cclink.KeychainItem{}, cclink.ErrKeychainUnsupported)

	// WHICH Claude Code is installed is a fourth axis, and it hid behind the
	// third: the keychain probe was stubbed and the version probe was not, so
	// the suite still reached PATH and read the developer's own install. Not
	// hypothetical -- the first run after doctor's keychain row learned to read
	// a version got 2.1.241 off this machine and printed the post-2.1.113
	// remedy, failing an assertion written about the other branch. On a machine
	// with no claude the same test would have passed, which is the shape of an
	// unsandboxed input that only fails for some developers.
	//
	// The default describes a machine with no claude on it, which is the one
	// answer no host can contradict. A test that means to describe an install
	// calls stubClaudeInstall by name.
	stubClaudeInstall(t, ccver.Install{}, ccver.ErrNoClaudeCode)
	return claude
}

// stubClaudeInstall describes the Claude Code installed on the machine one test
// will see.
//
// Both seams together, deliberately. doctor probes PATH itself and `ccdad run`
// describes the path it already resolved, so a test that set only one of them
// would sandbox one command and leave the other reading the developer's
// machine -- and run's is the one whose branch REFUSES to start.
func stubClaudeInstall(t *testing.T, install ccver.Install, err error) {
	t.Helper()
	savedProbe, savedDescribe := probeClaudeInstall, describeClaudeInstall
	t.Cleanup(func() { probeClaudeInstall, describeClaudeInstall = savedProbe, savedDescribe })
	probeClaudeInstall = func() (ccver.Install, error) { return install, err }
	describeClaudeInstall = func(string) ccver.Install { return install }
}

// claudeVersion is a described install at a version, for the tests whose whole
// subject is which side of 2.1.113 the machine is on.
func claudeVersion(major, minor, patch int) ccver.Install {
	return ccver.Install{
		Launcher: filepath.Join("/usr", "local", "bin", "claude"),
		Method:   ccver.MethodNPM,
		Version:  ccver.Version{Major: major, Minor: minor, Patch: patch},
		Known:    true,
	}
}

// stubKeychain describes the legacy macOS Keychain one doctor run will see.
func stubKeychain(t *testing.T, present bool, item cclink.KeychainItem, err error) {
	t.Helper()
	stubKeychainCandidates(t, present, item, []cclink.KeychainItem{item}, err)
}

// stubKeychainCandidates is the same seam for the case that has more than one
// candidate name -- a decomposed CLAUDE_CONFIG_DIR, where the item ccdad is
// looking for has two spellings and the report has to name both.
func stubKeychainCandidates(t *testing.T, present bool, item cclink.KeychainItem, checked []cclink.KeychainItem, err error) {
	t.Helper()
	saved := keychainProbe
	t.Cleanup(func() { keychainProbe = saved })
	keychainProbe = func(context.Context) (cclink.KeychainLookup, error) {
		return cclink.KeychainLookup{Present: present, Item: item, Checked: checked}, err
	}
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

// explicitArgs is what SetArgs has to be handed. Cobra reads a nil slice as
// "not set" and falls back to os.Args[1:] — the test binary's own command line
// — so a test that passed no arguments would be running whatever `go test` was
// invoked with. An empty non-nil slice is the "no arguments" it meant.
func explicitArgs(args []string) []string { return append([]string{}, args...) }

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
	root.SetArgs(explicitArgs(args))
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
	cmd.SetArgs(explicitArgs(args))
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
	if _, err := os.Stat(mustPath(ccpath.CredentialsPath())); !os.IsNotExist(err) {
		t.Fatalf("the live credentials file exists at %s; this path must not write it", mustPath(ccpath.CredentialsPath()))
	}
}

// liveLoginJSON is the credentials file's claudeAiOauth object as raw JSON, for
// the tests that write the file wholesale rather than through the store.
//
// It exists so those tests cannot spell a scope-less login by hand: see
// credsFor for why user:inference decides whether Claude Code has a login at
// all. extra is spliced in as further top-level keys, which is what the
// unknown-key probes need.
func liveLoginJSON(refresh, extra string) string {
	body := `{"claudeAiOauth":{"accessToken":"AT","refreshToken":"` + refresh +
		`","scopes":["user:inference","user:profile"]}`
	if extra != "" {
		body += "," + extra
	}
	return body + "}"
}

// credsFor builds a stored credential blob carrying a refresh token, which is
// what attribution anchors on.
//
// IT CARRIES user:inference, and that is load-bearing rather than decorative.
// Claude Code takes a login as a credential only when its scopes contain that
// one -- a Console sign-in leaves a well-formed record with an access token and
// without it, and no session ever authenticates with that record. A fixture
// without the scope describes a machine with NO login, which is not the machine
// any test using this means. Every login fixture in this package goes through
// here so the next one cannot be written scope-less.
func credsFor(refresh string) cclink.Blob {
	return cclink.Blob{"claudeAiOauth": json.RawMessage(
		`{"accessToken":"AT","refreshToken":"` + refresh + `","scopes":["user:inference","user:profile"]}`)}
}

func seedAccount(t *testing.T, uuid, email string) {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(store.Account{Provider: provider.Claude, UUID: uuid, Email: email}, credsFor("RT-"+uuid)); err != nil {
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
	if err := s.Add(store.Account{Provider: provider.Claude, UUID: uuid, Email: email, Disabled: true}, credsFor("RT-"+uuid)); err != nil {
		t.Fatal(err)
	}
}

// seedAPIKeyAccount stores an account whose credential Claude Code reads from
// somewhere other than the credentials file. There is no refresh grant behind
// one, so its usage can never be polled.
func seedAPIKeyAccount(t *testing.T, uuid, email string) {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	rec, err := json.Marshal(cclink.TokenRecord{Kind: cclink.APIKeyKind, Token: "sk-ant-api-" + uuid})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(store.Account{Provider: provider.Claude, UUID: uuid, Email: email, Kind: identity.KindAPIKey},
		cclink.Blob{cclink.TokenKey: rec}); err != nil {
		t.Fatal(err)
	}
}

// seedCreditAccount stores an account metered in money. Kind is set explicitly
// because store.Add takes the caller's Kind and Save serializes it: a credit
// account is the one the engine may never reach without the two independent
// opt-ins the credit gate requires.
func seedCreditAccount(t *testing.T, uuid, email string) {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(store.Account{Provider: provider.Claude, UUID: uuid, Email: email, Kind: identity.KindCredit}, credsFor("RT-"+uuid)); err != nil {
		t.Fatal(err)
	}
}

// seedCodexAccount stores an account served through ccdad's Codex proxy. Its
// credential is the one ccdad-owned record and nothing Claude Code has ever
// read, which is the shape every never-cross check is written against.
func seedCodexAccount(t *testing.T, uuid, email string) {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	cred := codexauth.Credential{
		IDToken:      "ID-" + uuid,
		AccessToken:  "AT-" + uuid,
		RefreshToken: "RT-" + uuid,
		AccountID:    "acct-" + uuid,
		UserID:       uuid,
	}
	if err := s.Add(store.Account{
		UUID: uuid, Email: email, Provider: provider.Codex, Kind: identity.KindSubscription,
	}, cred.ToBlob()); err != nil {
		t.Fatal(err)
	}
}

// storeAccounts reads the store's accounts inside an isolated world.
func storeAccounts(t *testing.T) []store.Account {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	return s.Accounts()
}

// seedPrimaryCreditAccount stores a credit-metered seat already marked primary.
// It is a fresh insert rather than an update for the same reason
// seedDisabledAccount is: store.Add deliberately preserves the stored flag over
// an incoming one.
func seedPrimaryCreditAccount(t *testing.T, uuid, email string) {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(store.Account{Provider: provider.Claude,
		UUID: uuid, Email: email, Kind: identity.KindCredit, Primary: true,
	}, credsFor("RT-"+uuid)); err != nil {
		t.Fatal(err)
	}
}

// addLiveKey adds a top-level key to the live credentials file, standing in for
// something Claude Code earned during use rather than at login.
func addLiveKey(t *testing.T, key, rawValue string) {
	t.Helper()
	raw, err := os.ReadFile(mustPath(ccpath.CredentialsPath()))
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
	if err := os.WriteFile(mustPath(ccpath.CredentialsPath()), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeLiveFile replaces Claude Code's credentials file wholesale, standing in
// for something that changed it without ccdad being told — `/login` inside
// Claude Code, or a restore from a backup.
func writeLiveFile(t *testing.T, raw string) {
	t.Helper()
	if err := os.WriteFile(mustPath(ccpath.CredentialsPath()), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A nil args slice is not "no arguments" to cobra: it means "not set", and
// Execute then falls back to os.Args[1:] — which under `go test` is the test
// binary's own command line. Every test that supplies no arguments goes through
// that path, and it works today only because pflag happens to swallow -test.*
// flags. Anything else sitting on that line is parsed as ccdad's arguments.
func TestNoArgumentsDoesNotInheritTheTestBinarysCommandLine(t *testing.T) {
	isolate(t)
	// Both axes stubbed so the expected code is a fact about this helper rather
	// than about the machine: bare `ccdad` with no terminal is a usage error,
	// and `go test` gives the binary a pipe while a developer running the
	// compiled binary by hand gives it a console.
	stubTTYs(t, false, false)
	saved := os.Args
	t.Cleanup(func() { os.Args = saved })
	os.Args = []string{saved[0], "list", "--json"}

	code, out, _, top := runRoot(t)
	if code != ExitUsage {
		t.Fatalf("exit = %d (%s), want %d for a bare ccdad with no terminal", code, top, ExitUsage)
	}
	if strings.Contains(out, "schemaVersion") {
		t.Fatalf("stdout = %q, want nothing on it: the helper ran the test binary's own command line instead of the empty one it was given", out)
	}
}

// runCmd's half of the same fallback, which the test above cannot reach: that
// one drives the root, and every command test in this package that supplies no
// arguments goes through runCmd instead. Reverting only runCmd's line leaves
// the rest of the suite green, so without this the fix is half-pinned.
func TestRunCmdWithNoArgumentsDoesNotInheritTheTestBinarysCommandLine(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, profileJSON("acct-1", "a@example.com"))
	})
	rec := stubLogin(t, loginToken("acct-1", "a@example.com", "RT-1"))
	saved := os.Args
	t.Cleanup(func() { os.Args = saved })
	// A flag `add` really has, so the fallback would change the answer rather
	// than being swallowed the way pflag swallows -test.* today.
	os.Args = []string{saved[0], "--console"}

	if err, _, _ := runCmd(t, newAddCmd()); err != nil {
		t.Fatal(err)
	}
	if got := rec.last(t).Surface; got != oauth.SurfaceClaudeAI {
		t.Fatalf("Surface = %v, want the subscription surface: runCmd ran the test binary's own command line instead of the empty one it was given", got)
	}
}

// The two variables the sandbox forgot, and they are the two that decide
// whether this suite sees escape bytes at all.
//
// colorprofile reads CLICOLOR_FORCE and NO_COLOR out of the environment it is
// handed, and colorWriter hands it os.Environ(). Measured on this tree: with
// CLICOLOR_FORCE=1 exported, colorWriter(&bytes.Buffer{}) resolves to ANSI256
// rather than NoTTY, a styled cell reaches the buffer as
// "* 1  \x1b[32m95%\x1b[m  ok\n", and hover_test.go's row assertions -- which
// split a row with strings.Fields and then index the result -- read a colour
// escape where they expect a number. A developer who exports either variable
// in their shell would get answers out of this suite that CI never sees, which
// is the same unsandboxed-input shape as the auth block in isolate.
//
// color_test.go's TestNoColorStripsEvenWhereColourWouldOtherwiseBeAllowed
// already clears CLICOLOR_FORCE by hand, for exactly this reason and with the
// same t.Setenv-to-empty idiom; that it had to is what says the sandbox was
// missing it. NO_COLOR goes with it rather than being left alone as the
// "harmless" direction: a suite whose colour assertions pass only because a
// developer's NO_COLOR floored them is asserting nothing.
func TestIsolateNeutralisesTheColourForcingVariables(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "1")
	t.Setenv("NO_COLOR", "1")
	isolate(t)

	for _, v := range []string{"CLICOLOR_FORCE", "NO_COLOR"} {
		if got := os.Getenv(v); got != "" {
			t.Errorf("%s = %q after isolate, want empty", v, got)
		}
	}
	w, ok := colorWriter(&bytes.Buffer{}).(*colorprofile.Writer)
	if !ok {
		t.Fatalf("colorWriter did not return a *colorprofile.Writer: %T", colorWriter(&bytes.Buffer{}))
	}
	if w.Profile > colorprofile.NoTTY {
		t.Fatalf("profile into a bytes.Buffer is %v, want NoTTY: the sandbox let a "+
			"forcing variable through and every table assertion in this package "+
			"is now reading escape bytes", w.Profile)
	}
}

// writeFile writes a fixture file, making its directory first. It is for the
// paths outside the sandboxed home that no t.Setenv can move -- see isolate's
// note on the two host files.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// stubLiveSource fixes which store doctor believes answered, without needing a
// keychain on the test machine. The blob itself is whatever seedHealthyMachine
// wrote; this axis is only about the NAME the report gives it.
func stubLiveSource(t *testing.T, src cclink.CredentialSource) {
	t.Helper()
	saved := loadLiveWithSource
	t.Cleanup(func() { loadLiveWithSource = saved })
	loadLiveWithSource = func() (cclink.Blob, cclink.CredentialSource, error) {
		live, err := cclink.Load()
		return live, src, err
	}
}

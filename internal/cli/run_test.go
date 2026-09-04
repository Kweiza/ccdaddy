package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/ccver"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// The command is spelled `run <ACCOUNT> [claude args…]`, so the account is
// mandatory. The check lives in Args rather than in RunE for the reason
// switch.go's validators give: cobra's own arity errors are plain errors and
// would exit 1, and 2 is reserved for exactly "a missing argument".
func TestRunNeedsAnAccount(t *testing.T) {
	isolate(t)

	code, _, errOut, top := runRoot(t, "run")
	if code != ExitUsage {
		t.Fatalf("exit = %d (%s / %s), want %d", code, errOut, top, ExitUsage)
	}
	if got := top + errOut; !strings.Contains(got, "run needs an account") {
		t.Errorf("error = %q, want it to name what is missing", got)
	}
}

// stubClaude replaces the launcher and returns a pointer the test reads after
// the command has run. Starting a real `claude` from this suite is the hazard
// helpers_test.go documents for every other uncontrollable dependency: it would
// launch a live session against a t.TempDir() the framework then deletes.
func stubClaude(t *testing.T, code ExitCode) *claudeStub {
	return stubClaudeDuring(t, code, nil)
}

// stubClaudeDuring is stubClaude with a hook that runs while the session is
// still alive. Anything on disk has to be asserted from in here: the command
// deletes the session directory when the child exits, which is the behaviour
// TestRunRemovesTheSessionWhenTheChildExits pins, so a test that looks
// afterwards is looking at a directory that is correctly gone.
func stubClaudeDuring(t *testing.T, code ExitCode, during func(launchSpec)) *claudeStub {
	t.Helper()
	// The resolver is stubbed too. Without it every one of these tests passes
	// or fails on whether the developer happens to have Claude Code installed
	// — which is exactly the hazard isolate() exists for, and which CI found
	// by not having it. Tests that mean to exercise the real launcher name
	// their own binary with stubLookClaude.
	stubLookClaude(t, filepath.Join(t.TempDir(), "claude"))
	var stub claudeStub
	saved := startChild
	t.Cleanup(func() { startChild = saved })
	startChild = func(spec launchSpec) (ExitCode, error) {
		stub.started, stub.spec = true, spec
		if during != nil {
			during(spec)
		}
		return code, nil
	}
	return &stub
}

// claudeStub records whether the launcher ran and with what. `started` is a
// field of its own because "did not launch" is the assertion for every refusal:
// a zero launchSpec cannot be told from one the command built badly.
type claudeStub struct {
	started bool
	spec    launchSpec
}

// Everything at or after ACCT goes to claude verbatim, hyphens included.
// Cobra's default interspersed parsing refuses `-p` as an unknown shorthand
// before RunE is ever reached, so this is the test that pins
// SetInterspersed(false).
func TestRunHandsEveryTokenAfterTheAccountToClaude(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	stub := stubClaude(t, ExitOK)

	code, _, errOut, top := runRoot(t, "run", "1", "--json", "-p", "hi")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	want := []string{"--json", "-p", "hi"}
	if !slices.Equal(stub.spec.Args, want) {
		t.Errorf("claude args = %q, want %q", stub.spec.Args, want)
	}
}

// A literal `--` immediately after ACCT is consumed and dropped. pflag does
// not do this for us — measured, a `--` after the first positional stays in
// args and ArgsLenAtDash() reports -1 — so it is stripped by hand, and exactly
// once. The second separator is a real argument of claude's.
func TestRunDropsExactlyOneSeparatorAfterTheAccount(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want []string
	}{
		{"one immediately after the account is consumed", []string{"run", "1", "--", "--json"}, []string{"--json"}},
		{"the second one survives to claude", []string{"run", "1", "--", "--", "--json"}, []string{"--", "--json"}},
		{"one later in the tail is claude's", []string{"run", "1", "-p", "--", "hi"}, []string{"-p", "--", "hi"}},
		{"nothing at all after the account", []string{"run", "1"}, nil},
		{"a bare separator and nothing else", []string{"run", "1", "--"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			seedAccount(t, "u-1", "a@example.com")
			stub := stubClaude(t, ExitOK)

			code, _, errOut, top := runRoot(t, tc.argv...)
			if code != ExitOK {
				t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
			}
			if !slices.Equal(stub.spec.Args, tc.want) {
				t.Errorf("claude args = %q, want %q", stub.spec.Args, tc.want)
			}
		})
	}
}

// Ambiguity is never resolved interactively: `ccdad run` ends in an exec and
// callers need determinism, and every other account-taking command turns a
// resolution failure into exit 2. The second half of the assertion is the one
// that matters: a reference ccdad could not resolve must not start anything.
func TestRunRefusesAReferenceItCannotResolve(t *testing.T) {
	for _, tc := range []struct{ name, ref, want string }{
		{"no such account", "nobody@example.com", "no account matches"},
		{"an ambiguous email", "same@example.com", "matches more than one account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			seedAccount(t, "u-1", "same@example.com")
			seedAccount(t, "u-2", "same@example.com")
			stub := stubClaude(t, ExitOK)

			code, _, errOut, top := runRoot(t, "run", tc.ref)
			if code != ExitUsage {
				t.Fatalf("exit = %d (%s / %s), want %d", code, errOut, top, ExitUsage)
			}
			if stub.started {
				t.Errorf("started claude for a reference that does not resolve: %+v", stub.spec)
			}
		})
	}
}

// envOf reads one variable out of a child environment slice, and reports
// whether it was there at all. Defined-but-empty is a distinct answer from
// absent for both variables this command sets, so the bool is not optional.
func envOf(env []string, name string) (string, bool) {
	prefix := name + "="
	value, found := "", false
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			// Last wins, which is what os/exec's dedup does.
			value, found = strings.TrimPrefix(kv, prefix), true
		}
	}
	return value, found
}

// The whole point of the command: the child reads a credential home of its own,
// so the live login is not what decides who the session is.
// CLAUDE_SECURESTORAGE_CONFIG_DIR is the narrowest lever for that — it scopes
// credentials and their locks and nothing else.
func TestRunPointsTheChildAtACredentialHomeOfItsOwn(t *testing.T) {
	claude := isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	stub := stubClaude(t, ExitOK)

	code, _, errOut, top := runRoot(t, "run", "1")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	scoped, ok := envOf(stub.spec.Env, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	if !ok {
		t.Fatal("the child was not given CLAUDE_SECURESTORAGE_CONFIG_DIR at all, so it would read the live login")
	}
	if scoped == claude {
		t.Fatalf("the child was pointed at the live credential home %q", scoped)
	}
	// Not the system temp directory: /tmp is age-cleaned, and the OS would
	// delete a live session's credentials underneath a running claude.
	sessions := filepath.Join(mustPath(ccpath.StoreHome()), "sessions")
	if !strings.HasPrefix(scoped, sessions+string(filepath.Separator)) {
		t.Errorf("session credential home = %q, want it under %q", scoped, sessions)
	}
}

// The account the caller named is the account the session runs as. Two accounts
// are seeded so that "it wrote a credentials file" cannot pass for "it wrote the
// right one" — the failure this guards against is a resolver whose result is
// computed and then dropped.
func TestRunSeedsTheSessionWithTheChosenAccountsCredentials(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")
	var raw []byte
	stubClaudeDuring(t, ExitOK, func(spec launchSpec) {
		home, _ := envOf(spec.Env, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
		var err error
		if raw, err = os.ReadFile(filepath.Join(home, ccpath.CredentialsFile)); err != nil {
			t.Errorf("reading the session credentials: %v", err)
		}
	})

	code, _, errOut, top := runRoot(t, "run", "b@example.com")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if !strings.Contains(string(raw), "RT-u-2") {
		t.Errorf("session credentials do not carry the chosen account's token: %s", raw)
	}
	if strings.Contains(string(raw), "RT-u-1") {
		t.Errorf("session credentials carry another account's token: %s", raw)
	}
}

// A session credential home holds a live refresh token, so it gets the same
// hygiene as the store: 0700 on the directory, 0600 on the file. On Windows
// chmod is a no-op and the protection is the ACL inherited from %USERPROFILE%
// instead, which is why the assertion is unix-only.
func TestRunGivesTheSessionThePrivateModesTheStoreUses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no chmod on Windows; the inherited user-profile ACL is the v1 answer")
	}
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	stubClaudeDuring(t, ExitOK, func(spec launchSpec) {
		home, _ := envOf(spec.Env, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
		for _, tc := range []struct {
			path string
			want os.FileMode
		}{
			{home, 0o700},
			{filepath.Join(home, ccpath.CredentialsFile), 0o600},
		} {
			info, err := os.Stat(tc.path)
			if err != nil {
				t.Error(err)
				continue
			}
			if perm := info.Mode().Perm(); perm != tc.want {
				t.Errorf("%s mode = %v, want %v", tc.path, perm, tc.want)
			}
		}
	})

	if code, _, errOut, top := runRoot(t, "run", "1"); code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
}

// The headline promise of the command, and the one thing it must never get
// wrong: `ccdad run` is not a switch. Whatever Claude Code was logged in as
// before is what it is logged in as after.
func TestRunLeavesTheLiveLoginExactlyAsItFoundIt(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")
	live := mustPath(ccpath.CredentialsPath())
	if err := os.WriteFile(live, []byte(`{"claudeAiOauth":{"refreshToken":"RT-live"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	stubClaude(t, ExitOK)

	if code, _, errOut, top := runRoot(t, "run", "b@example.com"); code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	after, err := os.ReadFile(live)
	if err != nil {
		t.Fatalf("the live credentials file is gone: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("the live login changed:\n before %s\n after  %s", before, after)
	}
}

// stubLookClaude points the resolver at a program the test controls. It is the
// seam that lets the REAL launcher run in this suite: everything about starting
// a child — stdio, waiting, the exit status — is exercised, and only the binary
// is a stand-in.
func stubLookClaude(t *testing.T, path string) {
	t.Helper()
	saved := lookClaude
	t.Cleanup(func() { lookClaude = saved })
	lookClaude = func(string) (string, error) { return path, nil }
}

// `ccdad run` is a runner, so claude's status is the answer — the convention
// env(1), nohup(1) and sudo(8) all follow. The exit-code table describes what
// ccdad does and carves `run` out by name (README, "Exit codes"); past the
// launch there is no ccdad left to describe.
func TestRunPropagatesClaudesExitStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/bin/sh is the stand-in binary here")
	}
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	stubLookClaude(t, "/bin/sh")

	for _, want := range []ExitCode{0, 7, 1} {
		code, _, errOut, top := runRoot(t, "run", "1", "-c", fmt.Sprintf("exit %d", want))
		if code != want {
			t.Errorf("exit = %d (%s / %s), want %d", code, errOut, top, want)
		}
	}
}

// A child killed by a signal has no exit status of its own — ProcessState
// reports -1, and os.Exit(-1) exits 255. The shell convention is 128+N, and
// 130 for SIGINT is the value the exit contract already names, so a Ctrl-C'd
// session reads the same whether the shell reports it or ccdad does.
func TestRunReportsASignalKilledChildAsTheShellDoes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no signals; a Ctrl-C'd child exits with STATUS_CONTROL_C_EXIT")
	}
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	stubLookClaude(t, "/bin/sh")

	for _, tc := range []struct {
		signal string
		want   ExitCode
	}{
		{"INT", 130},
		{"TERM", 143},
	} {
		t.Run(tc.signal, func(t *testing.T) {
			code, _, errOut, top := runRoot(t, "run", "1", "-c", "kill -"+tc.signal+" $$")
			if code != tc.want {
				t.Errorf("exit = %d (%s / %s), want %d", code, errOut, top, tc.want)
			}
		})
	}
}

// A session directory holds a live refresh token at 0600. Leaving it behind is
// how a store accumulates credentials nobody knows about, so the wait exists
// partly to have somewhere to delete it from.
//
// The `.lock` sibling is not an oversight in the assertion: Claude Code's
// legacy OAuth refresh lock is `realpath(<credential home>) + ".lock"`, a
// directory BESIDE the session rather than inside it, so os.RemoveAll on the
// session alone misses it.
func TestRunRemovesTheSessionWhenTheChildExits(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	stubLookClaude(t, filepath.Join(t.TempDir(), "claude"))
	var home string
	saved := startChild
	t.Cleanup(func() { startChild = saved })
	startChild = func(spec launchSpec) (ExitCode, error) {
		home, _ = envOf(spec.Env, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
		// What Claude Code does on its first refresh, reproduced: it creates
		// the legacy lock as a sibling of the credential home.
		if err := os.MkdirAll(home+".lock", 0o700); err != nil {
			t.Error(err)
		}
		return ExitOK, nil
	}

	if code, _, errOut, top := runRoot(t, "run", "1"); code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	for _, path := range []string{home, home + ".lock"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s survived the session (err = %v)", path, err)
		}
	}
}

// The hazard that makes the wait worth having at all.
//
// Claude Code refreshes inside the session's credential home, and the OAuth
// server rotates the refresh token when it does — revoking the old one. The
// store still holds the old one. Delete the session without looking and the
// account is dead: `ccdad switch` installs a revoked credential, the daemon
// cannot poll it, and the only repair is `ccdad add` again.
//
// internal/tokens already carries the same adopt-back for the live file; this
// is that rule applied to a file only `run` knows about.
func TestRunAdoptsBackARefreshTokenTheSessionRotated(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	stubClaudeDuring(t, ExitOK, func(spec launchSpec) {
		home, _ := envOf(spec.Env, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
		rotated := `{"claudeAiOauth":{"accessToken":"AT2","refreshToken":"RT-rotated"}}`
		if err := os.WriteFile(filepath.Join(home, ccpath.CredentialsFile), []byte(rotated), 0o600); err != nil {
			t.Error(err)
		}
	})

	if code, _, errOut, top := runRoot(t, "run", "1"); code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := s.Credentials("u-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(blob["claudeAiOauth"]); !strings.Contains(got, "RT-rotated") {
		t.Errorf("stored credentials still hold the pre-session token (%s); the account is now unusable", got)
	}
}

// `claude` is often npm's `.cmd` shim, and exec.LookPath honors PATHEXT, so
// that is the path `ccdad run` gets back. Which programs those are is the
// whole condition on the shim branch now that resolution runs for every one of
// them, so it is asserted on its own rather than only through a launch.
//
// The spellings matter more than they look. extOf is hand-written because
// filepath picks its separator at BUILD time, so a `C:\...` path is a string
// with no separators at all on the machine these lines were typed on — and a
// directory carrying the only dot in the path is the case a naive last-dot
// scan gets wrong.
func TestCmdShimTargetIsExactlyWhatCmdExeRuns(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"npm's shim, in the spelling Windows hands back", `C:\Users\x\AppData\Roaming\npm\claude.cmd`, true},
		{"the extension is matched case-insensitively", `C:\CLAUDE.CMD`, true},
		{"a .bat is the same shim", `C:\claude.BAT`, true},
		{"a native exe is not routed through cmd.exe", `C:\Program Files\claude\claude.exe`, false},
		{"a unix path is not either", "/usr/local/bin/claude", false},
		{"no extension at all", `C:\npm\claude`, false},
		{"a dot in a DIRECTORY is not an extension", `C:\tools\claude.d\claude`, false},
		{"nor is one in a unix directory", "/opt/claude.d/claude", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cmdShimTarget(tc.path); got != tc.want {
				t.Errorf("cmdShimTarget(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// What cmd.exe would do to an argument, which is a separate question from
// whether cmd.exe is in the launch at all. It decides one thing now: whether a
// shim that could NOT be resolved past is refused or simply launched.
//
// Go builds a command line to CommandLineToArgvW rules and has no special case
// for .bat/.cmd anywhere; cmd.exe does not parse by those rules. An argument
// with no space, quote or backslash is emitted RAW, so `fix&whoami` reaches
// cmd.exe as two commands. That is arbitrary command execution out of prompt
// text, and the check is a pure function so it is tested everywhere rather than
// only where it fires.
func TestUnsafeForCmdShimCatchesWhatCmdExeWouldReinterpret(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"an ampersand chains a second command", []string{"-p", "fix&whoami"}, "fix&whoami"},
		{"a pipe does too", []string{"a|b"}, "a|b"},
		{"a redirect truncates a file", []string{"a>b"}, "a>b"},
		{"a caret escapes the next character", []string{"a^b"}, "a^b"},
		{"a percent expands a variable, even inside quotes", []string{"%PATH%"}, "%PATH%"},
		{"a quote toggles cmd.exe's quoting state", []string{`say "hi" there`}, `say "hi" there`},
		{"a newline ends the command line", []string{"a\nb"}, "a\nb"},
		{"ordinary prompts pass", []string{"-p", "summarize this file", "--json"}, ""},
		{"so does a path with spaces and backslashes", []string{"--add-dir", `C:\Users\x\my repo`}, ""},
		{"no arguments at all", nil, ""},
		{"the first offender is the one reported", []string{"ok", "a&b", "c|d"}, "a&b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unsafeForCmdShim(tc.args); got != tc.want {
				t.Errorf("unsafeForCmdShim(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// The refusal reaches the user as a usage error, and nothing is started.
func TestRunRefusesAnArgumentACmdShimWouldReinterpret(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	stub := stubClaude(t, ExitOK)
	stubLookClaude(t, `C:\Users\x\AppData\Roaming\npm\claude.cmd`)

	code, _, errOut, top := runRoot(t, "run", "1", "-p", "fix&whoami")
	if code != ExitUsage {
		t.Fatalf("exit = %d (%s / %s), want %d", code, errOut, top, ExitUsage)
	}
	if got := top + errOut; !strings.Contains(got, "fix&whoami") {
		t.Errorf("error = %q, want it to quote the argument it refused", got)
	}
	if stub.started {
		t.Error("started claude with an argument cmd.exe would have re-split")
	}
}

// `ccdad run` supports both credential roots, chosen by a flag, and
// --full-profile is the other one: a whole config home of its own rather than
// credentials alone, so the session keeps its MCP logins.
//
// The negative assertion is the load-bearing one. Claude Code resolves its
// credential root as CLAUDE_SECURESTORAGE_CONFIG_DIR ?? CLAUDE_CONFIG_DIR ??
// ~/.claude, and mcpOAuth lives INSIDE .credentials.json under that root — so
// setting both variables would scope mcpOAuth away from the profile and undo
// the only reason this mode exists. The variable must be absent, not empty:
// defined-but-empty resolves to ~/.claude, which is the live credential home.
func TestRunFullProfileScopesTheConfigHomeAndLeavesCredentialsInside(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	stub := stubClaude(t, ExitOK)

	code, _, errOut, top := runRoot(t, "run", "--full-profile", "1")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if value, ok := envOf(stub.spec.Env, "CLAUDE_SECURESTORAGE_CONFIG_DIR"); ok {
		t.Errorf("CLAUDE_SECURESTORAGE_CONFIG_DIR = %q and defined; it must be absent, or mcpOAuth is "+
			"scoped away from the profile that exists to keep it", value)
	}
	cfg, ok := envOf(stub.spec.Env, "CLAUDE_CONFIG_DIR")
	if !ok {
		t.Fatal("the child was given no CLAUDE_CONFIG_DIR, so --full-profile did nothing")
	}
	profiles := filepath.Join(mustPath(ccpath.StoreHome()), "profiles")
	if !strings.HasPrefix(cfg, profiles+string(filepath.Separator)) {
		t.Errorf("profile = %q, want it under %q", cfg, profiles)
	}
}

// The flag belongs to ccdad and is only ccdad's BEFORE the account.
// Everything at or after ACCT goes to claude, so the same spelling afterwards
// is a claude argument — surprising enough that it is pinned rather than left
// to be rediscovered as a bug report.
func TestRunTreatsFullProfileAfterTheAccountAsClaudesArgument(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	stub := stubClaude(t, ExitOK)

	if code, _, errOut, top := runRoot(t, "run", "1", "--full-profile"); code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if !slices.Equal(stub.spec.Args, []string{"--full-profile"}) {
		t.Errorf("claude args = %q, want it forwarded verbatim", stub.spec.Args)
	}
	if _, ok := envOf(stub.spec.Env, "CLAUDE_CONFIG_DIR"); ok {
		if value, _ := envOf(stub.spec.Env, "CLAUDE_SECURESTORAGE_CONFIG_DIR"); value == "" {
			t.Error("the flag after the account switched ccdad's own mode")
		}
	}
}

// What a profile is seeded from, and what it deliberately is not.
//
// Top-level files carry the state that makes a session feel like the user's:
// settings, the plugin registry, and the global config with its per-project
// trust and MCP servers. Directories carry the bulk and the machine-specific
// caches — `projects/` alone measured 2.9 GB of the 3.0 GB on the machine this
// was written on — and copying them would make the first run of every account
// a multi-minute file copy.
//
// The credentials file is excluded by name, and that exclusion is the whole
// point: the profile's login comes from the STORE, so `--full-profile` means
// "the user's settings, this account's login" rather than "a second copy of
// whoever is logged in right now".
func TestRunFullProfileSeedsFromTheLiveConfigHomeWithoutItsBulkOrItsLogin(t *testing.T) {
	claude := isolate(t)
	seedAccount(t, "u-1", "a@example.com")

	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(claude, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("settings.json", `{"theme":"dark"}`)
	write(".claude.json", `{"projects":{"/w":{"hasTrustDialogAccepted":true}}}`)
	write(".credentials.json", `{"claudeAiOauth":{"refreshToken":"RT-live"}}`)
	write("projects/w/session.jsonl", "bulk")

	var got map[string]string
	stubClaudeDuring(t, ExitOK, func(spec launchSpec) {
		profile, _ := envOf(spec.Env, "CLAUDE_CONFIG_DIR")
		got = map[string]string{}
		for _, name := range []string{"settings.json", ".claude.json", ".credentials.json"} {
			if body, err := os.ReadFile(filepath.Join(profile, name)); err == nil {
				got[name] = string(body)
			}
		}
		if _, err := os.Stat(filepath.Join(profile, "projects")); err == nil {
			got["projects"] = "copied"
		}
	})

	if code, _, errOut, top := runRoot(t, "run", "--full-profile", "1"); code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if got["settings.json"] != `{"theme":"dark"}` {
		t.Errorf("settings.json = %q, want the live one carried in", got["settings.json"])
	}
	if !strings.Contains(got[".claude.json"], "hasTrustDialogAccepted") {
		t.Errorf(".claude.json = %q, want the global config carried in", got[".claude.json"])
	}
	if _, copied := got["projects"]; copied {
		t.Error("projects/ was copied; that is the 97% this mode exists to avoid")
	}
	if !strings.Contains(got[".credentials.json"], "RT-u-1") {
		t.Errorf("profile credentials = %q, want the store's copy for this account", got[".credentials.json"])
	}
	if strings.Contains(got[".credentials.json"], "RT-live") {
		t.Error("the live login was copied into the profile; --full-profile is not a switch")
	}
}

// A profile is state, not a cache. Re-seeding it on the second run would throw
// away the trust answers and MCP logins the first session accumulated — which
// is the only thing --full-profile buys over the default mode.
func TestRunFullProfileKeepsWhatTheLastSessionChanged(t *testing.T) {
	claude := isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	if err := os.WriteFile(filepath.Join(claude, "settings.json"), []byte(`{"theme":"live"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var profile string
	stubClaudeDuring(t, ExitOK, func(spec launchSpec) {
		profile, _ = envOf(spec.Env, "CLAUDE_CONFIG_DIR")
	})
	if code, _, errOut, top := runRoot(t, "run", "--full-profile", "1"); code != ExitOK {
		t.Fatalf("first run exit = %d (%s / %s), want 0", code, errOut, top)
	}

	// The session changes something inside its own profile, the way a real one
	// would by answering a trust prompt or logging into an MCP server.
	changed := filepath.Join(profile, "settings.json")
	if err := os.WriteFile(changed, []byte(`{"theme":"chosen-in-session"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if code, _, errOut, top := runRoot(t, "run", "--full-profile", "1"); code != ExitOK {
		t.Fatalf("second run exit = %d (%s / %s), want 0", code, errOut, top)
	}
	body, err := os.ReadFile(changed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "chosen-in-session") {
		t.Errorf("settings.json = %s, want the profile's own state kept", body)
	}
}

// A defined-but-empty CLAUDE_CONFIG_DIR is not "unset" to Claude Code, and it
// does not mean what it means to ccdad.
//
// Measured against 2.1.238: its An() coalesces with `??`, so an empty value
// stays empty and every derived path becomes relative — a session started that
// way creates projects/, sessions/ and backups/ in whatever directory the user
// happened to be in. ccpath.ConfigHome uses `!= ""` and reads the same variable
// as unset. The two disagree, so ccdad removes it rather than passing on a
// value whose meaning depends on who reads it.
func TestRunDoesNotPassOnADefinedButEmptyConfigDir(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	stub := stubClaude(t, ExitOK)

	if code, _, errOut, top := runRoot(t, "run", "1"); code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if value, ok := envOf(stub.spec.Env, "CLAUDE_CONFIG_DIR"); ok && value == "" {
		t.Error("the child inherited CLAUDE_CONFIG_DIR= (empty), which points its config home at the " +
			"working directory rather than at the user's")
	}
}

// Where `--help` is decides whose help it is, and the answer surprises people:
// before the account it is ccdad's, after it is claude's. That falls out of
// the "everything at or after ACCT" rule rather than being a separate one, and
// it is the reason DisableFlagParsing is the wrong tool — it would swallow
// ccdad's own help along with everything else.
func TestRunSplitsHelpAtTheAccount(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")

	for _, flag := range []string{"--help", "-h"} {
		t.Run("before the account: "+flag, func(t *testing.T) {
			stub := stubClaude(t, ExitOK)
			code, stdout, _, _ := runRoot(t, "run", flag)
			if code != ExitOK {
				t.Fatalf("exit = %d, want 0", code)
			}
			if !strings.Contains(stdout, "full-profile") {
				t.Errorf("ccdad's own help was not printed:\n%s", stdout)
			}
			if stub.started {
				t.Error("started claude for a request for ccdad's help")
			}
		})
		t.Run("after the account: "+flag, func(t *testing.T) {
			stub := stubClaude(t, ExitOK)
			code, stdout, _, _ := runRoot(t, "run", "1", flag)
			if code != ExitOK {
				t.Fatalf("exit = %d, want 0", code)
			}
			if !slices.Equal(stub.spec.Args, []string{flag}) {
				t.Errorf("claude args = %q, want %q forwarded", stub.spec.Args, flag)
			}
			if strings.Contains(stdout, "full-profile") {
				t.Errorf("ccdad answered a request meant for claude:\n%s", stdout)
			}
		})
	}
}

// seedTokenAccount stores an account whose credential is a token ccdad holds
// rather than an OAuth record Claude Code reads from a file.
func seedTokenAccount(t *testing.T, uuid, email, kind, token string) {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	rec, err := json.Marshal(cclink.TokenRecord{Kind: kind, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(store.Account{Provider: provider.Claude, UUID: uuid, Email: email}, cclink.Blob{cclink.TokenKey: rec}); err != nil {
		t.Fatal(err)
	}
}

// `ccdad switch` refuses a setup-token account, and its refusal says what to do
// instead: "Run Claude Code with it exported for the session instead:
// CLAUDE_CODE_OAUTH_TOKEN=<token> claude". That is this command's whole job, so
// run does it rather than repeating the refusal.
//
// No credentials file is written: the stored record is ccdad's own key, not an
// OAuth login, and writing it would put a file in the session that Claude Code
// cannot read as a login while looking exactly like one.
func TestRunExportsASetupTokenRatherThanWritingAFileClaudeCannotRead(t *testing.T) {
	isolate(t)
	seedTokenAccount(t, "u-1", "a@example.com", "setup-token", "sk-ant-oat-XYZ")

	var wrote bool
	stub := stubClaudeDuring(t, ExitOK, func(spec launchSpec) {
		home, _ := envOf(spec.Env, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
		_, err := os.Stat(filepath.Join(home, ccpath.CredentialsFile))
		wrote = err == nil
	})

	code, _, errOut, top := runRoot(t, "run", "1")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if got, ok := envOf(stub.spec.Env, "CLAUDE_CODE_OAUTH_TOKEN"); !ok || got != "sk-ant-oat-XYZ" {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN = %q (set: %v), want the stored setup token", got, ok)
	}
	if wrote {
		t.Error("wrote a credentials file for an account whose credential is not an OAuth login")
	}
}

// An API key is not a file Claude Code reads out of the credential home: it is
// primaryApiKey in the GLOBAL config, which the default mode deliberately
// shares with the live session. Writing it there would be a live mutation, and
// this command's promise is that it makes none.
func TestRunRefusesAnAPIKeyAccountAndSaysWhy(t *testing.T) {
	isolate(t)
	seedTokenAccount(t, "u-1", "a@example.com", cclink.APIKeyKind, "sk-ant-api-XYZ")
	stub := stubClaude(t, ExitOK)

	code, _, errOut, top := runRoot(t, "run", "1")
	if code != ExitUsage {
		t.Fatalf("exit = %d (%s / %s), want %d", code, errOut, top, ExitUsage)
	}
	if got := top + errOut; !strings.Contains(got, "API key") {
		t.Errorf("error = %q, want it to name what it cannot run", got)
	}
	if stub.started {
		t.Error("started a session that would not have been authenticated as that account")
	}
}

// The adopt-back is for the credential ccdad put there, and only that.
//
// A setup-token account gets no credentials file — Claude Code reads its token
// from the environment — but a session is a whole Claude Code, and a user who
// runs /login inside one leaves a claudeAiOauth behind in the session's own
// home. Carrying that into the store would silently attach an OAuth login to an
// account whose stored credential is a token record, changing what `switch`
// and attribution make of it. The account's identity is not the session's to
// change.
func TestRunDoesNotAdoptALoginBackIntoATokenAccount(t *testing.T) {
	isolate(t)
	seedTokenAccount(t, "u-1", "a@example.com", "setup-token", "sk-ant-oat-XYZ")
	stubClaudeDuring(t, ExitOK, func(spec launchSpec) {
		home, _ := envOf(spec.Env, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
		body := `{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-logged-in-inside"}}`
		if err := os.WriteFile(filepath.Join(home, ccpath.CredentialsFile), []byte(body), 0o600); err != nil {
			t.Error(err)
		}
	})

	if code, _, errOut, top := runRoot(t, "run", "1"); code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := s.Credentials("u-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := blob["claudeAiOauth"]; ok {
		t.Errorf("a session's own login was attached to a setup-token account: %v", blob)
	}
}

// liveGlobalConfig is what a test reads to prove `run` made no live mutation.
// A missing file reads as "" so the assertion works on a machine Claude Code
// has never been configured on, which is where a first `ccdad run` happens.
func liveGlobalConfig(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(mustPath(ccpath.GlobalConfigPath()))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// An API key is primaryApiKey in a GLOBAL config, so the mode that owns a
// global config is the mode that can serve one. --full-profile owns one; this
// is the account shape `run` used to refuse outright.
func TestRunFullProfileServesAnAPIKeyAccount(t *testing.T) {
	claude := isolate(t)
	seedTokenAccount(t, "u-1", "a@example.com", cclink.APIKeyKind, "sk-ant-api-XYZ")
	if err := os.WriteFile(filepath.Join(claude, ".claude.json"), []byte(`{"numStartups":7}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before := liveGlobalConfig(t)
	stub := stubClaude(t, ExitOK)

	code, _, errOut, top := runRoot(t, "run", "--full-profile", "1")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if !stub.started {
		t.Fatal("no session was started for an API-key account under --full-profile")
	}

	profile, ok := envOf(stub.spec.Env, "CLAUDE_CONFIG_DIR")
	if !ok {
		t.Fatal("the child was given no CLAUDE_CONFIG_DIR")
	}
	cfg, err := cclink.LoadGlobalConfigAt(ccpath.GlobalConfigPathIn(profile))
	if err != nil {
		t.Fatal(err)
	}
	if key, keyed := cclink.PrimaryAPIKey(cfg); !keyed || key != "sk-ant-api-XYZ" {
		t.Errorf("the profile's primaryApiKey = %q (set: %v), want the account's stored key", key, keyed)
	}
	// The whole promise of the command, on the file this change taught it to
	// write. A resolver that fell back to the ambient path would satisfy every
	// assertion above and fail this one.
	if after := liveGlobalConfig(t); after != before {
		t.Fatalf("the live global config was rewritten\nbefore: %s\nafter:  %s", before, after)
	}
	// And no credentials file, for the reason the setup-token branch gives:
	// writing the stored record into one puts something in the session that
	// looks like a login and is not one. Here it would also make the key
	// ccdad just installed inert.
	if _, err := os.Stat(filepath.Join(profile, ccpath.CredentialsFile)); !os.IsNotExist(err) {
		t.Errorf("wrote a credentials file for an API-key account (stat err: %v)", err)
	}
}

// The warning is about a LOGIN, and a profile's credentials file is not
// necessarily one: a session that used an MCP server but never signed in
// leaves an mcpOAuth behind with no claudeAiOauth beside it. Warning there
// would tell the user their key is inert when it is the credential Claude Code
// is about to use.
func TestRunFullProfileDoesNotWarnAboutACredentialsFileThatHoldsNoLogin(t *testing.T) {
	isolate(t)
	seedTokenAccount(t, "u-1", "a@example.com", cclink.APIKeyKind, "sk-ant-api-XYZ")
	profile := filepath.Join(mustPath(ccpath.StoreHome()), ProfilesDirName, "u-1")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, ccpath.CredentialsFile),
		[]byte(`{"mcpOAuth":{"srv":{"accessToken":"MCP"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stubClaude(t, ExitOK)

	code, _, errOut, top := runRoot(t, "run", "--full-profile", "1")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if strings.Contains(errOut, "reads it in preference") {
		t.Fatalf("warned that the key was inert behind a file holding no login:\n%s", errOut)
	}
}

// Claude Code reads the legacy <config home>/.config.json in preference when
// it is there, and seedProfile copies top-level FILES — so on a machine that
// has one, every profile has one. A key written to <profile>/.claude.json
// there goes into a file the session never reads, and the session runs
// unauthenticated while ccdad reports success.
func TestRunFullProfilePutsTheKeyInTheFileTheProfileActuallyReads(t *testing.T) {
	claude := isolate(t)
	seedTokenAccount(t, "u-1", "a@example.com", cclink.APIKeyKind, "sk-ant-api-XYZ")
	if err := os.WriteFile(filepath.Join(claude, ".config.json"), []byte(`{"numStartups":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := stubClaude(t, ExitOK)

	if code, _, errOut, top := runRoot(t, "run", "--full-profile", "1"); code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	profile, _ := envOf(stub.spec.Env, "CLAUDE_CONFIG_DIR")
	legacy := filepath.Join(profile, ".config.json")
	cfg, err := cclink.LoadGlobalConfigAt(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if key, ok := cclink.PrimaryAPIKey(cfg); !ok || key != "sk-ant-api-XYZ" {
		t.Errorf("primaryApiKey in %s = %q (set: %v); the key went somewhere the session does not read", legacy, key, ok)
	}
}

// The default mode still refuses, and the refusal has to name the way out.
// "Use 'ccdad switch'" was the whole answer before, and it is the wrong one
// for someone who asked for a session precisely because they did not want to
// move the live login.
func TestRunStillRefusesAnAPIKeyAccountInTheDefaultModeAndNamesTheFlag(t *testing.T) {
	claude := isolate(t)
	seedTokenAccount(t, "u-1", "a@example.com", cclink.APIKeyKind, "sk-ant-api-XYZ")
	if err := os.WriteFile(filepath.Join(claude, ".claude.json"), []byte(`{"numStartups":7}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before := liveGlobalConfig(t)
	stub := stubClaude(t, ExitOK)

	code, _, errOut, top := runRoot(t, "run", "1")
	if code != ExitUsage {
		t.Fatalf("exit = %d (%s / %s), want %d", code, errOut, top, ExitUsage)
	}
	if got := top + errOut; !strings.Contains(got, "--full-profile") {
		t.Errorf("the refusal does not name the mode that can serve it: %q", got)
	}
	if stub.started {
		t.Error("started a session that would not have been authenticated as that account")
	}
	if after := liveGlobalConfig(t); after != before {
		t.Fatalf("the refusing path still wrote the live global config\nbefore: %s\nafter:  %s", before, after)
	}
}

// A profile is persistent, so an OAuth login left in it by an earlier session
// — a /login typed inside one — outlives that session and makes primaryApiKey
// inert. ccdad writes the key anyway and says so, rather than deleting a login
// the user made inside their own profile.
func TestRunFullProfileWarnsWhenAnEarlierLoginWouldMakeTheKeyInert(t *testing.T) {
	isolate(t)
	seedTokenAccount(t, "u-1", "a@example.com", cclink.APIKeyKind, "sk-ant-api-XYZ")
	profile := filepath.Join(mustPath(ccpath.StoreHome()), ProfilesDirName, "u-1")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, ccpath.CredentialsFile),
		[]byte(`{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stubClaude(t, ExitOK)

	code, _, errOut, top := runRoot(t, "run", "--full-profile", "1")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if !strings.Contains(errOut, "reads it in preference") {
		t.Fatalf("no warning that the key is inert behind a login already in the profile:\n%s", errOut)
	}
	// Not deleted: the login belongs to whoever made it in there.
	if _, err := os.Stat(filepath.Join(profile, ccpath.CredentialsFile)); err != nil {
		t.Fatalf("the profile's existing login was removed: %v", err)
	}
}

// The default mode's scoping is inert on Claude Code 2.1.112 and earlier, and
// this is what the command does about it. The user's ruling was to refuse:
// continuing runs the session as the machine's LIVE login while ccdad reports
// success, which is the one thing this command promises not to do.

// The refusal itself. It is a usage error rather than a runtime one because the
// caller can fix it with a flag, and nothing is started — the assertion on the
// stub is not decoration: a check placed after the launch would satisfy the
// exit code and still have run the session as the wrong account.
func TestRunRefusesTheDefaultModeOnAClaudeCodeThatCannotScope(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	stub := stubClaude(t, ExitOK)
	stubClaudeInstall(t, claudeVersion(2, 1, 112), nil)

	code, _, errOut, top := runRoot(t, "run", "a@example.com")
	if code != ExitUsage {
		t.Fatalf("exit = %d (%s / %s), want %d", code, errOut, top, ExitUsage)
	}
	if stub.started {
		t.Fatal("started a session that would have run as the machine's live login")
	}
	message := errOut + top
	for _, want := range []string{
		// Why, in the terms the user can check: the variable and the release
		// it arrived in.
		"CLAUDE_SECURESTORAGE_CONFIG_DIR",
		"2.1.113",
		// Which account they thought they were getting.
		"a@example.com",
		// The way out that works on THIS machine, and the way out that removes
		// the problem.
		"--full-profile",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, message)
		}
	}
}

// The escape hatch has to actually work, or the refusal is a dead end.
// --full-profile scopes CLAUDE_CONFIG_DIR, which every era of Claude Code reads,
// so it is the one mode that still isolates on such a machine.
func TestRunFullProfileStillRunsOnAClaudeCodeThatCannotScope(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	stub := stubClaude(t, ExitOK)
	stubClaudeInstall(t, claudeVersion(2, 1, 112), nil)

	code, _, errOut, top := runRoot(t, "run", "--full-profile", "a@example.com")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0 — --full-profile is the remedy the refusal names", code, errOut, top)
	}
	if !stub.started {
		t.Fatal("no session was started")
	}
	if _, ok := envOf(stub.spec.Env, "CLAUDE_CONFIG_DIR"); !ok {
		t.Error("the child was not given CLAUDE_CONFIG_DIR, which is the only scoping this build honours")
	}
}

// The releases either side of the boundary, and the unreadable case, all in one
// table so the refusal cannot quietly widen. The 2.1.113 row is the one that
// matters most: an off-by-one on the boundary would refuse on the very release
// that fixed the problem, which is every current machine.
func TestRunRefusesOnlyWhatItMeasured(t *testing.T) {
	for _, tc := range []struct {
		name    string
		install ccver.Install
	}{
		{"the release that introduced the variable", claudeVersion(2, 1, 113)},
		{"a current release", claudeVersion(2, 1, 241)},
		// A refusal keyed on "ccdad could not tell" would break every machine
		// whose install ccdad cannot classify, and those machines are fine.
		{"an install ccdad could not classify", ccver.Install{Launcher: "/opt/weird/claude", Method: ccver.MethodUnknown}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			seedAccount(t, "u-1", "a@example.com")
			stub := stubClaude(t, ExitOK)
			stubClaudeInstall(t, tc.install, nil)

			code, _, errOut, top := runRoot(t, "run", "a@example.com")
			if code != ExitOK {
				t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
			}
			if !stub.started {
				t.Fatal("no session was started")
			}
		})
	}
}

// The version is read from the path PATH gave, and read BEFORE the cmd-shim
// rewrite, so the verdict belongs to the claude this invocation is about to run
// rather than to the interpreter ccdad ends up exec'ing on its behalf.
//
// The fixture is a real npm shim with an argument cmd.exe would re-split,
// because that is the only way the ordering is observable: launchPastShim then
// replaces `path` with node.exe, and a plain launcher never triggers it. Written
// with an ordinary fixture this test passed with the whole block moved AFTER the
// rewrite — it could not fail for the property it exists to pin.
func TestRunDescribesTheClaudeItIsAboutToRunAndNotTheInterpreter(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	stub := stubClaude(t, ExitOK)
	shim := installShim(t, "env-node.cmd")
	stubFileExists(t, false)
	stubLookProgram(t, `C:\Program Files\nodejs\node.exe`, nil)

	var asked []string
	saved := describeClaudeInstall
	t.Cleanup(func() { describeClaudeInstall = saved })
	describeClaudeInstall = func(path string) ccver.Install {
		asked = append(asked, path)
		return ccver.Install{Launcher: path}
	}

	if code, _, errOut, top := runRoot(t, "run", "1", "-p", "fix&whoami"); code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	// The launch really did go past the shim, or the ordering below is not
	// under test at all.
	if stub.spec.Path != `C:\Program Files\nodejs\node.exe` {
		t.Fatalf("launched %q — the shim rewrite did not happen, so this test proves nothing", stub.spec.Path)
	}
	if len(asked) != 1 {
		t.Fatalf("described %v, want exactly one path", asked)
	}
	if asked[0] != shim {
		t.Errorf("described %q, want the shim %q — node.exe has no Claude Code version, and asking about it "+
			"would answer 'unknown' on every machine that goes past a shim", asked[0], shim)
	}
}

// A setup-token account is NOT refused on a keychain-era build, and this is the
// half the first version of the refusal got wrong.
//
// authorise scopes such an account with CLAUDE_CODE_OAUTH_TOKEN in the child's
// environment and writes no credentials file at all. That variable predates
// 2.1.113 by a long way, and this tree's own measurement is that Claude Code
// prefers it over the stored LOGIN outright — so the session runs as the named
// account on a 2.1.112 machine, and the refusal's stated failure mode ("claude
// would read the machine's own credentials file") cannot happen to it.
//
// Beating the login is not beating everything: ANTHROPIC_AUTH_TOKEN is read one
// branch earlier and takes such a session from it on every build. That is
// refuseDisplacedAuth's subject, and isolate() empties the variable, which is
// why this test describes the era and only the era.
func TestRunDoesNotRefuseASetupTokenAccountOnAClaudeCodeThatCannotScope(t *testing.T) {
	isolate(t)
	seedTokenAccount(t, "u-1", "a@example.com", "setup-token", "sk-ant-oat-1")
	stub := stubClaude(t, ExitOK)
	stubClaudeInstall(t, claudeVersion(2, 1, 112), nil)

	code, _, errOut, top := runRoot(t, "run", "1")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0 — a setup token is scoped by the environment, not by the "+
			"credential home the era ignores", code, errOut, top)
	}
	if !stub.started {
		t.Fatal("no session was started")
	}
	if got, ok := envOf(stub.spec.Env, "CLAUDE_CODE_OAUTH_TOKEN"); !ok || got != "sk-ant-oat-1" {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN = %q, %v; want the account's token — that is what scopes this session",
			got, ok)
	}
}

// An api-key account on a keychain-era build gets authorise's accurate refusal,
// not the keychain one. Both are exit 2, so the level cannot tell them apart:
// the assertion is on the sentence that only the right one carries.
func TestRunGivesAnAPIKeyAccountItsOwnRefusalEvenOnAClaudeCodeThatCannotScope(t *testing.T) {
	isolate(t)
	seedTokenAccount(t, "u-1", "a@example.com", cclink.APIKeyKind, "sk-ant-api-1")
	stubClaude(t, ExitOK)
	stubClaudeInstall(t, claudeVersion(2, 1, 112), nil)

	code, _, errOut, top := runRoot(t, "run", "1")
	if code != ExitUsage {
		t.Fatalf("exit = %d (%s / %s), want %d", code, errOut, top, ExitUsage)
	}
	message := errOut + top
	if !strings.Contains(message, "API key account") {
		t.Errorf("the api-key refusal was replaced by another one:\n%s", message)
	}
	if strings.Contains(message, "CLAUDE_SECURESTORAGE_CONFIG_DIR") {
		t.Errorf("an api-key account was refused for the era instead of for its credential shape:\n%s", message)
	}
}

// A shell that already exports ANTHROPIC_AUTH_TOKEN outranks the login this
// session was built to scope, and the session would authenticate as that token
// while ccdad reported success.
//
// The assertion is not only the exit code: `run` has three refusals now and all
// three are exit 2, so the level cannot tell them apart. What only this one
// carries is the variable's name and the escape hatch.
func TestRunRefusesWhenTheShellsAuthTokenOutranksTheSessionsLogin(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	stub := stubClaude(t, ExitOK)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-ant-someone-elses")

	code, _, errOut, top := runRoot(t, "run", "1")
	if code != ExitUsage {
		t.Fatalf("exit = %d (%s / %s), want %d — the session would have run as that token", code, errOut, top, ExitUsage)
	}
	message := errOut + top
	if !strings.Contains(message, "ANTHROPIC_AUTH_TOKEN") {
		t.Errorf("the refusal does not name the source that wins:\n%s", message)
	}
	if !strings.Contains(message, "env -u ANTHROPIC_AUTH_TOKEN") {
		t.Errorf("the refusal does not say how to start this one session anyway:\n%s", message)
	}
	if stub.started {
		t.Error("claude was started — a refusal that still runs the session is not a refusal")
	}
}

// The same shell, and an account whose credential is a setup token rather than
// a login. The keychain-era carve-out above lets this shape through, and it
// must not let this one through: CLAUDE_CODE_OAUTH_TOKEN is the branch BELOW
// ANTHROPIC_AUTH_TOKEN, so exporting the account's token does not win.
func TestRunRefusesASetupTokenAccountWhenTheShellsAuthTokenOutranksIt(t *testing.T) {
	isolate(t)
	seedTokenAccount(t, "u-1", "a@example.com", "setup-token", "sk-ant-oat-1")
	stub := stubClaude(t, ExitOK)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-ant-someone-elses")

	code, _, errOut, top := runRoot(t, "run", "1")
	if code != ExitUsage {
		t.Fatalf("exit = %d (%s / %s), want %d", code, errOut, top, ExitUsage)
	}
	if stub.started {
		t.Error("claude was started as somebody else's token")
	}
}

// A CLAUDE_CODE_OAUTH_TOKEN already in the shell outranks a LOGIN, so a login
// account is refused for it.
func TestRunRefusesALoginAccountWhenTheShellExportsAnOAuthToken(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	stub := stubClaude(t, ExitOK)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-someone-elses")

	code, _, errOut, top := runRoot(t, "run", "1")
	if code != ExitUsage {
		t.Fatalf("exit = %d (%s / %s), want %d", code, errOut, top, ExitUsage)
	}
	if !strings.Contains(errOut+top, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("the refusal does not name the source that wins:\n%s", errOut+top)
	}
	if stub.started {
		t.Error("claude was started")
	}
}

// …and the SAME variable in the same shell is not a displacer for a setup-token
// account, because ccdad overwrites it with that account's own token. This is
// the case a refusal keyed on the variable being PRESENT would break, and it
// would break it silently — the session it blocks is one that works today.
func TestRunOverwritesTheShellsOAuthTokenForASetupTokenAccount(t *testing.T) {
	isolate(t)
	seedTokenAccount(t, "u-1", "a@example.com", "setup-token", "sk-ant-oat-mine")
	stub := stubClaude(t, ExitOK)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-someone-elses")

	code, _, errOut, top := runRoot(t, "run", "1")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0 — ccdad sets this variable itself for such an account",
			code, errOut, top)
	}
	if got, ok := envOf(stub.spec.Env, "CLAUDE_CODE_OAUTH_TOKEN"); !ok || got != "sk-ant-oat-mine" {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN = %q, %v; want the account's own token", got, ok)
	}
}

// A source with no variable behind it gets the remedy that applies to IT, and
// not an `env -u` for a name that is not a variable. The host token file is the
// clearest of the three: Claude Code reads that path on every machine when the
// descriptor variable is unset, and there is nothing to unset.
func TestRunRefusesForAHostTokenFileWithoutOfferingEnvU(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	stub := stubClaude(t, ExitOK)
	if err := os.MkdirAll(filepath.Dir(identity.HostOAuthTokenFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identity.HostOAuthTokenFile, []byte("injected-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, errOut, top := runRoot(t, "run", "1")
	if code != ExitUsage {
		t.Fatalf("exit = %d (%s / %s), want %d", code, errOut, top, ExitUsage)
	}
	message := errOut + top
	if !strings.Contains(message, identity.HostOAuthTokenFile) {
		t.Errorf("the refusal does not name the file that wins:\n%s", message)
	}
	if strings.Contains(message, "env -u") {
		t.Errorf("the refusal offers to unset a variable that does not exist:\n%s", message)
	}
	if stub.started {
		t.Error("claude was started")
	}
}

// --full-profile scopes a different DIRECTORY; it does not scope the
// environment, which is os.Environ() in either mode. The refusal therefore
// covers both — and it lands before the profile exists, because a refusal that
// first created persistent per-account state would leave the user something to
// clean up for a session that never started.
func TestRunRefusesUnderFullProfileAndLeavesNoProfileBehind(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	stub := stubClaude(t, ExitOK)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-ant-someone-elses")

	code, _, errOut, top := runRoot(t, "run", "--full-profile", "1")
	if code != ExitUsage {
		t.Fatalf("exit = %d (%s / %s), want %d", code, errOut, top, ExitUsage)
	}
	if stub.started {
		t.Error("claude was started")
	}
	profile := filepath.Join(mustPath(ccpath.StoreHome()), ProfilesDirName, "u-1")
	if _, err := os.Stat(profile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Stat(%s) = %v, want it never to have been created", profile, err)
	}
}

// The refusal runs before the credential home is made, for the reason
// refuseUnscopedRun's own placement gives: a refusal that first created a
// directory holding a live refresh token is tidied by the defer, but only after
// the fact.
func TestRunRefusesBeforeItCreatesASessionDirectory(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	stubClaude(t, ExitOK)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-ant-someone-elses")

	if code, _, errOut, top := runRoot(t, "run", "1"); code != ExitUsage {
		t.Fatalf("exit = %d (%s / %s), want %d", code, errOut, top, ExitUsage)
	}
	container := filepath.Join(mustPath(ccpath.StoreHome()), SessionsDirName)
	if _, err := os.Stat(container); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Stat(%s) = %v, want no session container at all", container, err)
	}
}

// An empty shell is the case every other test in this file describes, and it is
// asserted once here so that a gate which refused EVERYTHING would fail
// something that says so rather than only failing the rest by accident.
func TestRunStartsWhenNothingInTheShellOutranksTheAccount(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	stub := stubClaude(t, ExitOK)

	if code, _, errOut, top := runRoot(t, "run", "1"); code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if !stub.started {
		t.Fatal("no session was started")
	}
}

// `ccdad run <codex-account>` pins the launch to that account: the proxy serves
// its credential whatever the serving pointer says, and a switch does not move
// the session.
func TestRunOnACodexAccountPinsTheLaunch(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-run-1", "c@example.com")
	stub, port := routedWorld(t, ExitOK, nil)

	code, _, errOut, top := runRoot(t, "run", "c@example.com")
	if code != ExitOK {
		t.Fatalf("run on a codex account = %d, want 0\n%s\n%s", code, errOut, top)
	}
	if !stub.started {
		t.Fatal("no codex was started")
	}
	if want := fmt.Sprintf(`model_providers.ccdad.base_url="http://127.0.0.1:%d"`, port); !slices.Contains(stub.spec.Args, want) {
		t.Errorf("codex was given %q, want it pointed at %s", stub.spec.Args, want)
	}
	if v := envValueOf(stub.spec.Env, codexKeyEnv); v == "" {
		t.Error("the child got no launch secret")
	}
}

// The tail is OPTIONAL and defaults to codex, because `ccdad run <acct>` on a
// Claude account starts claude with no arguments and the shapes have to match.
func TestRunOnACodexAccountAcceptsAnExplicitCodexTail(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-run-2", "c@example.com")
	stub, _ := routedWorld(t, ExitOK, nil)

	if code, _, errOut, _ := runRoot(t, "run", "c@example.com", "--", "codex", "exec", "hi"); code != ExitOK {
		t.Fatalf("run = %d, want 0\n%s", code, errOut)
	}
	tail := stub.spec.Args[len(stub.spec.Args)-2:]
	if tail[0] != "exec" || tail[1] != "hi" {
		t.Errorf("codex was given %q, want it to end in the tail that was typed", stub.spec.Args)
	}
	// Asserted separately from the tail above, because the tail cannot see it:
	// a launch that forwarded the word would put it BEFORE `exec hi`, leaving
	// the last two arguments exactly where they are.
	if slices.Contains(stub.spec.Args, codexProgramName) {
		t.Errorf("codex was given %q; the word `codex` is the program and must not reach it as an argument", stub.spec.Args)
	}
}

// Anything else in the tail is a usage error rather than a silent launch of
// something the proxy cannot serve.
func TestRunOnACodexAccountRefusesAnotherProgram(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-run-3", "c@example.com")
	routedWorld(t, ExitOK, nil)

	code, _, errOut, top := runRoot(t, "run", "c@example.com", "--", "python", "-c", "print(1)")
	if code != ExitUsage {
		t.Fatalf("run on a codex account with a non-codex tail = %d, want %d\n%s\n%s", code, ExitUsage, errOut, top)
	}
	if !strings.Contains(errOut+top, "codex") {
		t.Errorf("the refusal does not say what the tail has to begin with:\n%s\n%s", errOut, top)
	}
}

// `codex login` and `codex logout` are the one tail that begins with `codex`
// and still cannot be run as the named account: neither reads it, and both act
// on codex's own home. `ccdad codex exec` hands them to the real codex because
// the shim makes that command the machine's `codex`; a pinned launch has no
// such invocation to keep working, so it refuses at exit 2 rather than spending
// a login on a home the account has nothing to do with.
//
// The machine here is fully routed, so the refusal is about the verb and not
// about a missing daemon -- which is what TestRunOnACodexAccountRefusesWhenThereIsNoProxy
// covers.
func TestRunOnACodexAccountRefusesTheLoginAndLogoutTails(t *testing.T) {
	for _, verb := range []string{"login", "logout"} {
		t.Run(verb, func(t *testing.T) {
			isolate(t)
			seedCodexAccount(t, "cx-run-5", "c@example.com")
			stub, _ := routedWorld(t, ExitOK, nil)

			code, _, errOut, top := runRoot(t, "run", "c@example.com", "--", "codex", verb)
			if code != ExitUsage {
				t.Fatalf("run on a codex account with a %s tail = %d, want %d\n%s\n%s",
					verb, code, ExitUsage, errOut, top)
			}
			// The load-bearing half: a refusal that still started codex would
			// have revoked the grant and then reported the refusal.
			if stub.started {
				t.Errorf("the refusal started codex anyway, as %q", stub.spec.Args)
			}
			said := errOut + top
			if !strings.Contains(said, "c@example.com") {
				t.Errorf("the refusal does not name the account:\n%s", said)
			}
			if !strings.Contains(said, "plain shell") || !strings.Contains(said, "ccdad codex add") {
				t.Errorf("the refusal does not name both ways to do what was meant:\n%s", said)
			}
		})
	}
}

// The help text and the behaviour, checked against each other. `run`'s Long
// promises a launch that never falls back, and that promise is only true
// because of the refusal above -- so a session that deletes one half has to
// fail here rather than leave the other half lying.
func TestRunsHelpSaysTheLoginTailsAreRefused(t *testing.T) {
	long := newRunCmd().Long
	for _, want := range []string{"codex login", "codex logout", "refused", "plain shell", "ccdad codex add"} {
		if !strings.Contains(long, want) {
			t.Errorf("`ccdad run` help does not mention %q, so it promises a carve-out it does not have:\n%s",
				want, long)
		}
	}
}

// A PINNED launch never falls back. The user named an account; running the
// session as whatever codex's own home holds would bill a different one and
// report success. Exit 2, which is `ccdad run`'s contract for a refusal.
func TestRunOnACodexAccountRefusesWhenThereIsNoProxy(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-run-4", "c@example.com")
	stubDaemonWorld(t, &fakeDaemon{held: true})
	unsetForTest(t, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	codex := filepath.Join(t.TempDir(), "codex")
	saved := resolveCodex
	t.Cleanup(func() { resolveCodex = saved })
	resolveCodex = func(string) (string, error) { return codex, nil }
	savedChild := startChild
	t.Cleanup(func() { startChild = savedChild })
	startChild = func(launchSpec) (ExitCode, error) {
		t.Error("a pinned codex launch fell back and started codex anyway")
		return ExitOK, nil
	}

	code, _, errOut, top := runRoot(t, "run", "c@example.com")
	if code != ExitUsage {
		t.Fatalf("a pinned launch with no proxy = %d, want %d\n%s\n%s", code, ExitUsage, errOut, top)
	}
	said := errOut + top
	if !strings.Contains(said, "plain shell") {
		t.Errorf("the refusal does not say what to do instead:\n%s", said)
	}
	if !strings.Contains(said, "c@example.com") {
		t.Errorf("the refusal does not name the account:\n%s", said)
	}
}

// The never-cross control. A Claude account takes exactly the route it always
// did: no codex is resolved, no launch record is created, and claude is what
// runs.
func TestRunOnAClaudeAccountNeverEntersTheCodexLauncher(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-claude-1", "a@example.com")
	stub := stubClaude(t, ExitOK)
	saved := resolveCodex
	t.Cleanup(func() { resolveCodex = saved })
	resolveCodex = func(string) (string, error) {
		t.Error("`ccdad run` on a Claude account resolved codex")
		return "", errNoCodex
	}

	if code, _, errOut, top := runRoot(t, "run", "a@example.com"); code != ExitOK {
		t.Fatalf("run on a claude account = %d, want 0\n%s\n%s", code, errOut, top)
	}
	if !stub.started {
		t.Fatal("no claude was started")
	}
	for _, arg := range stub.spec.Args {
		if strings.HasPrefix(arg, "model_provider") {
			t.Errorf("claude was given a codex override: %q", stub.spec.Args)
		}
	}
}

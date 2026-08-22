package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// §9.1 writes the command as `run <ACCOUNT> [claude args…]`, so the account is
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

// §9.1: "everything at or after ACCT goes to claude verbatim, hyphens
// included". Cobra's default interspersed parsing refuses `-p` as an unknown
// shorthand before RunE is ever reached, so this is the test that pins
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

// §9.1: "A literal `--` immediately after ACCT is consumed and dropped."
// pflag does not do this for us — measured, a `--` after the first positional
// stays in args and ArgsLenAtDash() reports -1 — so it is stripped by hand, and
// exactly once. The second separator is a real argument of claude's.
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

// §5.1 forbids interactive disambiguation because "`ccdad run` ends in an exec
// and callers need determinism", and every other account-taking command turns a
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
// so the live login is not what decides who the session is. §3.3 blesses
// CLAUDE_SECURESTORAGE_CONFIG_DIR for this — it scopes credentials and their
// locks and nothing else.
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
// hygiene as the store: 0700 on the directory, 0600 on the file. §10.3 accepts
// that Windows has no mode bits and relies on the inherited profile ACL, which
// is why the assertion is unix-only.
func TestRunGivesTheSessionThePrivateModesTheStoreUses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("§10.3: no chmod on Windows; the inherited user-profile ACL is the v1 answer")
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
// env(1), nohup(1) and sudo(8) all follow. §9.3's closed table describes what
// CCDAD does; past the launch there is no ccdad left to describe.
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
// 130 for SIGINT is the value §9.3 already names, so a Ctrl-C'd session reads
// the same whether the shell reports it or ccdad does.
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

// §10.3: "`claude` is a `.cmd` shim | exec.LookPath honors PATHEXT, but `.cmd`
// targets go through cmd.exe with restrictive argument escaping — never pass a
// prompt on argv on Windows."
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
		path string
		args []string
		want string
	}{
		{"a native exe is not routed through cmd.exe", `C:\claude.exe`, []string{"fix&whoami"}, ""},
		{"a unix path is not either", "/usr/local/bin/claude", []string{"fix&whoami"}, ""},
		{"an ampersand chains a second command", `C:\claude.cmd`, []string{"-p", "fix&whoami"}, "fix&whoami"},
		{"a pipe does too", `C:\claude.cmd`, []string{"a|b"}, "a|b"},
		{"a redirect truncates a file", `C:\claude.cmd`, []string{"a>b"}, "a>b"},
		{"a caret escapes the next character", `C:\claude.cmd`, []string{"a^b"}, "a^b"},
		{"a percent expands a variable, even inside quotes", `C:\claude.cmd`, []string{"%PATH%"}, "%PATH%"},
		{"a quote toggles cmd.exe's quoting state", `C:\claude.cmd`, []string{`say "hi" there`}, `say "hi" there`},
		{"a newline ends the command line", `C:\claude.cmd`, []string{"a\nb"}, "a\nb"},
		{"the extension is matched case-insensitively", `C:\CLAUDE.CMD`, []string{"a&b"}, "a&b"},
		{"a .bat shim is the same shim", `C:\claude.BAT`, []string{"a&b"}, "a&b"},
		{"ordinary prompts pass", `C:\claude.cmd`, []string{"-p", "summarize this file", "--json"}, ""},
		{"the first offender is the one reported", `C:\claude.cmd`, []string{"ok", "a&b", "c|d"}, "a&b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unsafeForCmdShim(tc.path, tc.args); got != tc.want {
				t.Errorf("unsafeForCmdShim(%q, %q) = %q, want %q", tc.path, tc.args, got, tc.want)
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

// --full-profile is the other half of the §13 Q1 decision: a whole config home
// of its own rather than credentials alone, so the session keeps its MCP
// logins.
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

// The flag belongs to ccdad and is only ccdad's BEFORE the account. §9.1 gives
// everything at or after ACCT to claude, so the same spelling afterwards is a
// claude argument — surprising enough that it is pinned rather than left to be
// rediscovered as a bug report.
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
// §9.1's "everything at or after ACCT" rather than being a separate rule, and
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
	if err := s.Add(store.Account{UUID: uuid, Email: email}, cclink.Blob{cclink.TokenKey: rec}); err != nil {
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

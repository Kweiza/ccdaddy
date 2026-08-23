package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/daemon"
)

// refusalMarker is the one sentence every scoped-session refusal carries. The
// tests match on it rather than on a whole message so that the wording can be
// improved without rewriting the suite -- but it is matched EXACTLY, because a
// refusal a user cannot recognise is the failure mode this rule exists to fix.
const refusalMarker = "cannot run inside a 'ccdad run' session"

// unsetForTest removes a variable for one test and puts it back afterwards.
//
// There is no t.Unsetenv. The save-and-restore is registered BEFORE the unset
// so it runs after it, and t.Cleanup is LIFO -- so this restores the value
// isolate(t) set and isolate's own cleanup then restores the process's.
func unsetForTest(t *testing.T, name string) {
	t.Helper()
	saved, had := os.LookupEnv(name)
	t.Cleanup(func() {
		if had {
			os.Setenv(name, saved)
			return
		}
		os.Unsetenv(name)
	})
	os.Unsetenv(name)
}

// enterRunSession puts the test process where the Bash tool inside `ccdad run`
// is: the session's credential home exported, and the account's login already
// written into it the way seedSession writes it.
//
// The directory name follows os.MkdirTemp's shape -- the uuid, a hyphen, and a
// run of digits -- because that is what `run` produces and the guard has to
// recognise the real thing rather than a tidier stand-in.
func enterRunSession(t *testing.T, uuid string) string {
	t.Helper()
	home := filepath.Join(mustPath(ccpath.StoreHome()), SessionsDirName, uuid+"-1234567890")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ccpath.CredentialsFile),
		[]byte(`{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-`+uuid+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", home)
	return home
}

// enterFullProfileSession is the other half of the same shape. `run
// --full-profile` REMOVES the credential variable rather than emptying it and
// points CLAUDE_CONFIG_DIR at a persistent profile, so a guard that only reads
// CLAUDE_SECURESTORAGE_CONFIG_DIR sees nothing at all here.
func enterFullProfileSession(t *testing.T, uuid string) string {
	t.Helper()
	home := filepath.Join(mustPath(ccpath.StoreHome()), ProfilesDirName, uuid)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	unsetForTest(t, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	return home
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The reported bug, end to end: `ccdad switch other` typed inside a session
// rewrote <session>/.credentials.json, reported "Switched to", and left the
// live login exactly as it was -- so the user's next command outside the
// session was still the old account, and the session's own login had been
// replaced with a stranger's in a directory `run` deletes on the way out.
func TestSwitchInsideARunSessionIsRefusedAndLeavesTheSessionAlone(t *testing.T) {
	isolate(t)
	seedAccount(t, "acct-1", "a@example.com")
	seedAccount(t, "acct-2", "b@example.com")
	home := enterRunSession(t, "acct-1")
	path := filepath.Join(home, ccpath.CredentialsFile)
	before := readFile(t, path)

	code, _, stderr, top := runRoot(t, "switch", "acct-2")

	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d\nstderr: %s\ntop: %s", code, ExitUsage, stderr, top)
	}
	if !strings.Contains(top, refusalMarker) {
		t.Fatalf("the refusal does not say what it is:\n%s", top)
	}
	// "a message naming the session" is the item's own acceptance test: a user
	// who does not know they are inside one cannot act on anything less.
	if !strings.Contains(top, home) {
		t.Fatalf("the refusal does not name the session directory %s:\n%s", home, top)
	}
	if got := readFile(t, path); got != before {
		t.Fatalf("the session's credentials were rewritten\nbefore: %s\nafter:  %s", before, got)
	}
}

// One row per command that would act on Claude Code's own state, or outlive
// the session carrying its scope. The arguments are the shortest form that
// reaches the guard: cobra validates positional arguments BEFORE any
// PersistentPreRun hook, so a row with too few of them would be refused for
// the wrong reason and prove nothing.
func TestEveryMutatorIsRefusedInsideARunSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"switch", []string{"switch", "acct-2"}},
		{"auto", []string{"auto", "--once"}},
		{"add", []string{"add"}},
		{"add-token", []string{"add-token", "sk-ant-oat-test"}},
		{"remove", []string{"remove", "acct-1", "--yes"}},
		{"uninstall", []string{"uninstall", "--yes"}},
		{"daemon start", []string{"daemon", "start"}},
		{"daemon restart", []string{"daemon", "restart"}},
		{"daemon child", []string{daemon.RunArg}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			stubDaemonWorld(t, &fakeDaemon{})
			stubDaemonRun(t)
			seedAccount(t, "acct-1", "a@example.com")
			seedAccount(t, "acct-2", "b@example.com")
			home := enterRunSession(t, "acct-1")

			code, _, stderr, top := runRoot(t, tc.args...)

			if code != ExitUsage {
				t.Fatalf("exit = %d, want %d\nstderr: %s\ntop: %s", code, ExitUsage, stderr, top)
			}
			if !strings.Contains(top, refusalMarker) || !strings.Contains(top, home) {
				t.Fatalf("`ccdad %s` was refused for some other reason:\n%s", strings.Join(tc.args, " "), top)
			}
		})
	}
}

// The refusal is a rule about writing Claude Code's state, not a quarantine.
// A read is exactly what someone inside a session wants -- and its answer
// changes in there, which is the reason to keep it reachable rather than a
// reason to block it.
func TestReadsAndStoreOnlyCommandsStillRunInsideARunSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"list", []string{"list"}},
		{"which", []string{"which"}},
		{"status", []string{"status"}},
		{"doctor", []string{"doctor"}},
		{"alias", []string{"alias", "acct-1", "one"}},
		{"disable", []string{"disable", "acct-1"}},
		{"primary", []string{"primary", "a@example.com", "on"}},
		{"bootstrap", []string{"bootstrap"}},
		{"config list", []string{"config", "list"}},
		{"daemon status", []string{"daemon", "status"}},
		{"daemon logs", []string{"daemon", "logs"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			stubDaemonWorld(t, &fakeDaemon{})
			seedAccount(t, "acct-1", "a@example.com")
			enterRunSession(t, "acct-1")

			_, _, stderr, top := runRoot(t, tc.args...)

			if strings.Contains(top, refusalMarker) || strings.Contains(stderr, refusalMarker) {
				t.Fatalf("`ccdad %s` was refused inside a session:\n%s%s", strings.Join(tc.args, " "), top, stderr)
			}
		})
	}
}

// `run --full-profile` scopes with the OTHER variable, and removes the one the
// default mode sets. Reading only CLAUDE_SECURESTORAGE_CONFIG_DIR -- which is
// what autostart.go's rule 3 does -- sees nothing here at all.
func TestAFullProfileSessionIsRecognisedToo(t *testing.T) {
	isolate(t)
	seedAccount(t, "acct-1", "a@example.com")
	seedAccount(t, "acct-2", "b@example.com")
	home := enterFullProfileSession(t, "acct-1")

	code, _, stderr, top := runRoot(t, "switch", "acct-2")

	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d\nstderr: %s\ntop: %s", code, ExitUsage, stderr, top)
	}
	if !strings.Contains(top, home) {
		t.Fatalf("the refusal does not name the profile %s:\n%s", home, top)
	}
	// A profile is not deleted when the run ends, and telling the user it is
	// would send them looking for a directory that is still there. The two
	// modes have to be distinguishable in the message, or one description
	// stands in for both.
	if !strings.Contains(top, "--full-profile") {
		t.Fatalf("the refusal describes a profile as if it were an ephemeral session:\n%s", top)
	}
}

// The test is deliberately NOT "is the credential home scoped". A user who
// scopes their own shell has told ccdad where their live login is, and
// switching into it is the correct answer rather than the bug -- ccdad only
// knows better about the directories it created itself. This is the whole
// reason the guard is narrower than autostart.go's, and the assertion that
// stops a later reader from "simplifying" it back.
func TestAShellTheUserScopedThemselvesIsNotARunSession(t *testing.T) {
	isolate(t)
	seedAccount(t, "acct-1", "a@example.com")
	seedAccount(t, "acct-2", "b@example.com")
	// isolate already points the credential home outside the ccdad store, and
	// writes a login there so there is something to switch away from.
	writeLiveFile(t, `{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-acct-1"}}`)

	// By email rather than by uuid: store.Resolve matches a uuid by PREFIX and
	// "acct-2" is not one it accepts, so a uuid here would fail for a reason
	// that has nothing to do with what this test is about.
	code, _, stderr, top := runRoot(t, "switch", "b@example.com")

	if strings.Contains(top, refusalMarker) {
		t.Fatalf("a shell the user scoped for themselves was treated as a ccdad session:\n%s", top)
	}
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\nstderr: %s\ntop: %s", code, ExitOK, stderr, top)
	}
}

// The prefix trap, written down because it is the one every containment check
// gets wrong: "<store>/sessions-old" starts with "<store>/sessions" as a
// STRING and is a different directory as a PATH.
func TestASiblingOfTheSessionsContainerIsNotInsideIt(t *testing.T) {
	isolate(t)
	seedAccount(t, "acct-1", "a@example.com")
	seedAccount(t, "acct-2", "b@example.com")
	sibling := filepath.Join(mustPath(ccpath.StoreHome()), SessionsDirName+"-old", "acct-1-1")
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", sibling)
	writeLiveFile(t, `{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-acct-1"}}`)

	if _, _, _, top := runRoot(t, "switch", "acct-2"); strings.Contains(top, refusalMarker) {
		t.Fatalf("%s was treated as being inside the sessions container:\n%s", sibling, top)
	}
}

// The container itself is not a session. `run` never sets the variable to it,
// and a filepath.Rel of "." is the only thing that separates the two.
func TestTheSessionsContainerItselfIsNotASession(t *testing.T) {
	isolate(t)
	seedAccount(t, "acct-1", "a@example.com")
	seedAccount(t, "acct-2", "b@example.com")
	container := filepath.Join(mustPath(ccpath.StoreHome()), SessionsDirName)
	if err := os.MkdirAll(container, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", container)
	writeLiveFile(t, `{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-acct-1"}}`)

	if _, _, _, top := runRoot(t, "switch", "acct-2"); strings.Contains(top, refusalMarker) {
		t.Fatalf("the sessions container was treated as a session:\n%s", top)
	}
}

// The property autostart.go's allow-list gets from BEING an allow-list, on a
// rule whose natural shape is a deny-list: a command added later cannot
// default to "allowed inside a session" silently, because it has no verdict at
// all until someone writes one down.
func TestEveryCommandHasAScopedSessionVerdict(t *testing.T) {
	root := NewRootCmd()
	// Cobra adds `help` during Execute, so it does not exist on a tree that
	// was only built. Without this the one command the walk would miss is the
	// one nobody would think to classify.
	root.InitDefaultHelpCmd()

	var missing, both []string
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		path := c.CommandPath()
		_, refused := scopedSessionRefusals[path]
		allowed := scopedSessionAllowed[path]
		switch {
		case refused && allowed:
			both = append(both, path)
		case !refused && !allowed:
			missing = append(missing, path)
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)

	sort.Strings(missing)
	sort.Strings(both)
	if len(missing) > 0 {
		t.Errorf("no scoped-session verdict for: %s\n"+
			"Add each to scopedSessionRefusals (with the clause saying what it would get wrong) "+
			"or to scopedSessionAllowed (with the reason it is safe).", strings.Join(missing, ", "))
	}
	if len(both) > 0 {
		t.Errorf("classified twice: %s", strings.Join(both, ", "))
	}
}

// autostart.go's rule 3 reads CLAUDE_SECURESTORAGE_CONFIG_DIR and only that,
// so `run --full-profile` -- which REMOVES that variable and scopes with
// CLAUDE_CONFIG_DIR instead -- walked straight through it. A daemon started
// there is pinned to the profile by daemon.ChildEnv and manages it for the
// rest of its life.
func TestAutoStartRefusesInsideAFullProfileSession(t *testing.T) {
	isolate(t)
	f := stubDaemonWorld(t, &fakeDaemon{})
	enableAutoStart(t)
	enterFullProfileSession(t, "acct-1")

	runRoot(t, "list")
	if f.spawns != 0 {
		t.Fatalf("auto-started %d daemons inside a --full-profile session", f.spawns)
	}
}

// The control for the test above: a CLAUDE_CONFIG_DIR that is simply where the
// user keeps their Claude Code configuration is not a session, and refusing to
// auto-start there would disable the feature for everyone who sets it.
func TestAutoStartStillFiresWithAnOrdinaryConfigDirOverride(t *testing.T) {
	isolate(t)
	f := stubDaemonWorld(t, &fakeDaemon{})
	enableAutoStart(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	runRoot(t, "list")
	if f.spawns != 1 {
		t.Fatalf("spawned %d daemons with an ordinary CLAUDE_CONFIG_DIR override, want 1", f.spawns)
	}
}

// The item's third question, measured before it was asked: doctor reported the
// session's credentials file as though it were the live login, and listed the
// override without noticing it pointed into ccdad's own sessions container. A
// user running doctor to find out why a switch did nothing was told everything
// was fine.
//
// Asserted per CHECK rather than over the whole report, and that is not
// fussiness: two checks now carry this sentence, so a test that greps the
// rendered table passes with either one of them deleted. Each has to be pinned
// by the row it belongs to or only one of them is really tested.
func TestDoctorSaysWhichSessionItIsInside(t *testing.T) {
	isolate(t)
	seedAccount(t, "acct-1", "a@example.com")
	home := enterRunSession(t, "acct-1")

	_, stdout, _, _ := runRoot(t, "doctor", "--json")

	var report struct {
		Checks []struct {
			Name   string `json:"name"`
			Level  string `json:"level"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("doctor --json did not parse (%v):\n%s", err, stdout)
	}
	details := map[string]string{}
	levels := map[string]string{}
	for _, c := range report.Checks {
		details[c.Name] = c.Detail
		levels[c.Name] = c.Level
	}

	// The check that names the credentials file must not present a session's
	// file as the machine's login.
	if got := details["claude-code"]; !strings.Contains(got, "ccdad run") || !strings.Contains(got, home) {
		t.Errorf("the claude-code check calls a session's file the live login: %q", got)
	}
	// The check that lists the path overrides must say what this one IS.
	if got := details["environment"]; !strings.Contains(got, "ccdad run") || !strings.Contains(got, home) {
		t.Errorf("the environment check lists the override without saying it is a session: %q", got)
	}
	// Not "ok": a report that says everything is fine is exactly what sent the
	// user away last time.
	if got := levels["environment"]; got != string(levelWarn) {
		t.Errorf("the environment check is %q inside a session, want %q", got, levelWarn)
	}
}

// A read whose ANSWER changes inside a session is allowed to run, and has to
// stop making a claim it can no longer support. `export --include-mcp` is the
// only path by which mcpOAuth leaves the machine, and the default session
// mode scopes mcpOAuth away with the credentials — so in here the empty answer
// says nothing about the machine, and "there are no MCP logins on this
// machine" is how someone loses their backup.
func TestExportDoesNotClaimTheMachineHasNoMCPLoginsFromInsideASession(t *testing.T) {
	isolate(t)
	seedAccount(t, "acct-1", "a@example.com")
	home := enterRunSession(t, "acct-1")
	out := filepath.Join(t.TempDir(), "backup.json")

	_, _, stderr, top := runRoot(t, "export", "--full", "--include-mcp", "--out", out)

	if strings.Contains(stderr, "on this machine to include") {
		t.Fatalf("export spoke for the machine from inside a session:\n%s", stderr)
	}
	if !strings.Contains(stderr, home) {
		t.Fatalf("export never says which session it is speaking from:\n%s\n%s", stderr, top)
	}
}

// The review's finding, pinned. `ccdad run` is allow-listed inside a session
// on the grounds that it replaces the scope in the CHILD — which is true, and
// says nothing about what it reads in its own process. seedProfile resolves
// ccpath.ConfigHome() at call time, so creating a profile from inside a
// --full-profile session seeded it from the OUTER account's profile, carrying
// that account's primaryApiKey into this one's config: a credential in a file
// the user never named, at a path nothing lists and `ccdad remove` never
// cleans.
func TestANestedFullProfileRunWillNotSeedOneAccountsProfileFromAnothers(t *testing.T) {
	isolate(t)
	seedTokenAccount(t, "u-1", "a@example.com", cclink.APIKeyKind, "sk-ant-api-KEY-A")
	seedAccount(t, "u-2", "b@example.com")
	stubClaude(t, ExitOK)

	// Stand where the Bash tool inside `ccdad run --full-profile u-1` stands.
	outer := enterFullProfileSession(t, "u-1")
	if err := os.WriteFile(filepath.Join(outer, ccpath.GlobalConfigFile),
		[]byte(`{"primaryApiKey":"sk-ant-api-KEY-A"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, stderr, top := runRoot(t, "run", "--full-profile", "2")
	if code != ExitUsage {
		t.Fatalf("exit = %d (%s / %s), want %d", code, stderr, top, ExitUsage)
	}
	if got := top + stderr; !strings.Contains(got, "inside ccdad's own store") {
		t.Errorf("the refusal does not say what is wrong with the source: %q", got)
	}

	// And the half that turns one refusal into a permanent defect: the profile
	// directory is created before the seed, and its existence is what tells
	// the NEXT run not to seed. Left behind, u-2's profile would be silently
	// unseeded forever.
	inner := filepath.Join(mustPath(ccpath.StoreHome()), ProfilesDirName, "u-2")
	if _, err := os.Stat(inner); !os.IsNotExist(err) {
		t.Fatalf("%s was left behind after the seed was refused (stat err: %v); the next run would skip seeding it", inner, err)
	}
}

// The narrowness of that refusal, which is the half that keeps it usable: a
// profile that already exists is never re-seeded, so nothing needs to be read
// and a nested run of an account that has been run before still works.
func TestANestedRunStillWorksForAProfileThatAlreadyExists(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")
	stub := stubClaude(t, ExitOK)
	if err := os.MkdirAll(filepath.Join(mustPath(ccpath.StoreHome()), ProfilesDirName, "u-2"), 0o700); err != nil {
		t.Fatal(err)
	}
	enterFullProfileSession(t, "u-1")

	if code, _, stderr, top := runRoot(t, "run", "--full-profile", "2"); code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, stderr, top)
	}
	if !stub.started {
		t.Error("no session was started for a profile that needed no seeding")
	}
}

// A DEFAULT-mode session leaves CLAUDE_CONFIG_DIR alone, so ConfigHome is the
// machine's and seeding is correct. Refusing there would break the ordinary
// nested run for no reason — which is what a check written on "am I in a
// session" rather than "is my source inside the store" would have done.
func TestANestedRunSeedsNormallyFromInsideADefaultModeSession(t *testing.T) {
	claude := isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")
	stub := stubClaude(t, ExitOK)
	if err := os.WriteFile(filepath.Join(claude, "settings.json"), []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	enterRunSession(t, "u-1")

	if code, _, stderr, top := runRoot(t, "run", "--full-profile", "2"); code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, stderr, top)
	}
	profile, _ := envOf(stub.spec.Env, "CLAUDE_CONFIG_DIR")
	if _, err := os.Stat(filepath.Join(profile, "settings.json")); err != nil {
		t.Fatalf("the profile was not seeded from the machine's config home: %v", err)
	}
}

// The review's second finding. The session note landed on the arm that says
// "there are none", and not on the arm that actually WRITES a secret into the
// export and calls it the machine's.
func TestExportSaysWhoseMCPLoginsItIsCarryingFromInsideASession(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	home := enterRunSession(t, "u-1")
	// What Claude Code leaves the moment one MCP server is authenticated
	// inside the session: run.go says mcpOAuth lives in .credentials.json
	// under the credential root, which in here is the session.
	if err := os.WriteFile(filepath.Join(home, ccpath.CredentialsFile),
		[]byte(`{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT"},`+
			`"mcpOAuth":{"srv":{"accessToken":"SESSION-MCP-SECRET"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "backup.json")

	_, _, stderr, _ := runRoot(t, "export", "--full", "--include-mcp", "--out", out)

	if strings.Contains(stderr, "this machine's MCP server logins") {
		t.Fatalf("the export called the session's MCP logins the machine's:\n%s", stderr)
	}
	if !strings.Contains(stderr, home) {
		t.Fatalf("the export never says which session it spoke for:\n%s", stderr)
	}
	// The secret really is in the file — this is a labelling defect, not a
	// leak, and a test that did not confirm the write would pass against an
	// export that had silently stopped carrying anything.
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "SESSION-MCP-SECRET") {
		t.Fatal("the export carried no MCP logins at all, so the warning under test never fired")
	}
}

// doctor's "no login" branch carries the same scope note as its OK branch, and
// nothing reached it: an API-key account under --full-profile has no
// credentials file at all, which is exactly that branch.
func TestDoctorNamesTheSessionEvenWhenThereIsNoLoginInIt(t *testing.T) {
	isolate(t)
	seedTokenAccount(t, "u-1", "a@example.com", cclink.APIKeyKind, "sk-ant-api-XYZ")
	home := enterFullProfileSession(t, "u-1")

	_, stdout, _, _ := runRoot(t, "doctor", "--json")

	var report struct {
		Checks []struct {
			Name   string `json:"name"`
			Level  string `json:"level"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	for _, c := range report.Checks {
		if c.Name != "claude-code" {
			continue
		}
		if !strings.Contains(c.Detail, "no login") {
			t.Fatalf("this fixture no longer reaches the no-login branch: %q", c.Detail)
		}
		if !strings.Contains(c.Detail, "ccdad run") || !strings.Contains(c.Detail, home) {
			t.Fatalf("the no-login branch does not say the directory is a session: %q", c.Detail)
		}
		return
	}
	t.Fatal("doctor emitted no claude-code check")
}

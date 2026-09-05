package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// stubExecutable points the uninstaller at a binary of the test's own, so that
// a test of "delete the binary" does not delete the test binary.
//
// It also holds the HKLM seam, because on Windows "delete the binary" is a
// rename plus a MOVEFILE_DELAY_UNTIL_REBOOT that writes the MACHINE's
// PendingFileRenameOperations. Every test below reaches that line on the
// windows-latest leg, so without the quarantine `go test ./...` queues one
// reboot-time delete per test into the registry of whatever machine ran it.
// uninstall_windows_test.go is where the real call is exercised, once, and
// takes its own entry back.
func stubExecutable(t *testing.T, path string) string {
	t.Helper()
	quarantineDelayedDelete(t)
	saved := executablePath
	t.Cleanup(func() { executablePath = saved })
	executablePath = func() (string, error) { return path, nil }
	return path
}

// fakeBinary is an executable file somewhere harmless.
func fakeBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ccdad")
	if err := os.WriteFile(path, []byte("#!/bin/false\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return stubExecutable(t, path)
}

func accountsFilePath() string { return filepath.Join(mustPath(ccpath.StoreHome()), "accounts.toml") }

func storeIsThere(t *testing.T) bool {
	t.Helper()
	_, err := os.Stat(accountsFilePath())
	return err == nil
}

// Both installers point at this command, and it deletes the live OAuth refresh
// token of every managed account. It has to say what it is about to destroy
// before it asks.
func TestUninstallEnumeratesBeforeAsking(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	stubEnvironment(t, true, false)
	bin := fakeBinary(t)
	seedAccount(t, "u-1", "work@example.com")
	seedAccount(t, "u-2", "home@example.com")

	cmd := newUninstallCmd()
	cmd.SetIn(strings.NewReader("n\n"))
	err, _, errOut := runCmd(t, cmd)

	if CodeFor(err) != ExitNothingToDo {
		t.Fatalf("answering no = %d, want %d", CodeFor(err), ExitNothingToDo)
	}
	for _, want := range []string{"work@example.com", "home@example.com", mustPath(ccpath.StoreHome()), bin, "2 account"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the enumeration does not mention %q:\n%s", want, errOut)
		}
	}
	if !storeIsThere(t) {
		t.Fatal("answering no deleted the store anyway")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatal("answering no deleted the binary anyway")
	}
}

// remove.go's exact discipline: a destructive command with no terminal to
// confirm at must be told explicitly, or a script deletes credentials nobody
// meant to lose. Usage error, so cron can tell it from a no-op.
func TestUninstallWithoutATTYAndWithoutYesIsAUsageError(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	stubEnvironment(t, false, false)
	fakeBinary(t)
	seedAccount(t, "u-1", "work@example.com")

	code, _, _, top := runRoot(t, "uninstall")
	if code != ExitUsage {
		t.Fatalf("uninstall without a TTY = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(top, "--yes") {
		t.Errorf("the refusal does not say how to proceed: %q", top)
	}
	if !storeIsThere(t) {
		t.Fatal("the store was deleted despite the refusal")
	}
}

func TestUninstallWithYesRemovesTheStoreAndTheBinary(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	stubEnvironment(t, false, false)
	bin := fakeBinary(t)
	seedAccount(t, "u-1", "work@example.com")

	code, _, errOut, top := runRoot(t, "uninstall", "--yes")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s%s", code, ExitOK, errOut, top)
	}
	if _, err := os.Stat(mustPath(ccpath.StoreHome())); !os.IsNotExist(err) {
		t.Errorf("the store survived: %v", err)
	}
	assertBinaryGone(t, bin)
}

// assertBinaryGone is "uninstall removed the binary", spelled for each
// platform's version of removing it.
//
// A running .exe cannot be deleted on Windows, so removeSelf renames it aside
// and asks the kernel to delete the leftover at the next restart. The rename is
// the step that matters there, and asserting the path is simply gone would
// either fail or — worse — pass for the wrong reason once the leftover is
// cleaned up by something else.
func assertBinaryGone(t *testing.T, bin string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		if _, err := os.Stat(bin); err == nil {
			t.Errorf("the binary is still at its own path; on Windows it has to be renamed aside")
		}
		return
	}
	if _, err := os.Stat(bin); !os.IsNotExist(err) {
		t.Errorf("the binary survived: %v", err)
	}
}

// CCDAD_HOME can point ANYWHERE, and ccpath.StoreHome returns the raw value.
// RemoveAll on a bare environment variable is how a home directory disappears.
func TestUninstallRefusesAStoreThatIsNotOne(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	stubEnvironment(t, false, false)
	fakeBinary(t)

	// A directory that exists and holds something precious, but is not a ccdad
	// store: no accounts.toml at its top level.
	elsewhere := t.TempDir()
	decoy := filepath.Join(elsewhere, "the-users-photos")
	if err := os.WriteFile(decoy, []byte("do not delete"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCDAD_HOME", elsewhere)

	code, _, _, top := runRoot(t, "uninstall", "--yes")
	if code != ExitFailure {
		t.Fatalf("exit = %d, want %d", code, ExitFailure)
	}
	if !strings.Contains(top, "accounts.toml") {
		t.Errorf("the refusal does not name what was missing: %q", top)
	}
	if !strings.Contains(top, elsewhere) {
		t.Errorf("the refusal does not name the directory it declined: %q", top)
	}
	if _, err := os.Stat(decoy); err != nil {
		t.Fatalf("uninstall deleted a directory that was not a ccdad store: %v", err)
	}
}

// A relative CCDAD_HOME is the same hazard as a store that is not one, one step
// earlier: os.RemoveAll of a relative path deletes whatever sits beside the
// working directory, and a different directory on every run.
func TestUninstallRefusesARelativeStore(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	stubEnvironment(t, false, false)
	bin := fakeBinary(t)
	t.Setenv("CCDAD_HOME", "relative-store")

	code, _, _, top := runRoot(t, "uninstall", "--yes")
	if code != ExitFailure {
		t.Fatalf("exit = %d, want %d", code, ExitFailure)
	}
	if !strings.Contains(top, "relative-store") {
		t.Errorf("the refusal does not name the path: %q", top)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatal("uninstall removed the binary despite refusing to touch the store")
	}
}

// A machine where ccdad ran but never stored an account still has a store —
// store.Open creates the credentials directory on the first command — and
// refusing to remove it would fail an uninstall on a machine with nothing in it.
// The marker set is every top-level entry ccdad creates, not accounts.toml
// alone.
func TestUninstallRemovesAStoreWithNoAccountsYet(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	stubEnvironment(t, false, false)
	fakeBinary(t)
	// What the first `ccdad status` on a fresh machine leaves behind, and nothing
	// else: no accounts.toml, because nothing has been saved.
	if err := os.MkdirAll(filepath.Join(mustPath(ccpath.StoreHome()), "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}

	code, _, errOut, top := runRoot(t, "uninstall", "--yes")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s%s", code, ExitOK, errOut, top)
	}
	if _, err := os.Stat(mustPath(ccpath.StoreHome())); !os.IsNotExist(err) {
		t.Errorf("the store survived: %v", err)
	}
}

// The series is a top-level entry nothing but ccdad writes, so it is evidence
// the directory is a ccdad store — and it can be the only one left. accounts.toml
// is absent until an account is saved, and a machine whose daemon has since been
// stopped has no lock, pid, log or status file either. A marker set that omitted
// it would refuse such a directory as "not a ccdad store" and leave it behind
// with nothing else in the tree that ever cleans it up.
//
// The name is hand-spelled here on purpose. The command reads it from the owning
// package's exported basename, and a test that did the same would agree with the
// implementation by construction rather than check it.
func TestUninstallRemovesAStoreWhoseOnlyMarkerIsTheHistory(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	stubEnvironment(t, false, false)
	fakeBinary(t)
	root := mustPath(ccpath.StoreHome())
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "history.json"), []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, errOut, top := runRoot(t, "uninstall", "--yes")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s%s", code, ExitOK, errOut, top)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("the store survived: %v", err)
	}
}

// Nothing to uninstall is 3, not 1: a second `ccdad uninstall` in a script is a
// no-op rather than a failure.
func TestUninstallOnAMachineWithNoStoreIsExitThree(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	stubEnvironment(t, false, false)
	// A binary the package manager owns, so there is nothing this command may
	// remove at all.
	t.Setenv("HOMEBREW_PREFIX", "/opt/homebrew")
	stubExecutable(t, "/opt/homebrew/bin/ccdad")

	code, _, _, _ := runRoot(t, "uninstall", "--yes")
	if code != ExitNothingToDo {
		t.Fatalf("exit = %d, want %d", code, ExitNothingToDo)
	}
}

// The daemon holds the singleton and rewrites status.json every second. Deleting
// the store underneath it leaves a live process writing into a directory that no
// longer exists — and on Windows its open handle blocks the removal outright.
func TestUninstallStopsTheDaemonBeforeDeletingTheStore(t *testing.T) {
	isolate(t)
	f := stubDaemonWorld(t, &fakeDaemon{held: true, pid: 4321, pidOK: true})
	stubEnvironment(t, false, false)
	fakeBinary(t)
	seedAccount(t, "u-1", "work@example.com")

	stillThere := false
	f.onShutdown = func() { stillThere = storeIsThere(t) }

	code, _, errOut, top := runRoot(t, "uninstall", "--yes")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s%s", code, ExitOK, errOut, top)
	}
	if len(f.signalled) != 1 || f.signalled[0] != 4321 {
		t.Fatalf("signalled %v, want exactly [4321]", f.signalled)
	}
	if !stillThere {
		t.Fatal("the store was already gone when the daemon was asked to stop")
	}
}

// A daemon that will not go is a reason to stop, not to delete the store out
// from under it.
func TestUninstallRefusesWhenTheDaemonWillNotStop(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{held: true, pid: 4321, pidOK: true, releaseAfter: 1 << 30})
	stubEnvironment(t, false, false)
	fakeBinary(t)
	seedAccount(t, "u-1", "work@example.com")

	code, _, _, _ := runRoot(t, "uninstall", "--yes")
	if code != ExitFailure {
		t.Fatalf("exit = %d, want %d", code, ExitFailure)
	}
	if !storeIsThere(t) {
		t.Fatal("the store was deleted while a daemon still held the singleton")
	}
}

func TestUninstallRefusesWhenTheLockCannotBeProbed(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{probeErr: errors.New("ENOLCK")})
	stubEnvironment(t, false, false)
	fakeBinary(t)
	seedAccount(t, "u-1", "work@example.com")

	code, _, _, _ := runRoot(t, "uninstall", "--yes")
	if code != ExitFailure {
		t.Fatalf("exit = %d, want %d", code, ExitFailure)
	}
	if !storeIsThere(t) {
		t.Fatal("the store was deleted without knowing whether a daemon was writing to it")
	}
}

// Uninstalling ccdad is not logging out of Claude Code. The live credentials
// file is left exactly as it is — and the user is told which account they are
// left on, because every OTHER account's refresh token has just been destroyed.
func TestUninstallLeavesTheLiveLoginAloneAndSaysWhichItIs(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	stubEnvironment(t, false, false)
	fakeBinary(t)
	seedAccount(t, "u-1", "work@example.com")
	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("switch = %d (%s)", code, top)
	}

	code, _, errOut, top := runRoot(t, "uninstall", "--yes")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitOK, top)
	}
	if _, err := os.Stat(mustPath(ccpath.CredentialsPath())); err != nil {
		t.Fatalf("uninstall touched Claude Code's own credentials file: %v", err)
	}
	// The CLOSING notice specifically, not just the label anywhere in the
	// output: the enumeration above the prompt already names every account, so
	// a test that only looked for the address would pass with the "what
	// happened" line deleted — and that line is the one a user reads after a
	// destructive command finishes.
	if !strings.Contains(errOut, "still logged in to Claude Code as work@example.com") {
		t.Errorf("the user is not told, on the way out, which account they are left logged in as:\n%s", errOut)
	}
}

// A Homebrew or Scoop binary belongs to the package manager. Deleting it leaves
// the manager believing ccdad is installed, and its next upgrade reinstates a
// binary the user thought they removed.
func TestUninstallRefusesToDeleteAPackageManagerBinary(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	stubEnvironment(t, false, false)
	seedAccount(t, "u-1", "work@example.com")
	brew := filepath.Join(t.TempDir(), "homebrew")
	bin := filepath.Join(brew, "bin", "ccdad")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("brewed"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOMEBREW_PREFIX", brew)
	stubExecutable(t, bin)

	code, _, errOut, top := runRoot(t, "uninstall", "--yes")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s%s", code, ExitOK, errOut, top)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("uninstall deleted a Homebrew-owned binary: %v", err)
	}
	if !strings.Contains(errOut, "Homebrew") {
		t.Errorf("the user is not told why the binary is still there:\n%s", errOut)
	}
	if _, err := os.Stat(mustPath(ccpath.StoreHome())); !os.IsNotExist(err) {
		t.Error("refusing the binary also stopped the store being removed")
	}
}

func TestPackageManagerOwning(t *testing.T) {
	cases := []struct {
		name string
		exe  string
		env  map[string]string
		want string
	}{
		{name: "a homebrew prefix from the environment", exe: "/opt/brew/bin/ccdad",
			env: map[string]string{"HOMEBREW_PREFIX": "/opt/brew"}, want: "Homebrew"},
		{name: "the default apple silicon prefix", exe: "/opt/homebrew/bin/ccdad", want: "Homebrew"},
		{name: "an intel cellar", exe: "/usr/local/Cellar/ccdad/1.0.0/bin/ccdad", want: "Homebrew"},
		{name: "linuxbrew", exe: "/home/linuxbrew/.linuxbrew/bin/ccdad", want: "Homebrew"},
		{name: "a scoop shim", exe: `C:\Users\someone\scoop\shims\ccdad.exe`, want: "Scoop"},
		{name: "a scoop app", exe: `C:\Users\someone\scoop\apps\ccdad\current\ccdad.exe`, want: "Scoop"},
		{name: "a scoop prefix from the environment", exe: `D:\tools\scoopdir\shims\ccdad.exe`,
			env: map[string]string{"SCOOP": `D:\tools\scoopdir`}, want: "Scoop"},
		{name: "what install.sh writes", exe: "/usr/local/bin/ccdad", want: ""},
		{name: "what go install writes", exe: "/home/someone/go/bin/ccdad", want: ""},
		// The prefix test has to compare whole SEGMENTS. A bare HasPrefix says
		// yes to this one, and the user is then told Homebrew owns a binary it
		// has never heard of — and the binary is never removed.
		{name: "a directory whose name merely starts like a prefix", exe: "/opt/homebrewery/bin/ccdad", want: ""},
		{name: "the same trap on the environment prefix", exe: "/opt/brewery/bin/ccdad",
			env: map[string]string{"HOMEBREW_PREFIX": "/opt/brew"}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := packageManagerOwning(tc.exe); got != tc.want {
				t.Errorf("packageManagerOwning(%q) = %q, want %q", tc.exe, got, tc.want)
			}
		})
	}
}

// The MCP registration is deferred out of this queue, so the hook finds nothing
// today. When it exists, a failure to unwire it must not strand a user with a
// store already deleted and a command that exited non-zero.
func TestUninstallWarnsButFinishesWhenTheMCPUnwireFails(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	stubEnvironment(t, false, false)
	fakeBinary(t)
	seedAccount(t, "u-1", "work@example.com")

	saved := unwireMCP
	t.Cleanup(func() { unwireMCP = saved })
	unwireMCP = func() (bool, error) { return false, errors.New("the plugin file is read-only") }

	code, _, errOut, top := runRoot(t, "uninstall", "--yes")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitOK, top)
	}
	if !strings.Contains(errOut, "read-only") {
		t.Errorf("the unwire failure was swallowed:\n%s", errOut)
	}
	if _, err := os.Stat(mustPath(ccpath.StoreHome())); !os.IsNotExist(err) {
		t.Error("the store survived a failure that must not stop the uninstall")
	}
}

// A parallel session's credentials live inside the store, so uninstall takes
// them with it — including one belonging to a `ccdad run` that is still going.
// It does not refuse: uninstall is a deliberate act and refusing would strand
// someone whose session is wedged. It says so, because "Removed <store>" alone
// does not tell a user their running session just lost its login.
func TestUninstallSaysWhenItIsTakingLiveSessionsWithIt(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	stubEnvironment(t, false, false)
	fakeBinary(t)
	seedAccount(t, "u-1", "a@example.com")

	session := filepath.Join(mustPath(ccpath.StoreHome()), SessionsDirName, "u-1-abc")
	if err := os.MkdirAll(session, 0o700); err != nil {
		t.Fatal(err)
	}

	code, stdout, errOut, top := runRoot(t, "uninstall", "--yes")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if got := stdout + errOut; !strings.Contains(got, "session") {
		t.Errorf("uninstall removed a session's credentials without saying so:\n%s", got)
	}
}

// The store is identified by what ccdad puts in it, and a store whose accounts
// have been removed but which still holds a session directory is still a ccdad
// store — refusing to uninstall it would leave a directory nothing else can
// clean up.
func TestUninstallRecognisesAStoreThatOnlyHoldsSessions(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	stubEnvironment(t, false, false)
	fakeBinary(t)

	root := t.TempDir()
	t.Setenv("CCDAD_HOME", root)
	if err := os.MkdirAll(filepath.Join(root, SessionsDirName, "u-1-abc"), 0o700); err != nil {
		t.Fatal(err)
	}

	code, _, errOut, top := runRoot(t, "uninstall", "--yes")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0 — a sessions directory is ccdad's own", code, errOut, top)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("the store survived (err = %v)", err)
	}
}

// The installer that wrote the entry recorded where it put it, so the uninstall
// does not have to guess -- and guessing is the failure that matters here,
// because a guess that looks in the wrong file reports a clean machine while
// the real entry survives and prints a connect failure in every session.
//
// This drives `ccdad uninstall` end to end rather than calling unwireMCP()
// directly, and that distinction is the point: a unit test of the hook is
// structurally blind to a bug in its CALLER, and the caller had one.
func TestUninstallRemovesTheRegistrationFromTheScopeItWasInstalledIn(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	fakeBinary(t)
	stubCcdadOnPath(t, true)
	seedAccount(t, "uuid-aaaa-0001", "work@example.com")
	if code, _, stderr, top := runRoot(t, "mcp", "install"); code != ExitOK {
		t.Fatalf("install = %d (%s%s)", code, stderr, top)
	}

	code, _, stderr, top := runRoot(t, "uninstall", "--yes")
	if code != ExitOK {
		t.Fatalf("uninstall = %d (%s%s)", code, stderr, top)
	}
	if !strings.Contains(stderr, "Removed ccdad's Claude Code registration") {
		t.Errorf("uninstall did not report removing the registration:\n%s", stderr)
	}
	if countCcdadServers(t) != 0 {
		t.Error("the entry survived a real `ccdad uninstall`")
	}
}

// The scope that exposes the ordering bug, and it only exposes it from a
// DIFFERENT directory than the install was run in.
//
// A project-scope entry lives in <cwd>/.mcp.json, and the record is what
// remembers which directory that was. If the store were deleted before the
// record is read, unwireMCP would fall back to scanning -- and the project leg
// of that scan reads the directory the user happens to be standing in, which is
// not the one the entry is in. It would find nothing, report a clean machine,
// and leave the entry behind. Running the uninstall from the same directory
// cannot tell the two implementations apart, because the fallback would find it
// there by luck.
func TestUninstallRemovesAProjectScopeRegistrationFromAnotherDirectory(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	fakeBinary(t)
	stubCcdadOnPath(t, true)
	seedAccount(t, "uuid-aaaa-0001", "work@example.com")

	project := t.TempDir()
	t.Chdir(project)
	if code, _, stderr, top := runRoot(t, "mcp", "install", "--scope", "project"); code != ExitOK {
		t.Fatalf("install --scope project = %d (%s%s)", code, stderr, top)
	}
	// Somewhere else entirely, which is where a user uninstalling from their
	// home directory actually is.
	t.Chdir(t.TempDir())

	code, _, stderr, top := runRoot(t, "uninstall", "--yes")
	if code != ExitOK {
		t.Fatalf("uninstall = %d (%s%s)", code, stderr, top)
	}
	if data, err := os.ReadFile(filepath.Join(project, ".mcp.json")); err == nil {
		t.Errorf("the project-scope entry survived an uninstall run from another directory: %s", data)
	}
	if !strings.Contains(stderr, "Removed ccdad's Claude Code registration") {
		t.Errorf("uninstall did not report removing the registration:\n%s", stderr)
	}
}

// No record is the machine this will actually meet: somebody registered the
// server with a ccdad old enough not to have recorded it, or their store was
// cleared under them. Scan all three, remove what is there, and never fail --
// the caller is midway through deleting the store and a hard error here strands
// the user.
//
// All three scopes, one subtest each, and that is not thoroughness for its own
// sake: the three legs read two different files and one of them is keyed to the
// current directory, so a scan that quietly dropped a leg would pass a test
// that only ever seeded the user scope. Measured -- with the project leg
// removed, a single-scope version of this test stayed green.
func TestWithNoRecordItScansEveryScopeAndStillNeverFails(t *testing.T) {
	for _, scope := range []string{"user", "local", "project"} {
		t.Run(scope, func(t *testing.T) {
			isolate(t)
			stubCcdadOnPath(t, true)
			t.Chdir(t.TempDir())
			if code, _, stderr, top := runRoot(t, "mcp", "install", "--scope", scope); code != ExitOK {
				t.Fatalf("install --scope %s = %d (%s%s)", scope, code, stderr, top)
			}
			// The record, gone. Everything else is exactly as the installer
			// left it, which is the state an upgrade from an older ccdad is in.
			if err := deleteMCPRecord(); err != nil {
				t.Fatal(err)
			}

			removed, err := unwireMCP()
			if err != nil {
				t.Fatalf("unwireMCP() with no record returned an error: %v", err)
			}
			if !removed {
				t.Errorf("the scan did not reach a --scope %s entry", scope)
			}
		})
	}
}

// An entry somebody typed themselves, and the half of the scan that has to be
// careful: it is removing one key out of a file that belongs to the user.
func TestTheScanRemovesAHandWrittenEntryAndNobodyElsesKeys(t *testing.T) {
	isolate(t)
	writeGlobalConfigFixture(t,
		`{"numStartups":7,"mcpServers":{"other":{"type":"stdio","command":"x"},`+
			`"ccdad":{"type":"stdio","command":"ccdad","args":["mcp"],"env":{}}}}`)

	removed, err := unwireMCP()
	if err != nil {
		t.Fatalf("unwireMCP() over a hand-written entry returned an error: %v", err)
	}
	if !removed {
		t.Error("a hand-written entry was not found by the scan")
	}
	if countCcdadServers(t) != 0 {
		t.Error("the hand-written entry survived the scan")
	}
	raw := compactJSON(t, readGlobalConfigFixture(t))
	for _, want := range []string{`"numStartups":7`, `"other"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("the scan took %s with it:\n%s", want, raw)
		}
	}
}

// Nothing anywhere is not an error either, and it is not a change.
func TestOnAMachineWithNoRegistrationTheUnwireIsSilentAndClean(t *testing.T) {
	isolate(t)
	removed, err := unwireMCP()
	if err != nil || removed {
		t.Fatalf("unwireMCP() = %v, %v; want false, nil on a machine with no entry", removed, err)
	}
}

// installedCodexShim puts a real codex shim on an otherwise bare machine: no
// startup file of any kind, a binary this command may delete, and a $SHELL
// whose dialect the PATH block is written in.
//
// It runs the real `ccdad codex shim install` rather than writing the script,
// the record and the block by hand. What the tests below ask is whether a
// REMOVAL reaches everything an install left, and a removal checked against a
// hand-built fixture only proves that the fixture was built to match it.
//
// It returns the startup file the install creates, which on a home directory
// with nothing in it is bash's ~/.bashrc.
func installedCodexShim(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("ccdad installs no shim on Windows, so there is nothing here to take back")
	}
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("PATH", "/usr/bin:/bin")
	// packageManagerOwning reads both of these, and a developer running the
	// suite under Homebrew would otherwise get the refusal branch where CI gets
	// the removal.
	t.Setenv("HOMEBREW_PREFIX", "")
	t.Setenv("SCOOP", "")
	fakeBinary(t)

	if code, _, errOut, top := runRoot(t, "codex", "shim", "install"); code != ExitOK {
		t.Fatalf("codex shim install = %d, want %d\n%s%s", code, ExitOK, errOut, top)
	}
	if _, ok := shimRecord(); !ok {
		t.Fatal("the install wrote no record, so these tests would be describing a machine with no shim")
	}
	return filepath.Join(os.Getenv("HOME"), ".bashrc")
}

// `ccdad add codex` installs the shim without being asked, so this enumeration
// is the only place a user who never ran `ccdad codex shim install` is told
// that a file called `codex` sits on their PATH at all -- and they are told it
// at the moment it is about to go. A confirmation prompt that does not name
// what it destroys is a prompt people say yes to.
//
// The line is keyed on the RECORD, which is what both subtests are for. The
// record is what says this machine wants <CCDAD_HOME>/bin on PATH --
// setup-path's derived directory set reads it and nothing else -- and a script
// somebody deleted by hand is something they have already said something
// about. Keying on the script would go silent on exactly the machine whose
// record and PATH entry are still there and still being removed.
func TestUninstallNamesTheCodexShimItIsTakingBack(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dropScript bool
	}{
		{name: "with the script on disk"},
		{name: "with the record but no script", dropScript: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installedCodexShim(t)
			if tc.dropScript {
				if err := os.Remove(shimPath()); err != nil {
					t.Fatal(err)
				}
			}
			stubEnvironment(t, true, false)

			cmd := newUninstallCmd()
			cmd.SetIn(strings.NewReader("n\n"))
			err, _, errOut := runCmd(t, cmd)
			if CodeFor(err) != ExitNothingToDo {
				t.Fatalf("answering no = %d, want %d\n%s", CodeFor(err), ExitNothingToDo, errOut)
			}
			if !strings.Contains(errOut, shimPath()) {
				t.Errorf("the enumeration does not name the codex shim at %s:\n%s", shimPath(), errOut)
			}
		})
	}
}

// The removal itself is already there -- "bin" and "codex-shim.json" are store
// markers, os.RemoveAll takes both, and unregisterPath removes the whole fenced
// block -- and "already there" is the claim worth checking rather than
// repeating. All three, because they go by three different mechanisms: the
// script goes with the directory tree, the record goes with it as a sibling,
// and the PATH entry is a rewrite of a file outside the store entirely.
func TestUninstallTakesTheShimTheRecordAndThePathEntry(t *testing.T) {
	rc := installedCodexShim(t)
	before, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(before), shimDir()) {
		t.Fatalf("%s does not register %s, so this test would prove nothing:\n%s", rc, shimDir(), before)
	}

	code, _, errOut, top := runRoot(t, "uninstall", "--yes")
	if code != ExitOK {
		t.Fatalf("uninstall = %d, want %d\n%s%s", code, ExitOK, errOut, top)
	}
	for _, path := range []string{shimPath(), shimRecordPath(), shimDir()} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s survived the uninstall (stat err = %v)", path, err)
		}
	}
	after, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), setupPathBegin) || strings.Contains(string(after), shimDir()) {
		t.Errorf("%s still carries ccdad's PATH block:\n%s", rc, after)
	}
}

// THE LIMIT, written down rather than repaired: a startup file ccdad itself
// created is left behind EMPTY. removeRC writes zero bytes over it, and
// os.Remove is never called on a startup file anywhere in this tree.
//
// Deleting it would be worse, and not marginally. Nothing on the machine
// records which startup files ccdad created -- writeRC returns the flag that
// prints "Created" for the person watching and writes it down nowhere, and the
// one place a note could have been kept, the store, is deleted several steps
// before unregisterPath runs. So the only evidence available at removal time
// is that the file is now empty, and an empty startup file is evidence of
// nothing: a user who ran `touch ~/.zshrc` to silence a first-run prompt, and
// one whose dotfiles repository tracks an empty one, have exactly the same
// file.
//
// The two mistakes also do not cost the same. An empty ~/.bashrc costs its
// owner nothing -- the shell reads it and does nothing -- while an unlink takes
// a path other things point at: replaceFile resolves symlinks precisely because
// these files are so often a stow or chezmoi checkout, and removing the link
// leaves that repository with an entry no ccdad command can put back.
func TestUninstallLeavesAStartupFileItCreatedBehindEmpty(t *testing.T) {
	installedCodexShim(t)
	home := os.Getenv("HOME")

	code, _, errOut, top := runRoot(t, "uninstall", "--yes")
	if code != ExitOK {
		t.Fatalf("uninstall = %d, want %d\n%s%s", code, ExitOK, errOut, top)
	}
	// Both files bash gets, because both were created by the install and the
	// argument above is about neither one in particular.
	for _, name := range []string{".bashrc", ".profile"} {
		path := filepath.Join(home, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("%s was deleted; a startup file ccdad created is left in place, empty: %v", path, err)
			continue
		}
		if info.Size() != 0 {
			body, _ := os.ReadFile(path)
			t.Errorf("%s is %d bytes after the removal, not empty:\n%s", path, info.Size(), body)
		}
	}
}

// The other half of the same line: a machine that never installed a shim is
// told nothing about one. Without the record guard every `ccdad uninstall` on a
// Claude-only machine names a file that is not there, in the middle of the one
// list a user reads before answering yes to a destructive command.
func TestUninstallSaysNothingAboutAShimWhenThereIsNoRecord(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	stubEnvironment(t, true, false)
	fakeBinary(t)
	seedAccount(t, "u-1", "work@example.com")

	cmd := newUninstallCmd()
	cmd.SetIn(strings.NewReader("n\n"))
	err, _, errOut := runCmd(t, cmd)
	if CodeFor(err) != ExitNothingToDo {
		t.Fatalf("answering no = %d, want %d\n%s", CodeFor(err), ExitNothingToDo, errOut)
	}
	// t.TempDir names its directories after the test, so every path in the
	// output below carries this test's own name -- including the word the
	// assertion is looking for. Take the name out first: what is under test
	// here is the prose.
	prose := strings.ReplaceAll(errOut, t.Name(), "<test>")
	if strings.Contains(strings.ToLower(prose), "shim") {
		t.Errorf("the confirmation names a shim on a machine that has none:\n%s", errOut)
	}
	if strings.Contains(errOut, shimDir()) {
		t.Errorf("the confirmation names the shim directory on a machine that has none:\n%s", errOut)
	}
}

// A store holding only the install record is still a ccdad store. Without this,
// a machine whose accounts were all removed -- or one where the only thing
// ccdad ever did was register the server -- is refused as "not a ccdad store"
// and the directory is left behind for nobody to clean up. It is the same
// reason the session and profile directories are already on that list.
func TestAStoreHoldingOnlyTheInstallRecordIsStillRecognised(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	fakeBinary(t)
	stubCcdadOnPath(t, true)
	if code, _, stderr, top := runRoot(t, "mcp", "install"); code != ExitOK {
		t.Fatalf("install = %d (%s%s)", code, stderr, top)
	}
	root := mustPath(ccpath.StoreHome())
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != mcpRecordFileName {
		t.Fatalf("the store holds %v; this test is only meaningful when the record is the only thing in it", entries)
	}

	code, _, stderr, top := runRoot(t, "uninstall", "--yes")
	if code != ExitOK {
		t.Fatalf("uninstall = %d (%s%s), want 0: a store holding only the record is still a ccdad store",
			code, stderr, top)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("%s survived the uninstall", root)
	}
}

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
func stubExecutable(t *testing.T, path string) string {
	t.Helper()
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

// §11.2 fix 6 points both installers at this command, and it deletes the live
// OAuth refresh token of every managed account. It has to say what it is about
// to destroy before it asks.
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
	// What the first `ccdad list` on a fresh machine leaves behind, and nothing
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

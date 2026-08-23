//go:build windows

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows/registry"
)

// The Windows half of `ccdad setup-path`, executed.
//
// Every DECISION setup-path makes is pure and lives in setuppath.go, where the
// tests run on whatever machine the suite runs on. What is in
// setuppath_windows.go is the part that has no answer off Windows: the registry
// access, the value kind, the ownership record, the UTF-16 ceiling and a
// SendMessageTimeoutW broadcast. Until this file existed, nothing had ever run
// one of them - `GOOS=windows go build` type-checks them and proves nothing
// about what they do.
//
// Three of the assertions below cannot be simulated at all:
//
//   - whether RegQueryValueEx reports the value's TYPE alongside
//     ErrUnexpectedType, which the refusal message prints;
//   - whether a Path whose UTF-8 length is past 32767 while its UTF-16 length
//     is not is accepted, which is every user with CJK directories on PATH;
//   - whether an entry stored as `%USERPROFILE%\bin` compares equal to its
//     expanded spelling through the real ExpandEnvironmentStringsW, which is
//     the defect install.ps1 shipped with.
//
// THE SCRATCH KEY IS NOT A CONVENIENCE. A contributor runs `go test ./...` on
// their own Windows machine, and a test that wrote HKCU\Environment\Path would
// leave one t.TempDir() on their real PATH per run, permanently -
// scripts/install_ps1_test.go skips on Windows rather than risk exactly that,
// and a seam is the version of that decision which still tests something.
// environmentSubkey and ccdadSubkey exist for this; every test in this file
// calls scratchRegistry before it touches either.

// scratchRegistry points the whole operation - the user PATH it reads and
// writes, and the ownership record it keeps - at two disposable keys named
// after the test, and puts the package back the way it found it afterwards.
//
// Both vars, never one. The PATH lives under environmentSubkey and the
// ownership record under ccdadSubkey, so swapping only the first would still
// write the real HKCU\Software\ccdad\PathEntry on a contributor's machine and
// hand `ccdad uninstall` permission to delete a directory this test invented.
func scratchRegistry(t *testing.T) {
	t.Helper()
	name := registrySafeName(t.Name())
	envKey := `Software\ccdad\test-environment-` + name
	stateKey := `Software\ccdad\test-state-` + name

	// A run that was interrupted leaves its keys behind, and a Path seeded by
	// the previous attempt would make this one assert against a starting state
	// it did not choose.
	dropScratchKey(envKey)
	dropScratchKey(stateKey)

	for _, path := range []string{envKey, stateKey} {
		// The environment key must EXIST and be empty: withUserPath opens it
		// rather than creating it, so an absent key fails with "opening HKCU\..."
		// instead of the "a profile that has never had a user PATH" branch that
		// the missing VALUE selects. HKCU\Environment is always there on a real
		// machine, which is the state this reproduces.
		key, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.ALL_ACCESS)
		if err != nil {
			t.Fatalf(`creating the scratch key HKCU\%s: %v`, path, err)
		}
		key.Close()
	}

	savedEnv, savedState := environmentSubkey, ccdadSubkey
	t.Cleanup(func() {
		environmentSubkey, ccdadSubkey = savedEnv, savedState
		// Only the two keys this made. Never HKCU\Software\ccdad itself: on a
		// machine where ccdad is installed that key holds the real PathEntry,
		// and deleting it would tell a later `ccdad uninstall` that the user's
		// own PATH entry was never ccdad's to remove.
		dropScratchKey(envKey)
		dropScratchKey(stateKey)
	})
	environmentSubkey, ccdadSubkey = envKey, stateKey
}

// dropScratchKey removes a scratch key if it is there. The keys this file
// creates never have subkeys, so RegDeleteKey is enough; an error means the key
// was already absent, which is the state being asked for.
func dropScratchKey(path string) {
	_ = registry.DeleteKey(registry.CURRENT_USER, path)
}

// registrySafeName turns a test name into one path component. Subtests put '/'
// in t.Name() and Go appends '#01' to a duplicate, neither of which belongs in
// a key name.
func registrySafeName(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, name)
}

// seedUserPath writes the Path value the test starts from, with the kind it
// starts from. The kind is an argument because preserving it is the property
// half of this file is about.
func seedUserPath(t *testing.T, value string, kind uint32) {
	t.Helper()
	key, err := registry.OpenKey(registry.CURRENT_USER, environmentSubkey, registry.SET_VALUE)
	if err != nil {
		t.Fatalf(`opening the scratch key HKCU\%s: %v`, environmentSubkey, err)
	}
	defer key.Close()
	if kind == registry.EXPAND_SZ {
		err = key.SetExpandStringValue("Path", value)
	} else {
		err = key.SetStringValue("Path", value)
	}
	if err != nil {
		t.Fatalf("seeding Path: %v", err)
	}
}

// readUserPath is the value and its kind, read the way the operation reads it:
// raw, never expanded.
func readUserPath(t *testing.T) (string, uint32) {
	t.Helper()
	key, err := registry.OpenKey(registry.CURRENT_USER, environmentSubkey, registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf(`opening the scratch key HKCU\%s: %v`, environmentSubkey, err)
	}
	defer key.Close()
	value, kind, err := key.GetStringValue("Path")
	if err != nil {
		t.Fatalf("reading Path back: %v", err)
	}
	return value, kind
}

// stubBroadcast replaces the desktop broadcast for the tests that are about the
// registry. The real one is asserted once, at the bottom of this file, and it
// costs up to five seconds per call.
func stubBroadcast(t *testing.T) *int {
	t.Helper()
	saved := broadcastEnvChange
	t.Cleanup(func() { broadcastEnvChange = saved })
	calls := 0
	broadcastEnvChange = func() error { calls++; return nil }
	return &calls
}

func kindName(kind uint32) string {
	switch kind {
	case registry.SZ:
		return "REG_SZ"
	case registry.EXPAND_SZ:
		return "REG_EXPAND_SZ"
	default:
		return fmt.Sprintf("registry type %d", kind)
	}
}

// The kind, and the entry as stored.
//
// [Environment]::SetEnvironmentVariable and registry.SetStringValue both write
// REG_SZ, and a Path that was REG_EXPAND_SZ loses %VAR% expansion for EVERY
// entry in it the moment one of them runs - not just for the entry being
// added. Writing the value back expanded does the same damage the other way
// round: it bakes today's %USERPROFILE% into the user's PATH forever.
func TestWindowsSetupPathKeepsTheValueKindAndTheEntryAsStored(t *testing.T) {
	scratchRegistry(t)
	stubBroadcast(t)
	t.Setenv("USERPROFILE", `C:\Users\ccdad-test`)
	seedUserPath(t, `%USERPROFILE%\bin`, registry.EXPAND_SZ)

	dir := `C:\Program Files\ccdad`
	added, err := registerUserPath(dir)
	if err != nil {
		t.Fatalf("registerUserPath: %v", err)
	}
	if !added {
		t.Fatal("registerUserPath reported nothing to do on a PATH that does not hold the directory")
	}

	got, kind := readUserPath(t)
	if kind != registry.EXPAND_SZ {
		t.Errorf("Path came back as %s, want REG_EXPAND_SZ: every %%VAR%% entry on the user's PATH stopped expanding",
			kindName(kind))
	}
	if want := `%USERPROFILE%\bin;` + dir; got != want {
		t.Errorf("Path = %q, want %q (the existing entry must come back verbatim, not expanded)", got, want)
	}
	if recorded, ok := recordedPathEntry(); !ok || recorded != dir {
		t.Errorf("recorded PATH entry = %q/%v, want %q/true; without the record `ccdad uninstall` has nothing it is allowed to remove",
			recorded, ok, dir)
	}
}

// A profile that has never had a user PATH. install.ps1 creates the value as
// ExpandString, and these two must agree or the same machine ends up with a
// different kind depending on which one ran first - after which the first
// %VAR% entry the user adds by hand stops expanding.
func TestWindowsSetupPathCreatesAMissingUserPathAsExpandable(t *testing.T) {
	scratchRegistry(t)
	stubBroadcast(t)

	dir := `C:\Program Files\ccdad`
	added, err := registerUserPath(dir)
	if err != nil {
		t.Fatalf("registerUserPath: %v", err)
	}
	if !added {
		t.Fatal("registerUserPath reported nothing to do on a profile with no user PATH at all")
	}
	got, kind := readUserPath(t)
	if kind != registry.EXPAND_SZ {
		t.Errorf("a created Path is %s, want REG_EXPAND_SZ - install.ps1 creates it expandable", kindName(kind))
	}
	if got != dir {
		t.Errorf("Path = %q, want %q", got, dir)
	}
}

// The defect install.ps1 shipped with, run against the real
// ExpandEnvironmentStringsW.
//
// Add-CcdadToUserPath read the value unexpanded - correctly - and then compared
// each component against a fully expanded install directory, so a user whose
// Path held `%LOCALAPPDATA%\Programs\ccdad` got a SECOND, expanded copy
// appended on every install, and it was a copy uninstall could not find. Both
// halves now compare a component as stored AND as expanded; only a real
// registry can say whether the expansion they get back is the one Windows
// performs.
func TestWindowsSetupPathDoesNotAppendACopyOfAnEntryStoredAsAVariable(t *testing.T) {
	stored := `%USERPROFILE%\bin`
	home := `C:\Users\ccdad-test`

	for _, tc := range []struct {
		name string
		dir  string
	}{
		{"expanded", home + `\bin`},
		// Windows compares PATH components without regard to case and a shell
		// resolves `...\bin` and `...\bin\` to one directory, so both of these
		// are the same entry as the one already stored. A duplicate here is a
		// duplicate the user sees.
		{"expanded in another case", strings.ToUpper(home) + `\BIN`},
		{"expanded with a trailing separator", home + `\bin\`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scratchRegistry(t)
			stubBroadcast(t)
			t.Setenv("USERPROFILE", home)
			seedUserPath(t, stored, registry.EXPAND_SZ)

			added, err := registerUserPath(tc.dir)
			if err != nil {
				t.Fatalf("registerUserPath: %v", err)
			}
			if added {
				got, _ := readUserPath(t)
				t.Fatalf("appended a second copy of an entry already on PATH as %s: Path = %q", stored, got)
			}
			if got, _ := readUserPath(t); got != stored {
				t.Errorf("Path = %q, want it untouched at %q", got, stored)
			}
			// The entry was the user's, not ccdad's. Recording it here would
			// hand `ccdad uninstall` permission to delete a directory ccdad
			// never added.
			if recorded, ok := recordedPathEntry(); ok {
				t.Errorf("recorded %q as ccdad's after adding nothing; uninstall would then remove the user's own PATH entry", recorded)
			}
		})
	}
}

// Twice is the ordinary case: install.ps1 registers the directory and the user
// then runs `ccdad setup-path` themselves.
func TestWindowsSetupPathRunTwiceAddsNothingTheSecondTime(t *testing.T) {
	scratchRegistry(t)
	stubBroadcast(t)
	seedUserPath(t, `C:\tools`, registry.EXPAND_SZ)

	dir := `C:\Program Files\ccdad`
	if added, err := registerUserPath(dir); err != nil || !added {
		t.Fatalf("first registerUserPath = %v, %v; want true, nil", added, err)
	}
	first, firstKind := readUserPath(t)

	added, err := registerUserPath(dir)
	if err != nil {
		t.Fatalf("second registerUserPath: %v", err)
	}
	if added {
		got, _ := readUserPath(t)
		t.Fatalf("a second run appended a duplicate: Path = %q", got)
	}
	second, secondKind := readUserPath(t)
	if second != first || secondKind != firstKind {
		t.Errorf("a run that added nothing rewrote the value: %q/%s became %q/%s",
			first, kindName(firstKind), second, kindName(secondKind))
	}
}

// Removal takes back the recorded entry and nothing else, and leaves the rest
// of the value exactly as it found it - including the kind and including a
// %VAR% entry it must not expand on the way through.
func TestWindowsUninstallRemovesTheRecordedEntryAndLeavesTheValueOtherwiseIntact(t *testing.T) {
	scratchRegistry(t)
	broadcasts := stubBroadcast(t)
	t.Setenv("USERPROFILE", `C:\Users\ccdad-test`)
	const seeded = `C:\Users\ccdad-test\go\bin;%USERPROFILE%\bin`
	seedUserPath(t, seeded, registry.EXPAND_SZ)

	dir := `C:\Program Files\ccdad`
	if added, err := registerUserPath(dir); err != nil || !added {
		t.Fatalf("registerUserPath = %v, %v; want true, nil", added, err)
	}

	places, err := unregisterPath(dir)
	if err != nil {
		t.Fatalf("unregisterPath: %v", err)
	}
	if want := `HKCU\` + environmentSubkey + `\Path`; len(places) != 1 || places[0] != want {
		t.Errorf("unregisterPath named %v, want [%s]", places, want)
	}
	got, kind := readUserPath(t)
	if got != seeded {
		t.Errorf("Path = %q, want %q - removal took back more than the entry it added", got, seeded)
	}
	if kind != registry.EXPAND_SZ {
		t.Errorf("Path came back as %s after removal, want REG_EXPAND_SZ", kindName(kind))
	}
	if recorded, ok := recordedPathEntry(); ok {
		t.Errorf("the ownership record survived removal as %q; a later uninstall would remove whatever now sits at that path", recorded)
	}
	// A terminal open across the uninstall must stop resolving a binary that is
	// no longer there. Once, not twice: registerUserPath does not announce
	// anything - setupPathApply does that - so this counts the removal alone.
	if *broadcasts != 1 {
		t.Errorf("the removal announced the environment change %d times, want 1", *broadcasts)
	}
}

// The finding that made the ownership record exist, executed.
//
// `go install` puts ccdad.exe in %USERPROFILE%\go\bin beside every other Go
// tool the user has, and that directory is on PATH because the USER put it
// there. Keying removal on "the directory the binary sits in" would strip it on
// uninstall and take every unrelated tool in it off PATH.
func TestWindowsUninstallLeavesAPathEntryCcdadNeverRecorded(t *testing.T) {
	scratchRegistry(t)
	stubBroadcast(t)
	const goBin = `C:\Users\ccdad-test\go\bin`
	seedUserPath(t, goBin, registry.EXPAND_SZ)
	// On this shell's PATH, which is what makes the entry visible to the user
	// and the refusal worth explaining.
	t.Setenv("PATH", goBin+`;C:\Windows\system32`)

	places, err := unregisterPath(goBin)
	if got, _ := readUserPath(t); got != goBin {
		t.Fatalf("Path = %q, want %q left alone: ccdad has no record of adding it, and `go install` puts ccdad.exe there beside every other Go tool", got, goBin)
	}
	if places != nil {
		t.Errorf("unregisterPath reported removing %v from a PATH it did not touch", places)
	}
	if err == nil {
		t.Fatal("uninstall said nothing about the entry it left on PATH; a user who expected it gone has no way to know to remove it")
	}
	if !strings.Contains(err.Error(), goBin) {
		t.Errorf("the notice does not name the entry that was left: %v", err)
	}
}

// pathRegistrations is what `ccdad uninstall` and `ccdad doctor` show a user
// BEFORE anything is removed, so it must answer for the record rather than for
// the PATH: an entry ccdad did not add is not something uninstall is about to
// take back, and listing it there would promise a removal that then does not
// happen.
func TestWindowsPathRegistrationsAnswersForTheRecordNotThePath(t *testing.T) {
	scratchRegistry(t)
	stubBroadcast(t)
	dir := `C:\Program Files\ccdad`
	seedUserPath(t, `C:\tools;`+dir, registry.EXPAND_SZ)

	places, err := pathRegistrations(dir)
	if err != nil {
		t.Fatalf("pathRegistrations: %v", err)
	}
	if places != nil {
		t.Errorf("pathRegistrations named %v for an entry ccdad never recorded adding", places)
	}

	// Now with the record in place. The entry is already on PATH, so
	// registerUserPath adds nothing and records nothing - recordPathEntry is
	// called directly, which is the state install.ps1 leaves behind when IT
	// registered the directory.
	if err := recordPathEntry(dir); err != nil {
		t.Fatalf("recordPathEntry: %v", err)
	}
	places, err = pathRegistrations(dir)
	if err != nil {
		t.Fatalf("pathRegistrations: %v", err)
	}
	if want := `HKCU\` + environmentSubkey + `\Path`; len(places) != 1 || places[0] != want {
		t.Errorf("pathRegistrations = %v, want [%s]", places, want)
	}
}

// A Path that is neither REG_SZ nor REG_EXPAND_SZ is not something to overwrite.
//
// This also pins the one fact the message depends on and that only the real API
// can supply: RegQueryValueEx reports the value's TYPE alongside the
// unexpected-type error, so the refusal can say WHICH type it found. If it came
// back zero the user would be told "registry type 0" and sent looking for a
// value that does not exist.
func TestWindowsSetupPathRefusesAPathItCannotRead(t *testing.T) {
	scratchRegistry(t)
	stubBroadcast(t)
	key, err := registry.OpenKey(registry.CURRENT_USER, environmentSubkey, registry.SET_VALUE)
	if err != nil {
		t.Fatalf("opening the scratch key: %v", err)
	}
	if err := key.SetDWordValue("Path", 7); err != nil {
		key.Close()
		t.Fatalf("seeding a REG_DWORD Path: %v", err)
	}
	key.Close()

	added, err := registerUserPath(`C:\Program Files\ccdad`)
	if err == nil {
		t.Fatal("overwrote a Path whose type it could not read")
	}
	if added {
		t.Error("registerUserPath reported a write it did not make")
	}
	if want := fmt.Sprintf("registry type %d", registry.DWORD); !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal does not name the type it found (%s): %v", want, err)
	}
	check, err := registry.OpenKey(registry.CURRENT_USER, environmentSubkey, registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf("opening the scratch key: %v", err)
	}
	defer check.Close()
	if _, _, err := check.GetIntegerValue("Path"); err != nil {
		t.Errorf("the unreadable Path was overwritten anyway: %v", err)
	}
}

// The ceiling is counted in UTF-16 code units because that is what Windows
// stores. len() counts UTF-8 bytes, and this PATH is past 32767 of those while
// being nowhere near the limit that actually exists - so a byte-counted ceiling
// refuses a write that fits, and tells the user a "character" count that is not
// one.
//
// Nothing on Linux can catch that: the arithmetic passes either way there, and
// only a registry that really stores UTF-16 says which number was the right one.
func TestWindowsSetupPathMeasuresTheCeilingInUTF16NotBytes(t *testing.T) {
	scratchRegistry(t)
	stubBroadcast(t)
	// 12000 CJK runes: 36000 UTF-8 bytes, 12000 UTF-16 code units.
	seeded := `C:\` + strings.Repeat("한", 12000)
	if len(seeded) <= maxEnvironmentValue {
		t.Fatalf("this fixture is not past the byte ceiling it exists to cross: %d bytes", len(seeded))
	}
	if n := len(utf16.Encode([]rune(seeded))) + 1; n > maxEnvironmentValue {
		t.Fatalf("this fixture is past the real ceiling too, so it proves nothing: %d code units", n)
	}
	seedUserPath(t, seeded, registry.EXPAND_SZ)

	dir := `C:\Program Files\ccdad`
	added, err := registerUserPath(dir)
	if err != nil {
		t.Fatalf("refused a user PATH that fits: %v", err)
	}
	if !added {
		t.Fatal("registerUserPath reported nothing to do")
	}
	if got, _ := readUserPath(t); got != seeded+";"+dir {
		t.Errorf("the value did not survive the round trip through the registry:\n got %q\nwant %q", got, seeded+";"+dir)
	}
}

// Past the ceiling the write still SUCCEEDS and new processes get a PATH they
// cannot use, so this refuses rather than truncating - a truncated PATH is a
// machine where half the user's tools stop resolving, caused by ccdad.
func TestWindowsSetupPathRefusesToPushThePathPastTheCeiling(t *testing.T) {
	scratchRegistry(t)
	stubBroadcast(t)
	// Exactly at the ceiling with its terminator: one more entry cannot fit.
	seeded := strings.Repeat("x", maxEnvironmentValue-1)
	seedUserPath(t, seeded, registry.EXPAND_SZ)

	added, err := registerUserPath(`C:\Program Files\ccdad`)
	if err == nil {
		t.Fatal("wrote a user PATH past the ceiling a process environment can hold")
	}
	if added {
		t.Error("registerUserPath reported a write it did not make")
	}
	if !strings.Contains(err.Error(), fmt.Sprint(maxEnvironmentValue)) {
		t.Errorf("the refusal does not name the ceiling it hit: %v", err)
	}
	if got, _ := readUserPath(t); got != seeded {
		t.Errorf("the refused write truncated the value anyway: it is now %d characters, was %d", len(got), len(seeded))
	}
}

// The command itself, end to end, on the platform - the report, the registry
// and the exit code.
//
// Exit 3 keys off what is REGISTERED rather than off the live %PATH%: the
// terminal running this test has not picked the change up and never will, and
// a second run must still say there is nothing to do.
func TestWindowsSetupPathCommandWritesTheRegistryThenExitsThreeOnTheSecondRun(t *testing.T) {
	isolate(t)
	scratchRegistry(t)
	stubBroadcast(t)
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stubExecutable(t, filepath.Join(dir, "ccdad.exe"))
	// packageManagerOwning consults these, and a developer running the suite
	// under Scoop would otherwise get a refusal instead of a write.
	t.Setenv("SCOOP", "")
	t.Setenv("HOMEBREW_PREFIX", "")
	// A PATH that does NOT hold the directory, which is the machine this
	// command exists for.
	t.Setenv("PATH", `C:\Windows\system32`)

	code, _, errOut, _ := runRoot(t, "setup-path")
	if code != ExitOK {
		t.Fatalf("setup-path = %d, want %d\n%s", code, ExitOK, errOut)
	}
	if !strings.Contains(errOut, dir) {
		t.Errorf("the report does not name the directory it registered:\n%s", errOut)
	}
	got, kind := readUserPath(t)
	if got != dir {
		t.Errorf("user PATH = %q, want %q", got, dir)
	}
	if kind != registry.EXPAND_SZ {
		t.Errorf("user PATH came back as %s, want REG_EXPAND_SZ", kindName(kind))
	}

	code, _, errOut, _ = runRoot(t, "setup-path")
	if code != ExitNothingToDo {
		t.Fatalf("a second setup-path = %d, want %d\n%s", code, ExitNothingToDo, errOut)
	}
	if second, _ := readUserPath(t); second != got {
		t.Errorf("the second run rewrote the value: %q became %q", got, second)
	}
}

// The broadcast body itself, with the seam left alone - the one assertion in
// this file that runs the user32 call rather than a stand-in.
//
// Three things are being asked, and every one of them has already been a bug
// somewhere in this function:
//
//   - it RETURNS. LazyProc.Call resolves through mustFind, which PANICS when
//     the DLL or the export is missing, and that would take down a command
//     whose registry write has already succeeded. Find() is called first for
//     exactly this, and a panic here is a failed test rather than a killed
//     process only because this is a test.
//   - it returns in bounded time. SendMessageTimeout, never SendMessage: a
//     plain broadcast waits for every top-level window on the desktop, so one
//     hung modal would hang ccdad with no way to interrupt it.
//   - a failure never reports itself as a success. LazyProc.Call builds its
//     error from GetLastError unconditionally, so on a failure that set no last
//     error it is Errno(0), which FORMATS as "The operation completed
//     successfully" - a bug report pointed in exactly the wrong direction.
//
// It deliberately does not require the broadcast to succeed. Whether a runner
// has a desktop with windows on it to answer is not this repository's property,
// and the command already treats a failed broadcast as a warning: the write is
// durable either way and the user pays one new terminal.
func TestWindowsTheRealEnvironmentBroadcastReturnsWithoutPanicking(t *testing.T) {
	done := make(chan error, 1)
	go func() { done <- sendSettingChange() }()

	select {
	case err := <-done:
		if err == nil {
			return
		}
		if strings.Contains(err.Error(), "operation completed successfully") {
			t.Errorf("a failed broadcast reported itself as a success: %v", err)
		}
		if strings.TrimSpace(err.Error()) == "" {
			t.Error("a failed broadcast reported an empty reason")
		}
		t.Logf("the broadcast did not reach a window on this runner, which is allowed: %v", err)
	case <-time.After(30 * time.Second):
		// Six times the 5s the call was given. A runner slow enough to need
		// more than this has a problem the timeout was supposed to bound.
		t.Fatal("the broadcast did not return within 30s; SMTO_ABORTIFHUNG and the 5s timeout did not bound it")
	}
}

//go:build windows

package ccver

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// THE HALF OF THIS PACKAGE THAT ONLY A WINDOWS RUNNER CAN FAIL.
//
// Everything else about the copy branch is a path shape and is exercised on
// whatever host runs the suite. This is not: Claude Code puts the native
// launcher under lAi()'s home, which reads HOME before os.homedir(), while
// ccpath.Home is os.UserHomeDir — %USERPROFILE% here and $HOME everywhere else.
// Off Windows the two are the same string, so a test written there passes with
// either resolver and pins nothing. Here they are different directories, and
// looking under the wrong one finds no launcher at all: doctor would tell a
// Git-for-Windows user with a working native install that ccdad cannot find
// their Claude Code, and `ccdad run` would start without ever checking the
// version.
func TestANativeCopyUnderHOMEIsFoundWhenUSERPROFILEIsElsewhere(t *testing.T) {
	layoutHome := tempDir(t)
	profile := tempDir(t)
	t.Setenv("HOME", layoutHome)
	t.Setenv("USERPROFILE", profile)
	t.Setenv("XDG_DATA_HOME", "")

	versions := filepath.Join(layoutHome, ".local", "share", "claude", "versions")
	write(t, filepath.Join(versions, "2.1.240"), "ELF................240")
	write(t, filepath.Join(versions, "2.1.241"), "ELF................241")
	launcher := filepath.Join(layoutHome, ".local", "bin", "claude.exe")
	write(t, launcher, "ELF................241")

	saved := lookPath
	t.Cleanup(func() { lookPath = saved })
	lookPath = func(string) (string, error) { return "", errors.New("not on PATH") }

	got, err := Probe()
	if err != nil {
		t.Fatalf("Probe() looked for the launcher under the wrong home: %v", err)
	}
	if got.Launcher != launcher {
		t.Fatalf("Launcher = %q, want %q", got.Launcher, launcher)
	}
	if !got.Known {
		t.Fatalf("Probe() found the copy and could not name it: %s", got.Why)
	}
	if got.Version != (Version{2, 1, 241}) {
		t.Errorf("Version = %s, want 2.1.241", got.Version)
	}
	if got.Method != MethodNative || !got.Copied {
		t.Errorf("Method = %q, Copied = %v — want a native install whose launcher is a copy",
			got.Method, got.Copied)
	}
}

// The other direction of the same split, and the reason LayoutHome could not
// simply replace ccpath.Home in this file: ~/.claude/local is a CONFIG-side
// path, so it hangs off os.homedir() and therefore off %USERPROFILE%. A
// fallback list that resolved both entries through the layout home would walk
// past a local install sitting exactly where Claude Code put it.
func TestTheLocalInstallFallbackStaysUnderUSERPROFILE(t *testing.T) {
	layoutHome := tempDir(t)
	profile := tempDir(t)
	t.Setenv("HOME", layoutHome)
	t.Setenv("USERPROFILE", profile)
	t.Setenv("XDG_DATA_HOME", "")

	local := filepath.Join(profile, ".claude", "local")
	write(t, filepath.Join(local, "claude"), "#!/bin/sh\n")
	write(t, filepath.Join(local, "node_modules", "@anthropic-ai", "claude-code", "package.json"),
		manifest(PackageName, "2.1.90"))

	saved := lookPath
	t.Cleanup(func() { lookPath = saved })
	lookPath = func(string) (string, error) { return "", errors.New("not on PATH") }

	got, err := Probe()
	if err != nil {
		t.Fatalf("Probe() did not look for the local install under %%USERPROFILE%%: %v", err)
	}
	if got.Version != (Version{2, 1, 90}) {
		t.Fatalf("Probe() = %s from %q, want 2.1.90 from the local install under %q",
			got.Version, got.Launcher, local)
	}
}

// A keychain-era native install on Windows, which is the machine the item behind
// this work is about and which the npm registry says is real: the platform
// binaries were published for 2.1.110, 2.1.111 and 2.1.112, and 2.1.112's own
// bundle carries the native installer's win32 branch with CLAUDE_SECURESTORAGE_
// CONFIG_DIR occurring in it exactly zero times. Before the bytes were read,
// such an install was Known=false, so KeychainEra() was false, so `ccdad run`
// started a default-mode session that silently ran as the live login.
func TestAKeychainEraNativeCopyIsNowInTheEra(t *testing.T) {
	layoutHome := tempDir(t)
	t.Setenv("HOME", layoutHome)
	t.Setenv("USERPROFILE", tempDir(t))
	t.Setenv("XDG_DATA_HOME", "")

	versions := filepath.Join(layoutHome, ".local", "share", "claude", "versions")
	write(t, filepath.Join(versions, "2.1.112"), "ELF-112")
	launcher := filepath.Join(layoutHome, ".local", "bin", "claude.exe")
	write(t, launcher, "ELF-112")

	got := Describe(launcher)
	if !got.KeychainEra() {
		t.Fatalf("KeychainEra() = false for %s — the refusal `ccdad run` exists for still does not fire "+
			"on Windows: %s", got, got.Why)
	}
}

// THE OTHER HOME, and the regression this file exists to stop repeating. Claude
// Code INSTALLS under lAi()'s home and SEARCHES under a plain os.homedir(), so
// the documented Windows install — `irm https://claude.ai/install.ps1 | iex`
// from PowerShell, where $env:HOME does not exist — lands under %USERPROFILE%.
// Run ccdad from an MSYS2 or Cygwin shell, whose HOME is C:\msys64\home\<user>,
// and a search that had moved wholesale to the layout home would report NO
// CLAUDE CODE on a machine that has a working one.
func TestAnInstallUnderUSERPROFILEIsFoundWhenHOMEPointsElsewhere(t *testing.T) {
	shellHome := tempDir(t)
	profile := tempDir(t)
	t.Setenv("HOME", shellHome)
	t.Setenv("USERPROFILE", profile)
	t.Setenv("XDG_DATA_HOME", "")

	versions := filepath.Join(profile, ".local", "share", "claude", "versions")
	write(t, filepath.Join(versions, "2.1.112"), "ELF-112")
	launcher := filepath.Join(profile, ".local", "bin", "claude.exe")
	write(t, launcher, "ELF-112")

	saved := lookPath
	t.Cleanup(func() { lookPath = saved })
	lookPath = func(string) (string, error) { return "", errors.New("not on PATH") }

	got, err := Probe()
	if err != nil {
		t.Fatalf("Probe() reported no Claude Code on a machine with a native install under %%USERPROFILE%%: %v", err)
	}
	if got.Launcher != launcher {
		t.Fatalf("Launcher = %q, want %q", got.Launcher, launcher)
	}
	if !got.KeychainEra() {
		t.Fatalf("KeychainEra() = false for %s — found the install and could not name it: %s", got, got.Why)
	}
}

// %PATH% and %HOME% are two independently-typed spellings of one directory here,
// and Windows treats them as the same directory while filepath.Clean does not.
// Probe answers from PATH first, so the launcher arrives spelled as PATH had it
// while the versions tree is spelled as HOME has it — and an unfolded comparison
// would answer "this is not a native install" about a native install, which is
// the whole branch gated off on the one platform it is for.
func TestALauncherSpelledWithADifferentCaseIsStillANativeCopy(t *testing.T) {
	home := tempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", "")

	versions := filepath.Join(home, ".local", "share", "claude", "versions")
	write(t, filepath.Join(versions, "2.1.112"), "ELF-112")
	launcher := filepath.Join(home, ".local", "bin", "claude.exe")
	write(t, launcher, "ELF-112")

	// What a hand-edited PATH entry, or an installer that wrote the directory
	// in another case, hands to exec.LookPath. The file opens either way.
	asPathSpelledIt := strings.ToUpper(launcher)
	if asPathSpelledIt == launcher {
		t.Skip("this host's temp directory has no case to change")
	}

	got := Describe(asPathSpelledIt)
	if !got.Known {
		t.Fatalf("Describe(%q) could not name the install its own HOME holds: %s", asPathSpelledIt, got.Why)
	}
	if got.Version != (Version{2, 1, 112}) {
		t.Errorf("Version = %s, want 2.1.112", got.Version)
	}
	if got.Method != MethodNative || !got.Copied {
		t.Errorf("Method = %q, Copied = %v — want a native install whose launcher is a copy",
			got.Method, got.Copied)
	}
}

// With neither variable set there is no home to spell, and LayoutHome must say
// so rather than hand back "" — which would make every layout path relative and
// send the launcher search into whatever directory ccdad was started in. Pinned
// on Windows as well as off it, because this is the platform where the two
// variables can disagree and only one of them is os.UserHomeDir's.
func TestProbeSaysThereIsNoHomeRatherThanSearchingRelativePaths(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("CCDAD_HOME", tempDir(t))

	saved := lookPath
	t.Cleanup(func() { lookPath = saved })
	lookPath = func(string) (string, error) { return "", errors.New("not on PATH") }

	_, err := Probe()
	if err == nil {
		t.Fatal("Probe() searched a machine with no home directory and reported a result")
	}
	if errors.Is(err, ErrNoClaudeCode) {
		t.Fatal("Probe() reported ErrNoClaudeCode — that asserts a search was performed and found nothing, " +
			"and no path could even be spelled")
	}
}

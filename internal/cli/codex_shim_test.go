package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// The shim body is pinned BYTE FOR BYTE, and every word of it is load-bearing.
// `exec` replaces the shell rather than leaving one waiting, so the process
// tree a user's terminal reports is the codex they started and Ctrl-C reaches
// it. The bare word `ccdad` resolves through PATH, so an updated binary in the
// same directory is picked up with nothing to re-install. `--` stops ccdad
// parsing what follows, which is how `codex -c foo=bar` survives. `"$@"` is
// quoted, so a prompt with a space in it stays one argument.
func TestTheCodexShimIsExactlyTheTwoLinesItHasToBe(t *testing.T) {
	if want := "#!/bin/sh\nexec ccdad codex exec -- \"$@\"\n"; codexShimBody != want {
		t.Fatalf("codexShimBody = %q, want %q", codexShimBody, want)
	}
}

func TestCodexShimInstallWritesTheShimAndRegistersItsDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("there is no shim on Windows; the refusal has its own test")
	}
	isolate(t)
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOMEBREW_PREFIX", "")
	t.Setenv("SCOOP", "")
	stubExecutable(t, filepath.Join(t.TempDir(), "ccdad"))

	code, _, errOut, top := runRoot(t, "codex", "shim", "install")
	if code != ExitOK {
		t.Fatalf("codex shim install = %d, want %d\n%s\n%s", code, ExitOK, errOut, top)
	}

	body, err := os.ReadFile(shimPath())
	if err != nil {
		t.Fatalf("the shim was not written: %v", err)
	}
	if string(body) != codexShimBody {
		t.Errorf("the shim is %q, want %q", body, codexShimBody)
	}
	info, err := os.Stat(shimPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("the shim is mode %v, which no shell will execute", info.Mode().Perm())
	}

	raw, err := os.ReadFile(shimRecordPath())
	if err != nil {
		t.Fatalf("the install record was not written: %v", err)
	}
	var rec codexShimRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("the install record does not parse: %v (%s)", err, raw)
	}
	if rec.SchemaVersion != 1 || rec.Path != shimPath() || rec.InstalledAt == "" {
		t.Errorf("the install record is %+v, want schemaVersion 1, path %s and a stamp", rec, shimPath())
	}

	// The registration goes through setup-path's own writer, so there is ONE
	// block on the machine rather than two, and `ccdad uninstall` takes it back
	// with the removal it already has.
	rc := filepath.Join(os.Getenv("HOME"), ".bashrc")
	block, err := os.ReadFile(rc)
	if err != nil {
		t.Fatalf("no startup file was written: %v", err)
	}
	if !strings.Contains(string(block), shimDir()) {
		t.Errorf("%s does not register %s:\n%s", rc, shimDir(), block)
	}
	if !strings.Contains(string(block), setupPathBegin) {
		t.Errorf("%s carries no ccdad block, so uninstall cannot take it back:\n%s", rc, block)
	}
}

func TestCodexShimInstallIsNothingToDoTheSecondTime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("there is no shim on Windows; the refusal has its own test")
	}
	isolate(t)
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOMEBREW_PREFIX", "")
	t.Setenv("SCOOP", "")
	stubExecutable(t, filepath.Join(t.TempDir(), "ccdad"))

	if code, _, errOut, _ := runRoot(t, "codex", "shim", "install"); code != ExitOK {
		t.Fatalf("the first install = %d, want %d\n%s", code, ExitOK, errOut)
	}
	before, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".bashrc"))
	if err != nil {
		t.Fatal(err)
	}

	code, _, errOut, _ := runRoot(t, "codex", "shim", "install")
	if code != ExitNothingToDo {
		t.Fatalf("the second install = %d, want %d (the world is already as the caller asked)\n%s",
			code, ExitNothingToDo, errOut)
	}
	after, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".bashrc"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("the second install rewrote the startup file:\n--- first ---\n%s\n--- second ---\n%s", before, after)
	}
}

// A shim whose SCRIPT survives but whose record was deleted is not "already
// installed": the record is what setup-path's derived set keys on, so leaving
// it missing would leave the directory unregistered forever while the command
// reported nothing to do.
func TestCodexShimInstallRewritesAMissingRecord(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("there is no shim on Windows; the refusal has its own test")
	}
	isolate(t)
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOMEBREW_PREFIX", "")
	t.Setenv("SCOOP", "")
	stubExecutable(t, filepath.Join(t.TempDir(), "ccdad"))

	if code, _, errOut, _ := runRoot(t, "codex", "shim", "install"); code != ExitOK {
		t.Fatalf("the first install = %d, want %d\n%s", code, ExitOK, errOut)
	}
	if err := os.Remove(shimRecordPath()); err != nil {
		t.Fatal(err)
	}

	if code, _, errOut, _ := runRoot(t, "codex", "shim", "install"); code != ExitOK {
		t.Fatalf("install with the record deleted = %d, want %d\n%s", code, ExitOK, errOut)
	}
	if _, err := os.Stat(shimRecordPath()); err != nil {
		t.Errorf("the record was not rewritten: %v", err)
	}
}

// A shim whose executable bit was taken away is not "already installed". It is
// the one broken shim a user cannot diagnose: `codex` is still first on PATH,
// the body is still right, and every attempt is `codex: Permission denied` with
// nothing to point at. os.WriteFile applies its mode only when it CREATES the
// file, so the repair is the chmod -- and the mode has to be part of what
// install compares, or the early return answers "nothing to do" to the machine
// that most needs the repair.
func TestCodexShimInstallRestoresAnExecutableBitSomebodyTookAway(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("there is no shim on Windows; the refusal has its own test")
	}
	isolate(t)
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOMEBREW_PREFIX", "")
	t.Setenv("SCOOP", "")
	stubExecutable(t, filepath.Join(t.TempDir(), "ccdad"))

	if code, _, errOut, _ := runRoot(t, "codex", "shim", "install"); code != ExitOK {
		t.Fatalf("the first install = %d, want %d\n%s", code, ExitOK, errOut)
	}
	if err := os.Chmod(shimPath(), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, errOut, top := runRoot(t, "codex", "shim", "install")
	if code != ExitOK {
		t.Fatalf("install over a non-executable shim = %d, want %d (there is a repair to make)\n%s\n%s",
			code, ExitOK, errOut, top)
	}
	info, err := os.Stat(shimPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("the shim is still mode %v, which no shell will execute", info.Mode().Perm())
	}
	body, err := os.ReadFile(shimPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != codexShimBody {
		t.Errorf("the repair left the shim as %q, want %q", body, codexShimBody)
	}
}

// Windows gets no shim in v1, and the refusal says why and what to run instead
// rather than failing silently.
func TestCodexShimInstallRefusesOnWindows(t *testing.T) {
	isolate(t)
	saved := shimOS
	t.Cleanup(func() { shimOS = saved })
	shimOS = "windows"

	code, _, errOut, top := runRoot(t, "codex", "shim", "install")
	if code != ExitBlocked {
		t.Fatalf("codex shim install on Windows = %d, want %d\n%s\n%s", code, ExitBlocked, errOut, top)
	}
	said := errOut + top
	if !strings.Contains(said, "cmd.exe") {
		t.Errorf("the refusal does not say why there is no Windows shim:\n%s", said)
	}
	if !strings.Contains(said, "ccdad codex exec") {
		t.Errorf("the refusal does not name what to run instead:\n%s", said)
	}
	if _, err := os.Stat(filepath.Join(mustPath(ccpath.StoreHome()), shimRecordName)); err == nil {
		t.Error("the refusal still wrote an install record")
	}
}

// The totality test in scoped_test.go walks the tree and fails on any command
// with no verdict. This pins WHICH verdict the two new paths carry, which that
// walk cannot: a refusal would satisfy it just as well and would stop a user
// installing the shim from inside a `ccdad run` session, where installing it is
// exactly as safe as it is outside one.
func TestTheCodexShimPathsAreAllowedInAScopedSession(t *testing.T) {
	for _, path := range []string{"ccdad codex shim", "ccdad codex shim install"} {
		if _, refused := scopedSessionRefusals[path]; refused {
			t.Errorf("%q is refused inside a scoped session; it writes only ccdad's own store and the user's startup file, neither of which a session scopes", path)
		}
		if !scopedSessionAllowed[path] {
			t.Errorf("%q has no scoped-session verdict", path)
		}
	}
}

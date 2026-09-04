package cli

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// denyExecute puts the machine into the one state this tree has to tell apart
// from a missing execute bit: a file whose mode says 0o755 and which THIS user
// still cannot run. It is what an ACL entry and a hostile ownership both look
// like from Go, and it is the state no chmod ccdad makes can leave.
//
// A REAL ACL wherever the platform will make one. Measured on darwin:
// `chmod +a "user:<me> deny execute" <file>` leaves the file mode 0o755, leaves
// Perm()&0o111 set, and makes exec.LookPath on it "permission denied" -- so the
// state under test is the state on a user's machine and not a description of
// it. The ACL is removed again on cleanup, before the temp directory is.
//
// Where no ACL can be made -- another OS, a filesystem mounted without them,
// a chmod that reports success and changes nothing -- the runnable probe is
// stubbed for this one path instead, so the arm is covered on every OS this
// suite runs on rather than only on the one that can build the state for real.
// It says in the log which of the two it used, because a test that cannot say
// that proves less than it looks like it does.
func denyExecute(t *testing.T, path string) {
	t.Helper()
	// An ABSOLUTE chmod, because the tests that need this state are the ones
	// that replace PATH with a shim directory and a fake codex, and a bare
	// `chmod` there is not found -- which would silently downgrade every one of
	// them to the stub on the one platform that can do this for real.
	chmod := ""
	for _, candidate := range []string{"/bin/chmod", "/usr/bin/chmod"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			chmod = candidate
			break
		}
	}
	if me, uerr := user.Current(); uerr == nil && runtime.GOOS == "darwin" && chmod != "" {
		entry := "user:" + me.Username + " deny execute"
		if out, cerr := exec.Command(chmod, "+a", entry, path).CombinedOutput(); cerr != nil {
			t.Logf("%s +a could not make an ACL on %s (%v: %s)", chmod, path, cerr, out)
		} else {
			t.Cleanup(func() { _ = exec.Command(chmod, "-a", entry, path).Run() })
			if _, lerr := exec.LookPath(path); lerr != nil {
				t.Logf("the state under test is a real ACL: %v", lerr)
				return
			}
			t.Logf("%s +a reported success on %s and it is still runnable, so the ACL did not stick", chmod, path)
		}
	}
	saved := shimRunnable
	t.Cleanup(func() { shimRunnable = saved })
	shimRunnable = func(p string) error {
		if p == path {
			return fmt.Errorf("exec: %q: %w", p, fs.ErrPermission)
		}
		return saved(p)
	}
	t.Logf("no ACL could be made here, so the runnable probe is stubbed for %s", path)
}

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

// A shim ccdad cannot make runnable is NOT "already installed". Everything
// install compares is a write it can make -- the body, the record, the mode --
// and a shim held down by an ACL entry passes all three while being unable to
// start. Reporting exit 3 there is the failure this refusal exists for: doctor
// sends the user to this command, the command says the shim is already there,
// and the row keeps saying the same thing forever.
func TestCodexShimInstallRefusesAShimItCannotMakeRunnable(t *testing.T) {
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
	denyExecute(t, shimPath())

	code, _, errOut, top := runRoot(t, "codex", "shim", "install")
	if code != ExitBlocked {
		t.Fatalf("install over a shim ccdad cannot make runnable = %d, want %d (there is nothing it can "+
			"write that fixes this)\n%s\n%s", code, ExitBlocked, errOut, top)
	}
	said := errOut + top
	if !strings.Contains(said, "ACL") {
		t.Errorf("the refusal does not say what is holding the shim down:\n%s", said)
	}
	if !strings.Contains(said, "chmod -N") {
		t.Errorf("the refusal does not name what the user can do about it:\n%s", said)
	}
	if strings.Contains(said, "already") {
		t.Errorf("the refusal still reports the shim as already installed:\n%s", said)
	}
}

// The same refusal from the other side of the early return: with the record
// deleted, install takes the WRITE path, rewrites the body and chmods 0o755 --
// and a deny entry is not a mode bit, so the shim comes out of that chmod
// exactly as unrunnable as it went in. Without the probe after the chmod the
// command prints "Wrote <shim>. A new terminal runs `codex` through ccdad" on a
// machine where no terminal will.
func TestCodexShimInstallDoesNotClaimAChmodItCannotMake(t *testing.T) {
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
	denyExecute(t, shimPath())

	code, _, errOut, top := runRoot(t, "codex", "shim", "install")
	if code != ExitBlocked {
		t.Fatalf("install that rewrote a shim it cannot run = %d, want %d\n%s\n%s",
			code, ExitBlocked, errOut, top)
	}
	if strings.Contains(errOut+top, "A new terminal runs") {
		t.Errorf("install claimed the shim now works:\n%s\n%s", errOut, top)
	}
	// The record is what setup-path's derived set keys on, so it must not come
	// back for a shim ccdad just found it cannot run.
	if _, err := os.Stat(shimRecordPath()); err == nil {
		t.Error("install wrote the install record for a shim it refused")
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

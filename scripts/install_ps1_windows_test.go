//go:build windows

package scripts

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// install.ps1's upgrade path, executed against a binary that is really
// running.
//
// Everything else about install.ps1 is tested by driving the whole installer
// under pwsh on Linux, and runInstallPs1 skips itself on Windows on purpose:
// the last thing the driver does is Add-CcdadToUserPath, which would append a
// t.TempDir() to a contributor's real HKCU PATH once per run, permanently. The
// cost of that skip was that the one branch with no meaning off Windows --
// "the file at $Target is mapped into a live process" -- was reachable by
// nothing. On Linux, Move-Item -Force over the old binary simply succeeds, so
// the rename-aside runs but the condition that makes it necessary never holds.
//
// Install-CcdadBinary was split out of Invoke-CcdadInstall so this file can
// call it alone. It touches no registry and starts no download, so there is
// nothing here to keep off a real machine.
//
// The build tag is doing the work a t.Skip would otherwise do, which is why
// there is none: off Windows there is no such thing as a mapped image to test
// against, and a skip would report that as a test that ran.

// startRunningBinary puts a real, executing image at path.
//
// internal/cli/uninstall_windows_test.go carries the same helper for the same
// reason -- `ccdad uninstall` renames a running .exe aside exactly as this
// installer does. They are in different packages and a test helper is not
// something to export from a non-test one, so the duplication is deliberate;
// if the technique here stops working it has stopped working there too.
//
// cmd.exe is copied rather than built: what is needed is any PE that Windows
// has mapped into a live process. `/k` with an open stdin pipe sits at a
// prompt indefinitely, so the process outlives the test body instead of the
// test racing its exit.
func startRunningBinary(t *testing.T, path string) {
	t.Helper()
	source := os.Getenv("COMSPEC")
	if source == "" {
		source = filepath.Join(os.Getenv("SystemRoot"), `System32\cmd.exe`)
	}
	image, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("reading %s to make a running binary out of: %v", source, err)
	}
	if err := os.WriteFile(path, image, 0o755); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}

	cmd := exec.Command(path, "/k")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("opening stdin for %s: %v", path, err)
	}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", path, err)
	}
	// Before t.TempDir's own cleanup, which was registered earlier and so runs
	// later: RemoveAll cannot delete a mapped image.
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
}

// runFragment runs a fragment with install.ps1's functions defined and nothing
// installed, and hands back the output whether or not it succeeded.
//
// dotSource is the same thing for tests that expect success; this one exists
// because half of what is being asserted here is a failure -- that Windows
// refuses the plain overwrite the rename-aside was written to avoid.
func runFragment(t *testing.T, fragment string) (string, error) {
	t.Helper()
	shell := powershell(t)
	script, err := filepath.Abs(filepath.Join("..", "install.ps1"))
	if err != nil {
		t.Fatalf("resolving install.ps1: %v", err)
	}
	body := ". '" + script + "' -NoRun\n" + fragment
	cmd := exec.Command(shell, "-NoProfile", "-NonInteractive", "-Command", body)
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// leftoverNames is what the install directory holds besides ccdad.exe.
func leftoverNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.Name() != "ccdad.exe" {
			names = append(names, e.Name())
		}
	}
	return names
}

// The upgrade every Windows user gets: the binary being replaced is the one
// running the daemon, or simply one an open terminal is sitting in.
func TestInstallPs1UpgradeLandsOverARunningBinary(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ccdad.exe")
	startRunningBinary(t, target)

	staging := filepath.Join(dir, ".ccdad-install.staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(staging, "ccdad-windows-amd64.exe")
	if err := os.WriteFile(source, []byte("the new release"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The control, and it has to come first. If Windows allowed this, the
	// rename-aside would be four lines of ceremony over a Move-Item and the
	// rest of this test would be measuring nothing.
	out, err := runFragment(t, "try { Move-Item -LiteralPath '"+source+"' -Destination '"+target+
		"' -Force; 'OVERWROTE' } catch { 'REFUSED' }")
	if err != nil {
		t.Fatalf("the control fragment did not run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "REFUSED") {
		t.Fatalf("Move-Item overwrote a running .exe (%s); the upgrade path's whole reason for existing is that it cannot", strings.TrimSpace(out))
	}

	out, err = runFragment(t, "Install-CcdadBinary -Source '"+source+"' -Target '"+target+"'")
	if err != nil {
		t.Fatalf("Install-CcdadBinary over a running binary: %v\n%s", err, out)
	}

	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading the installed binary: %v", err)
	}
	if string(body) != "the new release" {
		t.Errorf("ccdad.exe holds %q, want the new release; the upgrade did not land", body)
	}
	// The old image is still mapped, so the best-effort delete cannot have
	// succeeded and the leftover is the evidence the rename happened at all.
	leftovers := leftoverNames(t, dir)
	var aside []string
	for _, name := range leftovers {
		if strings.HasPrefix(name, ".ccdad-old.") && strings.HasSuffix(name, ".exe") {
			aside = append(aside, name)
		}
	}
	if len(aside) != 1 {
		t.Fatalf("the install directory holds %v; want exactly one .ccdad-old.*.exe, which is the old binary moved out of the way", leftovers)
	}
}

// The other half, and the one that keeps the leftover from being permanent: a
// binary nothing holds is renamed aside AND deleted, so an upgrade run while
// ccdad is not running leaves the install directory as it found it.
func TestInstallPs1UpgradeTakesTheOldBinaryWithItWhenNothingHoldsIt(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ccdad.exe")
	if err := os.WriteFile(target, []byte("the old release"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "ccdad-windows-amd64.exe")
	if err := os.WriteFile(source, []byte("the new release"), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := runFragment(t, "Install-CcdadBinary -Source '"+source+"' -Target '"+target+"'")
	if err != nil {
		t.Fatalf("Install-CcdadBinary: %v\n%s", err, out)
	}

	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "the new release" {
		t.Errorf("ccdad.exe holds %q, want the new release", body)
	}
	if names := leftoverNames(t, dir); len(names) != 0 {
		t.Errorf("the install directory holds %v; an upgrade with nothing running has no leftover to leave", names)
	}
}

// A fresh install goes through the same function, and Test-Path is the only
// thing standing between it and a rename of a file that is not there.
func TestInstallPs1InstallsWhereThereWasNothing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ccdad.exe")
	source := filepath.Join(t.TempDir(), "ccdad-windows-amd64.exe")
	if err := os.WriteFile(source, []byte("the first release"), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := runFragment(t, "Install-CcdadBinary -Source '"+source+"' -Target '"+target+"'")
	if err != nil {
		t.Fatalf("Install-CcdadBinary onto an empty directory: %v\n%s", err, out)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "the first release" {
		t.Errorf("ccdad.exe holds %q, want the first release", body)
	}
	if names := leftoverNames(t, dir); len(names) != 0 {
		t.Errorf("a fresh install left %v behind", names)
	}
}

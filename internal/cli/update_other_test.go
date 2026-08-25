//go:build !windows

package cli

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// startRunningImage puts a real, executing image at path and returns once the
// kernel has it mapped.
//
// /bin/sh is copied rather than built, because what is needed is an image the
// kernel has mapped into a live process and any executable will do — building
// one would spend a compiler on a property the copy already has. It is copied
// rather than executed in place so that the file under test is the one this
// process holds open.
func startRunningImage(t *testing.T, path string) {
	t.Helper()
	image, err := os.ReadFile("/bin/sh")
	if err != nil {
		t.Skipf("no /bin/sh to make a running image out of: %v", err)
	}
	if err := os.WriteFile(path, image, 0o755); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	cmd := exec.Command(path, "-c", "read x")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("opening stdin for %s: %v", path, err)
	}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", path, err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
}

// The control comes first. Without it this file proves that a rename works on a
// file, which is not the claim: the claim is that a rename works where a WRITE
// does not, which is the entire reason the staged file is a sibling of the
// target rather than something in /tmp that gets copied over.
func TestReplaceBinaryWorksWhereAWriteOverALiveImageDoesNot(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ccdad")
	startRunningImage(t, target)

	if err := os.WriteFile(target, []byte("new"), 0o755); err == nil {
		t.Fatal("writing over a live image succeeded; this platform does not " +
			"have the constraint replaceBinary exists for, and the rest of this test proves nothing")
	}

	staged := filepath.Join(dir, "ccdad-staged")
	if err := os.WriteFile(staged, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceBinary(staged, target); err != nil {
		t.Fatalf("replaceBinary() over a live image: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "#!/bin/sh\nexit 0\n" {
		t.Errorf("target holds %q, want the staged bytes", got)
	}
	if _, err := os.Stat(staged); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the staged file survived the rename (stat said %v)", err)
	}
}

// The mode arrives with the file, because the download step chmods it after
// closing it. replaceBinary must not quietly reset it.
func TestReplaceBinaryKeepsTheStagedMode(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ccdad")
	staged := filepath.Join(dir, "ccdad-staged")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(staged, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceBinary(staged, target); err != nil {
		t.Fatalf("replaceBinary(): %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want -rwxr-xr-x", info.Mode().Perm())
	}
}

// A failure must be reported and must leave the old binary where it was. The
// error is checked with errors.Is against fs.ErrNotExist and never against a
// message: "no such file or directory" is ENOENT's Unix text and Windows says
// something else.
func TestReplaceBinaryLeavesTheTargetAloneWhenTheStagedFileIsMissing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ccdad")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := replaceBinary(filepath.Join(dir, "absent"), target)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("replaceBinary() error = %v, want it to wrap fs.ErrNotExist", err)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil || string(got) != "old" {
		t.Errorf("target = %q (%v), want the old bytes untouched", got, readErr)
	}
}

//go:build !windows

package cli

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// Whether a write over a live image fails is a platform fact, and it is not the
// same fact on every Unix. Measured on GitHub's runners on 2026-08-26, with the
// child proved alive by signal 0 immediately before and immediately after the
// write:
//
//	linux/amd64    open …/ccdad: text file busy
//	darwin/arm64    <nil>
//
// On darwin both a copy of /bin/sh and an image the Go toolchain had just built
// were overwritten while running, so what differs is the platform and not the
// way the image was made: macOS does not hold the constraint Linux reports as
// ETXTBSY.
//
// This is a DECLARED fact and not a skip on a failed probe, because a probe
// alone cannot tell "this platform has no such constraint" from "the constraint
// regressed on a platform that has one" — and the second is the only thing the
// control below exists to catch. So the control asserts the fact in both
// directions and is loud either way.
const liveImageIsWriteProtected = runtime.GOOS != "darwin"

// The control, and it is a test of its own rather than the first half of the
// one below. It does not hold everywhere and the claim below does: on darwin a
// live image can simply be written over, so pairing them would either give up
// the rename coverage on the platform `ccdad update` most needs it, or leave
// this file's central claim quietly conditional.
func TestAWriteOverALiveImageFailsWhereThePlatformHasThatConstraint(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ccdad")
	startRunningImage(t, target)

	err := os.WriteFile(target, []byte("new"), 0o755)
	switch {
	case liveImageIsWriteProtected && err == nil:
		t.Fatalf("writing over a live image succeeded on %s, which is recorded above as "+
			"having the constraint replaceBinary exists for. Either the constraint went "+
			"away or startRunningImage stopped producing a live image, and under both of "+
			"those the test below proves less than its name claims.", runtime.GOOS)
	case !liveImageIsWriteProtected && err != nil:
		t.Fatalf("writing over a live image failed on %s (%v), which is recorded above as "+
			"having no such constraint. That record is what makes this file lenient here, "+
			"so it has to be corrected rather than left to pass quietly.", runtime.GOOS, err)
	}
}

// The claim, and this one holds on every platform the file builds for:
// replaceBinary puts the staged bytes at target even when target is a live
// image, and consumes the staged file doing it. On linux that is only possible
// because it renames rather than writes — which is what the control above
// proves, and the entire reason the staged file is a sibling of the target
// rather than something in /tmp that gets copied over. On darwin a write would
// have worked too; the rename is still what runs, and this is what says it runs
// correctly there.
func TestReplaceBinaryReplacesALiveImage(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ccdad")
	startRunningImage(t, target)

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
//
// Staged is written at 0o750, not the 0o755 an implementation might
// hardcode instead of propagating: with 0o755 on both sides, a
// replaceBinary that ignores the staged file's mode and just chmods the
// target to 0o755 by itself would satisfy this test by coincidence. 0o750
// is not a mode replaceBinary or anything downstream produces on its own,
// so matching it can only mean the mode really travelled with the file.
func TestReplaceBinaryKeepsTheStagedMode(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ccdad")
	staged := filepath.Join(dir, "ccdad-staged")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new"), 0o750); err != nil {
		t.Fatal(err)
	}
	// Load-bearing, not redundant with the mode WriteFile was just given, but
	// only under a STRICTER umask than the common 022: 0o750&^0o022 == 0o750,
	// since 022 clears only the group-write and other-write bits and 0o750
	// has neither set. A umask of 0o077 -- one a security-conscious shell or
	// CI runner may set -- clears every group and other bit instead, so
	// WriteFile alone would leave staged at 0o750&^0o077 == 0o700 and this
	// Chmod is what pulls the group-read/execute bits back.
	if err := os.Chmod(staged, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := replaceBinary(staged, target); err != nil {
		t.Fatalf("replaceBinary(): %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o750 {
		t.Errorf("mode = %v, want -rwxr-x---", info.Mode().Perm())
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

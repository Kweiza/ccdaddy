//go:build windows

package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
)

// The Windows half of `ccdad update`, executed against images Windows has
// really mapped. startRunningBinary and longPath live in
// uninstall_windows_test.go; this is the same package, so they are used rather
// than copied.
//
// The controls come FIRST and they are the load-bearing part. Without them this
// file proves that a rename works, which is not the claim: the claim is that a
// rename works where the two obvious alternatives do not, and both of those
// claims sit in a comment in update_windows.go where nothing executes them.

// Control one: renaming ONTO a live image fails. This is the asymmetry
// replaceBinary is built on — the live image can be renamed AWAY, and cannot be
// renamed OVER.
func TestWindowsAPlainRenameOverALiveImageFails(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ccdad.exe")
	startRunningBinary(t, target)

	staged := filepath.Join(dir, "staged.exe")
	if err := os.WriteFile(staged, []byte("MZ not really a binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, target); err == nil {
		t.Fatal("os.Rename replaced a live image; replaceBinary's whole two-step dance " +
			"exists for a constraint this machine does not have, and every claim below is void")
	}
	// And the half that DOES work, which is the one the implementation uses.
	if err := os.Rename(target, filepath.Join(dir, "aside.exe")); err != nil {
		t.Fatalf("a live image could not be renamed away: %v", err)
	}
}

// Control two: cclink.WriteFileAtomic fails AND spends its whole backoff. This
// is the executable form of a claim that is otherwise only an assertion in a
// comment — its retry loop treats ERROR_ACCESS_DENIED as transient, which is
// permanent for a mapped image, so it burns ten attempts and nine sleeps of
// 2+4+8+16+32+64+128+250+250 ms.
func TestWindowsWriteFileAtomicIsNotTheHelperForThis(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ccdad.exe")
	startRunningBinary(t, target)

	start := time.Now()
	err := cclink.WriteFileAtomic(target, []byte("new"), 0o755)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("WriteFileAtomic replaced a live image")
	}
	const wholeBackoff = 754 * time.Millisecond
	if elapsed < wholeBackoff {
		t.Errorf("WriteFileAtomic failed after %v, want at least %v — it is supposed to burn "+
			"every retry on an error its classifier calls transient and this one is not", elapsed, wholeBackoff)
	}
	// Best-effort cleanup of the orphan WriteFileAtomic's own doc comment
	// documents: its deferred os.Remove of the temp file can itself lose to a
	// scanner holding that file, which is the same class of transient handle
	// this whole test is about, just landing on the temp name instead of the
	// target. Nothing else in this file globs for it, so if the deferred
	// remove already won, this loop simply finds nothing.
	orphans, _ := filepath.Glob(filepath.Join(dir, cclink.TempPattern(target)))
	for _, o := range orphans {
		_ = os.Remove(o)
	}
}

// The real one, over a live image: it succeeds, and it leaves exactly one
// aside, because the delete cannot take a file the old process still holds.
func TestWindowsReplaceBinaryOverALiveImageLeavesOneAside(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ccdad.exe")
	startRunningBinary(t, target)

	staged := filepath.Join(dir, "staged.exe")
	if err := os.WriteFile(staged, []byte("the new ccdad"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceBinary(staged, target); err != nil {
		t.Fatalf("replaceBinary() over a live image: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "the new ccdad" {
		t.Errorf("target holds %q, want the staged bytes", got)
	}
	asides, err := filepath.Glob(filepath.Join(dir, ".ccdad-old.*.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if len(asides) != 1 {
		t.Errorf("asides = %v, want exactly one: the old image is still mapped, so the "+
			"best-effort delete must fail rather than the rename", asides)
	}
}

// With nothing holding the old image, the best-effort delete succeeds and the
// directory is left clean. This is the arm that proves the delete is attempted
// at all rather than skipped.
func TestWindowsReplaceBinaryLeavesNoAsideWhenTheOldImageIsNotRunning(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ccdad.exe")
	staged := filepath.Join(dir, "staged.exe")
	if err := os.WriteFile(target, []byte("the old ccdad"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("the new ccdad"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceBinary(staged, target); err != nil {
		t.Fatalf("replaceBinary(): %v", err)
	}
	asides, err := filepath.Glob(filepath.Join(dir, ".ccdad-old.*.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if len(asides) != 0 {
		t.Errorf("asides = %v, want none: nothing holds the old image, so the delete must take it", asides)
	}
}

// Two updates against two live images must not collide. A fixed suffix — which
// is what uninstall_windows.go correctly uses, because uninstall runs once —
// would make the second replacement fail on the first one's leftover.
func TestWindowsTwoReplacementsProduceDistinctAsides(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ccdad.exe")

	for i := range 2 {
		startRunningBinary(t, target)
		staged := filepath.Join(dir, "staged.exe")
		if err := os.WriteFile(staged, []byte(strings.Repeat("n", i+1)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := replaceBinary(staged, target); err != nil {
			t.Fatalf("replaceBinary() run %d: %v", i+1, err)
		}
	}
	asides, err := filepath.Glob(filepath.Join(dir, ".ccdad-old.*.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if len(asides) != 2 {
		t.Fatalf("asides = %v, want two distinct names", asides)
	}
	if asides[0] == asides[1] {
		t.Errorf("both replacements produced %q; the suffix is not random", asides[0])
	}
}

// The rollback. A second rename that fails must put the old binary back, or the
// machine is left with no ccdad at all — a worse outcome than a machine still
// on the old version.
func TestWindowsAFailedSecondRenameRestoresTheOldBinary(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ccdad.exe")
	if err := os.WriteFile(target, []byte("the old ccdad"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A staged file that is not there is the cheapest way to make the second
	// rename fail after the first one has already succeeded.
	err := replaceBinary(filepath.Join(dir, "absent.exe"), target)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("replaceBinary() error = %v, want it to wrap fs.ErrNotExist", err)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("the old binary was not put back: %v", readErr)
	}
	if string(got) != "the old ccdad" {
		t.Errorf("target = %q, want the old bytes", got)
	}
	asides, _ := filepath.Glob(filepath.Join(dir, ".ccdad-old.*.exe"))
	if len(asides) != 0 {
		t.Errorf("the rollback left %v behind", asides)
	}
}

// Mark-of-the-Web, with a positive control so it cannot pass by Go simply being
// unable to address an alternate data stream on this filesystem.
func TestWindowsTheReplacedBinaryCarriesNoZoneIdentifier(t *testing.T) {
	dir := t.TempDir()

	// The control. If this fails, the test below is checking nothing — and it
	// must fail LOUDLY rather than skip: GitHub's Windows runners give NTFS
	// for both the workspace and TEMP, so alternate data streams are always
	// available here, and a t.Skipf would let a real filesystem change silently
	// turn this test into one that always reports success without looking.
	control := filepath.Join(dir, "control.exe")
	if err := os.WriteFile(control, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(control+":Zone.Identifier", []byte("[ZoneTransfer]\r\nZoneId=3\r\n"), 0o644); err != nil {
		t.Fatalf("this filesystem does not carry alternate data streams (%v), so the assertion below would be vacuous", err)
	}
	if _, err := os.Stat(control + ":Zone.Identifier"); err != nil {
		t.Fatalf("a Zone.Identifier written by this test cannot be read back (%v); the assertion below would be vacuous", err)
	}

	staged := filepath.Join(dir, "staged.exe")
	if err := os.WriteFile(staged, []byte("the new ccdad"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "ccdad.exe")
	if err := replaceBinary(staged, target); err != nil {
		t.Fatalf("replaceBinary(): %v", err)
	}
	if _, err := os.Stat(target + ":Zone.Identifier"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the replaced binary carries a zone marking (stat said %v); install.ps1 runs "+
			"Unblock-File for this and the Go path is supposed not to need it", err)
	}
	// longPath is used nowhere above, and that is deliberate: nothing here
	// compares a path against a spelling Windows chose. Keep it referenced so a
	// later assertion that DOES need it does not have to rediscover it.
	_ = longPath
}

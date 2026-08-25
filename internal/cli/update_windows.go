//go:build windows

package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// replaceBinary puts staged at target on Windows, where a running .exe cannot
// be overwritten or deleted but CAN be renamed — and the rename is the step
// that matters.
//
// It is a port of install.ps1's Install-CcdadBinary plus a rollback the
// original does not need. install.ps1 removes the aside immediately, and that
// is right there: a fresh install that fails should leave the machine as it
// found it. Here the user had a working binary a moment ago, so the aside IS
// the rollback target, and deleting it first would destroy exactly the file the
// rollback needs. The delete therefore comes AFTER the second rename, and it is
// still best-effort: it fails for precisely the upgrade that needed the rename,
// because the old process still holds the image. Every upgrade over a live
// process leaves a .ccdad-old.*.exe behind, nothing sweeps them, and uninstall
// does not know the name — it uses a different one. That is accepted.
//
// cclink.WriteFileAtomic looks like exactly the right function and is a trap.
// Its retry loop classifies errors through winerr.Retryable, which reports true
// for ERROR_ACCESS_DENIED — the transient antivirus-or-indexer case it was
// written for. Renaming onto a running .exe returns that same error
// PERMANENTLY, so it burns all ten attempts and 754 ms of backoff and then
// fails anyway. Two secondary reasons, true on Unix as well: its signature
// takes the whole file as a []byte, and its temporary name is
// cclink.TempPattern(target) — filepath.Base plus the suffix, so ccdad.exe.tmp-*
// and not ccdad.tmp-*, which is a name it cannot produce. Spell it with the
// helper rather than by hand; the writer and daemon.SweepStatusTemps each held
// their own copy of that suffix once, and changing one left the suite green
// with the sweep collecting nothing. The temp is also a
// non-dotfile briefly sitting in a directory on the user's PATH.
//
// The aside stays in the install directory rather than %TEMP%, which is
// routinely on another volume; a cross-volume move is a copy, and a mapped
// image does not permit one.
//
// Mark-of-the-Web needs no handling. install.ps1 runs Unblock-File because the
// zone marking survives its move; the path here is net/http into os.Create into
// io.Copy — CreateFileW and WriteFile and nothing else — so no Zone.Identifier
// stream is ever created, and os.Rename preserves none.
func replaceBinary(staged, target string) error {
	aside := ""
	// Lstat rather than Stat: a dangling symlink at the target is still an
	// entry that has to be moved out of the way, and Stat would report it
	// missing and then fail the rename.
	if _, err := os.Lstat(target); err == nil {
		suffix, err := randomSuffix()
		if err != nil {
			return err
		}
		// The suffix is RANDOM. uninstall_windows.go uses a fixed name and is
		// right to — uninstall runs once. Two updates a minute apart would
		// collide. os.CreateTemp is not a substitute: it CREATES the file, and
		// renaming onto a name it just created races itself.
		aside = filepath.Join(filepath.Dir(target), ".ccdad-old."+suffix+".exe")
		// Declared before the branch so the rollback below can see it.
		if err := os.Rename(target, aside); err != nil {
			return fmt.Errorf("moving %s aside: %w", target, err)
		}
	}
	if err := os.Rename(staged, target); err != nil {
		if aside != "" {
			// Put it back. A machine with no ccdad at all is a worse outcome
			// than a machine still running the old one.
			_ = os.Rename(aside, target)
		}
		return fmt.Errorf("moving %s into place at %s: %w", staged, target, err)
	}
	if aside != "" {
		_ = os.Remove(aside)
	}
	return nil
}

// randomSuffix names one aside. Eight bytes of crypto/rand rather than a
// counter or a timestamp: two `ccdad update` runs a minute apart must not
// collide, and there is no shared state between two processes to count in.
func randomSuffix() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("naming a temporary file beside the ccdad binary: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

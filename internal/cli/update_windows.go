//go:build windows

package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// replaceBinary puts staged at target on Windows, where a running .exe cannot
// be overwritten or deleted but CAN be renamed — and the rename is the step
// that matters.
//
// It is a port of install.ps1's Install-CcdadBinary plus a rollback the
// original does not have. install.ps1's own aside dance is gated on
// Test-Path -LiteralPath $Target, so it runs only in the same case this
// function handles -- a binary already at the destination -- and never on a
// fresh install. In that case it deletes the aside immediately, before the
// second Move-Item lands the new binary; a failure on that second move then
// has nothing to restore from, because the working copy is already gone.
// That is the same rollback hole this function closes: the delete here
// happens AFTER the second rename succeeds, and it is still best-effort --
// it fails for precisely the upgrade that needed the rename, because the old
// process still holds the image. Every upgrade over a live process leaves a
// .ccdad-old.*.exe behind, nothing sweeps them, and uninstall does not know
// the name — it uses a different one. That is accepted.
//
// cclink.WriteFileAtomic looks like exactly the right function and is a trap.
// Its retry loop classifies errors through winerr.Retryable, which reports true
// for ERROR_ACCESS_DENIED — the transient antivirus-or-indexer case it was
// written for. Renaming onto a running .exe returns that same error
// PERMANENTLY, so it burns all ten attempts and 754 ms of backoff and then
// fails anyway. Two secondary reasons, true on Unix as well: its signature
// takes the whole file as a []byte, and its temporary name is
// cclink.TempPattern(target) — filepath.Base plus the suffix, so ccdad.exe.tmp-*
// and not ccdad.tmp-*, which is a name it cannot produce.
//
// The aside stays in the install directory rather than %TEMP%, which is
// routinely on another volume; a cross-volume move is a copy, and a mapped
// image does not permit one.
//
// Mark-of-the-Web needs no handling. install.ps1 runs Unblock-File on the
// asset BEFORE Install-CcdadBinary's Move-Item, and says why right above that
// call: the marking survives a move, so it has to be stripped first or
// SmartScreen acts on it the first time the binary runs. Here there is
// nothing to strip: net/http into os.Create into io.Copy is CreateFileW and
// WriteFile and nothing else, so staged never has a Zone.Identifier stream in
// the first place. (That "never creates one" claim is about the download
// path, not about this function -- internal/release proves it directly,
// against a real fetch, rather than this comment asserting it.)
func replaceBinary(staged, target string) error {
	// aside is set below when something already occupies target; the
	// rollback branch after the second rename reads it.
	aside := ""

	// Lstat rather than Stat: on Windows a dangling symlink or junction at
	// the target is still a directory entry, and Lstat reports it as
	// present where Stat would follow it, find nothing at the resolved
	// path, and report the target itself as absent -- which would skip the
	// aside step and rename straight over the stale link instead of moving
	// it out of the way first. The rename that follows succeeds either way:
	// like POSIX rename(2), it replaces the link entry itself and does not
	// dereference it.
	_, lstatErr := os.Lstat(target)
	switch {
	case lstatErr == nil:
		suffix, err := randomSuffix()
		if err != nil {
			return err
		}
		// The suffix is RANDOM. uninstall_windows.go uses a fixed name and is
		// right to — uninstall runs once. Two updates a minute apart would
		// collide. os.CreateTemp is not a substitute: it CREATES the file, and
		// renaming onto a name it just created races itself.
		aside = filepath.Join(filepath.Dir(target), ".ccdad-old."+suffix+".exe")
		if err := os.Rename(target, aside); err != nil {
			return fmt.Errorf("moving %s aside: %w", target, err)
		}
	case errors.Is(lstatErr, fs.ErrNotExist):
		// Nothing at target; the plain rename below is all that is needed.
	default:
		// A permissions error on the install directory, surfaced here
		// instead of a dozen lines down as a failure on the SECOND rename,
		// whose message would name staged and target and say nothing about
		// the real cause.
		return fmt.Errorf("checking for an existing binary at %s: %w", target, lstatErr)
	}

	if err := os.Rename(staged, target); err != nil {
		if aside != "" {
			// Put it back. A machine with no ccdad at all is a worse outcome
			// than a machine still running the old one. If the rollback
			// itself fails, the working binary is stranded at aside rather
			// than silently lost -- name that path in the error so the user
			// can recover it by hand, the choice uninstall_windows.go makes
			// for its own privileged step when it can fail after the point
			// of no return.
			if rollbackErr := os.Rename(aside, target); rollbackErr != nil {
				return fmt.Errorf("moving %s into place at %s: %w (and restoring the previous binary from %s also failed: %v; move it back by hand)",
					staged, target, err, aside, rollbackErr)
			}
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

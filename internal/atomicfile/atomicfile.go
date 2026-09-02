// Package atomicfile writes a file through a sibling temp file and a rename,
// so a concurrent reader sees one whole version or another and never a torn
// one.
//
// It is a leaf, and that is the point. This body used to live in
// internal/cclink beside the credentials file it was written for, which meant
// every package that needed an atomic write took a dependency on the package
// that reads and rewrites Claude Code's login. A switch that must never touch
// that login cannot prove it while importing the package that does, so the
// writer moved out and cclink keeps a wrapper.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Kweiza/ccdaddy/internal/winerr"
)

// replaceAttempts and the backoff bounds exist for Windows, where antivirus
// scanners and the search indexer hold transient handles on a file being
// replaced. Measured, roughly 44% of replaces hit one. On Unix the retry
// never triggers because winerr.Retryable always reports false there.
const (
	replaceAttempts   = 10
	replaceBackoffMin = 2 * time.Millisecond
	replaceBackoffMax = 250 * time.Millisecond
)

// createTemp is a seam so a test can prove the temp file is created beside
// the target — a sibling — rather than in the system temp directory. Nothing
// in this package assigns to it outside a test.
var createTemp = os.CreateTemp

// tempSuffix is the tail of the temp file's name. It is spelled once, here,
// and reached only through TempPattern.
const tempSuffix = ".tmp-*"

// TempPattern returns the name of the sibling temp file WriteFile writes
// beside path, as a pattern.
//
// The returned string is simultaneously an os.CreateTemp pattern and a
// filepath.Glob pattern — both read `*` as "the part that varies" — and that is
// the whole point of exporting it. A rename that never completed strands the
// temp file, and whatever collects those orphans has to name them;
// daemon.SweepStatusTemps is the one that does. Deriving its glob from here
// rather than spelling the suffix a second time is what stops the writer and
// the sweeper from drifting apart. They were two literals under a comment
// claiming they were one, nothing executed that claim, and changing the
// writer's left the whole suite green with the sweep collecting nothing.
//
// It takes the full path rather than a base name so a caller cannot hand it the
// wrong half; only the base is used, so the result names one directory entry
// and never contains a separator.
//
// The two readings part company for a base name holding a glob metacharacter —
// `*`, `?`, `[` — which os.CreateTemp takes literally and filepath.Match does
// not. Nothing sweeps such a file: the only sweeper names its files with
// constants, and the two callers that take a path from the user
// (`export --out`, `runway --out`) have no sweeper at all.
func TempPattern(path string) string { return filepath.Base(path) + tempSuffix }

// renameFile is a seam so a test can drive the replace-retry loop with a
// deterministic failure sequence instead of depending on an actual Windows
// sharing violation.
var renameFile = os.Rename

// retryable reports whether a failed rename is worth retrying. It defaults to
// winerr.Retryable; a test may swap it to make the retry loop's own
// bookkeeping (attempt count, giving up) observable without depending on
// platform-specific errors.
var retryable = winerr.Retryable

// syncFile flushes a file's contents to disk. It is a seam so a test can pin
// that the sync happens BEFORE the rename: crash durability itself is not
// observable from a Go test, but a refactor deleting the call is, and without
// the sync a crash between write and rename can leave the renamed file present
// but empty -- which reads as a corrupt credential store.
var syncFile = (*os.File).Sync

// WriteFile writes data to path via a sibling temp file and a rename.
//
// The temp file MUST be a sibling: a rename within one directory is atomic, so
// a concurrent reader sees either the whole old file or the whole new one.
// Writing to the system temp directory and moving across filesystems degrades
// to copy-then-unlink, where a reader can catch a half-written credential file.
//
// The rename also changes the inode and advances the mtime, which is exactly
// what Claude Code's change probe watches, so a switch hot-reloads a running
// session with no restart.
//
// On Windows an antivirus scanner or the search indexer may hold the target
// file open when the rename is attempted; that is retried per winerr.Retryable.
// If the temp file itself is still held after every retry, it can survive as
// an orphan — accepted here, since guarding against it needs a second retry
// loop for a rarer failure than the one this function already handles. An
// orphan is named by TempPattern, which is what a sweeper globs.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)

	tmp, err := createTemp(dir, TempPattern(path))
	if err != nil {
		return fmt.Errorf("creating temp file beside %s: %w", filepath.Base(path), err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	// fsync makes the temp file's data durable before the rename swaps it in,
	// so a crash right after a successful rename cannot leave the target
	// present but empty. It does not make the rename itself durable — without
	// an additional fsync on the containing directory, a crash could still
	// lose the directory-entry update and revert to the old file. That gap is
	// accepted: the failure mode is benign, since the old credentials survive
	// intact rather than becoming corrupt.
	if err := syncFile(tmp); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("setting mode on temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}

	backoff := replaceBackoffMin
	for attempt := 1; ; attempt++ {
		err := renameFile(tmpName, path)
		if err == nil {
			return nil
		}
		if attempt >= replaceAttempts || !retryable(err) {
			return fmt.Errorf("replacing %s: %w", filepath.Base(path), err)
		}
		time.Sleep(backoff)
		if backoff *= 2; backoff > replaceBackoffMax {
			backoff = replaceBackoffMax
		}
	}
}

// SwapRename replaces the rename step for the duration of one test and returns
// a function that puts the original back.
//
// It is exported for exactly one caller: internal/cclink's global-config test
// asserts that Claude Code's config lock is HELD at the moment of the rename,
// not merely around the call, and the rename now happens in this package. That
// assertion is the one that separates an implementation which read under the
// lock, released, and then wrote -- which would satisfy every other test in
// that file while racing Claude Code's own rewrite of the file on startup.
//
// It is not safe for concurrent use and must not be called outside a test.
func SwapRename(f func(oldpath, newpath string) error) (restore func()) {
	prev := renameFile
	renameFile = f
	return func() { renameFile = prev }
}

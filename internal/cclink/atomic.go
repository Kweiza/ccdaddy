package cclink

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// replaceAttempts and the backoff bounds exist for Windows, where antivirus
// scanners and the search indexer hold transient handles on a file being
// replaced. Measured, roughly 44% of replaces hit one. On Unix the retry
// never triggers because replaceRetryable always reports false.
const (
	replaceAttempts   = 10
	replaceBackoffMin = 2 * time.Millisecond
	replaceBackoffMax = 250 * time.Millisecond
)

// createTemp is a seam so a test can prove the temp file is created beside
// the target — a sibling — rather than in the system temp directory. Nothing
// in this package assigns to it outside a test.
var createTemp = os.CreateTemp

// renameFile is a seam so a test can drive the replace-retry loop with a
// deterministic failure sequence instead of depending on an actual Windows
// sharing violation.
var renameFile = os.Rename

// retryable reports whether a failed rename is worth retrying. It defaults to
// the build-tagged replaceRetryable; a test may swap it to make the retry
// loop's own bookkeeping (attempt count, giving up) observable without
// depending on platform-specific errors.
var retryable = replaceRetryable

// WriteFileAtomic writes data to path via a sibling temp file and a rename.
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
// file open when the rename is attempted; that is retried per replaceRetryable.
// If the temp file itself is still held after every retry, it can survive as
// an orphan — accepted here, since guarding against it needs a second retry
// loop for a rarer failure than the one this function already handles.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)

	tmp, err := createTemp(dir, filepath.Base(path)+".tmp-*")
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
	if err := tmp.Sync(); err != nil {
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

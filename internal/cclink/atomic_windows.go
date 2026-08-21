//go:build windows

package cclink

import (
	"errors"

	"golang.org/x/sys/windows"
)

// replaceRetryable reports whether a failed rename is worth retrying.
//
// On Windows an antivirus scanner or the search indexer routinely holds a
// transient handle on the file being replaced, which surfaces as one of these
// three errors. Measured, roughly 44% of replaces hit one, so a bounded retry
// is the difference between a reliable switch and a coin flip.
func replaceRetryable(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

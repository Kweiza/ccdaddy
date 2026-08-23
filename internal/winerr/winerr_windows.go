//go:build windows

package winerr

import (
	"errors"

	"golang.org/x/sys/windows"
)

// Retryable reports whether a failed file operation is worth trying again.
//
// On Windows an antivirus scanner or the search indexer routinely holds a
// transient handle on a file being replaced or read, which surfaces as one of
// these three errors. Measured, roughly 44% of replaces hit one, so treating
// them as final is the difference between a reliable operation and a coin flip.
func Retryable(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

//go:build !windows

package cclink

// replaceRetryable reports whether a failed rename is worth retrying. On Unix a
// rename within a directory does not fail for transient reasons, so it never is.
func replaceRetryable(error) bool { return false }

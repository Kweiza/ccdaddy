//go:build !windows

package winerr

// Retryable reports whether a failed file operation is worth trying again. The
// conditions it names have no Unix counterpart -- a rename within a directory
// does not fail for transient reasons, and an open does not fail because
// another process happens to have the file open -- so off Windows it never is.
func Retryable(error) bool { return false }

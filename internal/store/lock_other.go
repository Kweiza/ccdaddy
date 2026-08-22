//go:build !unix

package store

// locksUnsupported has no errno cases off unix. Windows reports a filesystem
// that cannot lock through its own error codes, and the platforms gofrs/flock
// does not implement answer errors.ErrUnsupported, which classifyLockError
// already recognises. syscall does not define ENOLCK on every GOOS, so this
// cannot be one file with a runtime branch.
func locksUnsupported(error) bool { return false }

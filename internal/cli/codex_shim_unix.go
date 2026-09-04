//go:build !windows

package cli

import (
	"io/fs"
	"syscall"
)

// shimFileOwner reports the uid that owns the file info describes.
//
// A file of its own because there is no portable way to ask. On Unix the answer
// is in the *syscall.Stat_t behind FileInfo.Sys(); on Windows Sys() is a
// *syscall.Win32FileAttributeData with no uid in it at all, and syscall.Stat_t
// is not even a type there -- writing this inline would stop the package
// compiling on an OS it is compiled on.
//
// The second return is "the platform said", not "the file has an owner". Nothing
// on Windows reaches this question: the shim is refused before a file is
// created, and `ccdad doctor`'s row is skipped before one is stat'ed. The
// windows half answers anyway rather than guessing a uid, and the callers treat
// an unanswered owner as this user's -- which is what it is on every machine
// that reaches the question at all.
func shimFileOwner(info fs.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

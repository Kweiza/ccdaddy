//go:build windows

package cli

import "io/fs"

// shimFileOwner has no answer on Windows, and says so rather than guessing one.
//
// FileInfo.Sys() is a *syscall.Win32FileAttributeData here, which carries times
// and attributes and no owner; the owner is a SID reachable only through the
// security API, and it does not compare to a uid. Nothing reaches this: there is
// no shim on Windows, so install refuses before a file is created and
// `ccdad doctor`'s codex-shim row is skipped before one is stat'ed. The unix
// half of the pair is where the question is answered.
func shimFileOwner(fs.FileInfo) (int, bool) { return 0, false }

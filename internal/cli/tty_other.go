//go:build !windows

package cli

import "os"

// setConsoleVT is a no-op where a terminal needs no permission to be a
// terminal. Only the Windows console has a mode bit that has to be widened
// before an escape sequence means anything.
func setConsoleVT(*os.File) error { return nil }

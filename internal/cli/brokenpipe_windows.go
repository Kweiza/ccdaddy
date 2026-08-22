//go:build windows

package cli

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isBrokenPipe reports whether err is a write to a stdout whose reader has
// gone away.
//
// Windows has neither SIGPIPE nor EPIPE. A write to a pipe the reader has
// closed answers ERROR_BROKEN_PIPE (109) or ERROR_NO_DATA (232) -- "The pipe
// has been ended" and "The pipe is being closed", which is which depending on
// whether the reader closed its handle or the whole pipe is tearing down --
// and syscall.Errno.Is maps NEITHER to syscall.EPIPE: its whole switch is
// ErrPermission, ErrExist, ErrNotExist and ErrUnsupported.
//
// So the single `errors.Is(err, syscall.EPIPE)` this replaced was simply false
// on Windows, and `ccdad list --json | head -1` printed "ccdad: writing
// output: The pipe is being closed." and exited non-zero -- against §9.3 and
// against the plan's global "EPIPE on stdout is not an error; it ends the run
// at exit 0". Nothing noticed, because until the three-OS matrix existed
// nothing in this repository had ever run on Windows.
func isBrokenPipe(err error) bool {
	return errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_NO_DATA)
}

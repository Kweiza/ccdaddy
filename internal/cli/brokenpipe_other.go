//go:build !windows

package cli

import (
	"errors"
	"syscall"
)

// isBrokenPipe reports whether err is a write to a stdout whose reader has
// gone away. EPIPE is the errno here, and ignoreSIGPIPE is what makes the
// write return it rather than killing the process.
func isBrokenPipe(err error) bool { return errors.Is(err, syscall.EPIPE) }

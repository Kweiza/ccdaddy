//go:build !windows && !plan9

package cli

import (
	"os"
	"os/signal"
	"syscall"
)

// ignoreSIGPIPE makes a write to a closed stdout return EPIPE instead of
// killing the process.
//
// Go's runtime treats SIGPIPE on file descriptors 1 and 2 specially: without
// this, the signal is delivered with its default disposition and the process
// dies with status 141, so the write never returns an error and ExecuteWith's
// EPIPE branch is unreachable dead code. The exit contract requires the
// opposite: EPIPE is not an error, and `ccdad list --json | head -1` exits 0.
// That held only by accident, for outputs small enough to fit the pipe buffer.
//
// signal.Notify (rather than signal.Ignore) is what flips the runtime into
// returning EPIPE; the channel is never read, which is fine because the signal
// carries no information we want beyond the write error itself.
func ignoreSIGPIPE() {
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
}

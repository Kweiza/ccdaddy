//go:build !windows

package cli

import "syscall"

// errBrokenPipeForTest is what a closed reader answers with on this platform.
// A test that hard-codes syscall.EPIPE passes on Windows without asserting
// anything, because nothing there ever produces it.
var errBrokenPipeForTest error = syscall.EPIPE

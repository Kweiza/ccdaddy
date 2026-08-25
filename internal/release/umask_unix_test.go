//go:build !windows

package release

import "syscall"

// syscallUmask sets the process umask and returns the previous value. It is
// process-wide and therefore not parallel-safe; the one test that uses it does
// not call t.Parallel.
func syscallUmask(mask int) int { return syscall.Umask(mask) }

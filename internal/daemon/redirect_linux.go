package daemon

import (
	"os"
	"syscall"
)

// platformRedirectStderr duplicates f onto descriptor 2.
//
// Dup3 rather than Dup2: linux/arm64 and linux/riscv64 have no dup2 syscall at
// all, and syscall.Dup2 is therefore undefined there. Dup3 with no flags is the
// same operation on every linux ccdad targets.
//
// This is the standard library, not golang.org/x/sys — production code in this
// package deliberately reaches no further than syscall on unix.
func platformRedirectStderr(f *os.File) error {
	return syscall.Dup3(int(f.Fd()), int(stderrFD()), 0)
}

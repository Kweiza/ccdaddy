//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package daemon

import (
	"os"
	"syscall"
)

// platformRedirectStderr duplicates f onto descriptor 2. The BSDs have dup2 and
// no dup3, which is the mirror image of linux/arm64.
func platformRedirectStderr(f *os.File) error {
	return syscall.Dup2(int(f.Fd()), int(stderrFD()))
}

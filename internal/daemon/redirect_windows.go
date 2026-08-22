package daemon

import (
	"os"

	"golang.org/x/sys/windows"
)

// platformRedirectStderr repoints the process's standard error handle.
//
// Windows has no dup2, and SetStdHandle is not in the standard library's
// syscall package — golang.org/x/sys/windows is already a dependency of this
// module and is the only way to reach it. Reassigning os.Stderr as well is not
// belt and braces: SetStdHandle moves what GetStdHandle answers, which is what
// the runtime uses for a panic, while os.Stderr is a *File captured at startup
// around the ORIGINAL handle and would otherwise keep writing to the null
// device Spawn handed the child.
func platformRedirectStderr(f *os.File) error {
	if err := windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(f.Fd())); err != nil {
		return err
	}
	os.Stderr = f
	return nil
}

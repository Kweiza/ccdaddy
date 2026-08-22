//go:build unix

package daemon

import "golang.org/x/sys/unix"

// sessionID reports the calling process's session.
//
// syscall.Getsid does not exist in the standard library — only Getpgid,
// Getpgrp, Getppid and Setsid do — so the real assertion needs
// golang.org/x/sys/unix. It is already a direct dependency, and this keeps the
// import on the TEST side: nothing in the package's production code reaches
// outside the standard library on unix.
//
// The weaker substitute would be Getpgrp() == Getpid(), which proves a new
// process GROUP. Setsid creates both, so that would pass without ever showing
// that the child left the controlling terminal — which is the entire point.
func sessionID() (int, error) { return unix.Getsid(0) }

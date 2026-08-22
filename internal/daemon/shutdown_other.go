//go:build !unix

package daemon

import "errors"

// requestShutdown refuses on platforms with no way to reach a detached process.
//
// Windows is one of them until §8.4's named event lands, and that is not a gap
// that can be filled with a pid: signals do not exist there,
// GenerateConsoleCtrlEvent cannot reach a DETACHED_PROCESS child, and killing a
// pid read out of a file is how an unrelated process gets terminated.
//
// Refusing is the point. Returning nil would make `ccdad daemon stop` announce
// a stop that never happened and then wait out its whole timeout on a singleton
// nobody was going to release.
func requestShutdown(int) error { return errors.ErrUnsupported }

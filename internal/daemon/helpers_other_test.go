//go:build !unix

package daemon

import "errors"

// sessionID has no meaning off unix. There is no POSIX session for it to
// return on Windows, where detachment is a console property and a process-group
// property instead — and the console half is now asserted in this package by
// TestSpawnLeavesTheChildWithNoConsole, not, as this comment used to say, by
// the install smoke suite, which never asked about a console at all.
func sessionID() (int, error) { return 0, errors.ErrUnsupported }

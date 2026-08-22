//go:build unix

package daemon

import (
	"errors"
	"syscall"
)

// locksUnsupported recognises the two errnos an NFS or CIFS mount with no lock
// daemon answers with. This is the condition the singleton's three-outcome
// contract exists for: reported as "not running" it would send a supervisor
// into a respawn loop forever.
func locksUnsupported(err error) bool {
	return errors.Is(err, syscall.ENOLCK) || errors.Is(err, syscall.ENOSYS)
}

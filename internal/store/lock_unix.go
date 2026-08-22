//go:build unix

package store

import (
	"errors"
	"syscall"
)

// locksUnsupported recognises the two errnos an NFS or CIFS mount with no lock
// daemon answers with. A store on such a mount refuses to write rather than
// writing unguarded: a network home directory is the case with the MOST
// concurrent writers, so it is the last place to fall back to the race this
// lock exists to close.
func locksUnsupported(err error) bool {
	return errors.Is(err, syscall.ENOLCK) || errors.Is(err, syscall.ENOSYS)
}

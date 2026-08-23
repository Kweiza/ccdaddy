//go:build unix

package credhome

import (
	"errors"
	"syscall"
)

// locksUnsupported recognises the two errnos an NFS or CIFS mount with no lock
// daemon answers with. A home directory on a network mount is the ordinary
// shape this meets: the store can be local while Claude Code's credential home
// is not, so this branch is reachable with the daemon singleton already taken.
func locksUnsupported(err error) bool {
	return errors.Is(err, syscall.ENOLCK) || errors.Is(err, syscall.ENOSYS)
}

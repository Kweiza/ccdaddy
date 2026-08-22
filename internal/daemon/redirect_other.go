//go:build !linux && !windows && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package daemon

import (
	"errors"
	"os"
)

// platformRedirectStderr refuses on a platform nobody has implemented it for,
// rather than silently succeeding and throwing away every crash trace. The same
// posture as detach's fallback.
func platformRedirectStderr(*os.File) error {
	return errors.ErrUnsupported
}

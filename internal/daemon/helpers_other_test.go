//go:build !unix

package daemon

import "errors"

// sessionID has no meaning off unix; Windows detachment is a console and
// process-group property, asserted by the install smoke suite rather than here.
func sessionID() (int, error) { return 0, errors.ErrUnsupported }

//go:build !unix && !windows

package daemon

import (
	"errors"
	"os/exec"
)

// detach refuses on platforms with no way to detach a child.
//
// This file exists so that `go build ./...` stays honest on every GOOS rather
// than only on the six ccdad releases for. wasip1 in particular compiles the
// rest of this repository today, and a two-file split would silently take that
// away: syscall.SysProcAttr exists there but has neither Setsid nor
// CreationFlags, so the failure would be a build error in a file that had
// nothing to do with the change.
func detach(*exec.Cmd) error {
	return errors.ErrUnsupported
}

//go:build windows

package daemon

import (
	"os/exec"
	"syscall"
)

// detach gives the child no console and its own process group. The flag values
// live in spawn.go so a test on any platform can assert them; see the comment
// there for why the set is exactly these two.
func detach(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: flagDetachedProcess | flagCreateNewProcessGroup,
	}
	return nil
}

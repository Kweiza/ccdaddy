//go:build windows

package daemon

import (
	"os/exec"
	"syscall"
)

// detachedProcess is CREATE_NEW_PROCESS_GROUP's counterpart from the Windows
// process creation flags. It is declared here rather than imported so this file
// needs nothing outside the standard library; syscall supplies
// CREATE_NEW_PROCESS_GROUP but not this one.
const detachedProcess = 0x00000008

// detach gives the child no console and its own process group.
//
// DETACHED_PROCESS is the load-bearing flag: without it the child inherits the
// console and dies on CTRL_CLOSE_EVENT when the window closes. The flag set is
// exactly two values on purpose. CREATE_NO_WINDOW is documented as ignored when
// DETACHED_PROCESS is set, and DETACHED_PROCESS combined with
// CREATE_NEW_CONSOLE fails outright — so a belt-and-braces flag set here is a
// startup failure rather than extra safety.
func detach(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: detachedProcess | syscall.CREATE_NEW_PROCESS_GROUP,
	}
	return nil
}

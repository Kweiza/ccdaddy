//go:build unix

package daemon

import (
	"os/exec"
	"syscall"
)

// detach puts the child in a session of its own, which is what severs it from
// the controlling terminal: it survives the terminal closing, and a Ctrl-C in
// the shell that started it does not reach it.
func detach(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return nil
}

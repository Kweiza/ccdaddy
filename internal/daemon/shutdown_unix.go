//go:build unix

package daemon

import (
	"fmt"
	"syscall"
)

// requestShutdown sends SIGTERM, which Run's handler turns into a stop.
//
// SIGTERM and never SIGKILL: the whole reason the daemon traps signals rather
// than dying on them is that a tick killed mid-swap abandons Claude Code's
// three lock directories on disk, where cclock's stale windows are 60 s, 60 s
// and 15 s — so an impatient shutdown wedges Claude Code's own token refresh
// for up to a minute. There is deliberately no escalation to SIGKILL here
// either: a daemon that will not stop is something the user must be told
// about, not something to paper over.
func requestShutdown(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("asking the daemon at pid %d to stop: %w", pid, err)
	}
	return nil
}

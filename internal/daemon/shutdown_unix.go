//go:build unix

package daemon

import (
	"context"
	"errors"
	"fmt"
	"syscall"
)

// requestShutdown sends SIGTERM, which Run's handler turns into a stop.
//
// SIGTERM and never SIGKILL: the whole reason the daemon traps signals rather
// than dying on them is that a tick killed mid-swap abandons Claude Code's
// three lock directories on disk, where cclock's stale windows are 60 s, 60 s
// and 15 s — so an impatient shutdown wedges Claude Code's own token refresh
// for up to a minute.
func requestShutdown(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("asking the daemon at pid %d to stop: %w", pid, err)
	}
	return nil
}

// forceShutdown refuses on Unix, and the refusal is the design.
//
// A daemon that ignores SIGTERM is a bug the user must be told about rather
// than one ccdad papers over, and `kill -9` is one command away when they
// decide otherwise. Windows gets an escalation because it has no equivalent a
// user can safely reach for — see ForceShutdown.
func forceShutdown(shutdownTarget) error { return errors.ErrUnsupported }

// watchShutdownRequest has nothing to do on Unix: signals are the mechanism,
// and watchSignals already listens for them.
func watchShutdownRequest(context.Context, func(), *Logger) {}

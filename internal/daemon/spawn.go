package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// RunArg is the argument that tells a re-executed ccdad it is the daemon and
// must run in the foreground. Without it the child would auto-start a child of
// its own.
//
// It lives here, not in internal/cli, and the direction is not negotiable:
// internal/cli imports internal/daemon, never the reverse, or autostart closes
// an import cycle the moment it lands. The leading underscores keep it out of
// the namespace a user could type by accident.
const RunArg = "__daemon"

// Spawn starts a detached daemon and returns without waiting for it.
//
// Three rules, all of which have their own failure mode:
//
//  1. All three standard descriptors are redirected before Start. A child that
//     inherits the parent's pipes keeps them open, so `V=$(ccdad which)` hangs
//     forever waiting for an EOF that never comes. This matters more here than
//     in most programs because the daemon auto-starts from ANY ccdad command,
//     so every command in the tree would be affected — and the bug is invisible
//     interactively, where the terminal is not a pipe.
//
//  2. Release, never Wait. Wait blocks the CLI for the daemon's whole lifetime;
//     omitting both leaks the process handle on Windows.
//
//  3. os.Executable, never os.Args[0]. The latter may be a bare PATH name or a
//     path relative to a working directory the daemon is about to leave. Note
//     that on Linux this resolves through /proc/self/exe, so after an in-place
//     binary replacement it re-execs the OLD inode — which is why install.sh
//     stops the daemon before replacing the binary rather than after.
//
// Both descriptors point at the null device, including stderr. Handing the
// child a log file opened HERE would look tidier and would break log rotation
// permanently: the daemon would keep writing into the renamed inode while the
// fresh daemon.log stayed empty. The daemon opens its own log, and redirecting
// its own stderr onto that log — so a panic, which goes straight to descriptor
// 2 without passing through any logger — is the log's owner's job.
//
// The environment is inherited whole, because CCDAD_HOME and CLAUDE_CONFIG_DIR
// have to reach the daemon or it would manage a different store than the CLI
// that started it. That inheritance is exactly why autostart must be suppressed
// under `go test`: an unsuppressed spawn detaches a daemon pinned to a
// t.TempDir() that is about to be deleted underneath it.
func Spawn() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating the ccdad binary: %w", err)
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("opening %s: %w", os.DevNull, err)
	}
	defer devnull.Close()

	cmd := exec.Command(exe, RunArg)
	cmd.Stdin = devnull
	cmd.Stdout = devnull
	cmd.Stderr = devnull
	// The daemon outlives whatever directory it was started from, and holding
	// a working directory open keeps a filesystem busy and stops a removable
	// volume unmounting. The root of the volume the binary lives on exists for
	// as long as the binary does. VolumeName is "" on Unix, so this is "/"
	// there and "C:\" on Windows, with no build tag.
	cmd.Dir = filepath.VolumeName(exe) + string(os.PathSeparator)
	if err := detach(cmd); err != nil {
		return fmt.Errorf("detaching the daemon: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the daemon: %w", err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("releasing the daemon's process handle: %w", err)
	}
	return nil
}

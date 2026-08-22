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

// Windows process creation flags, as literals so that a test on any platform
// can assert their values; spawn_windows.go is the only consumer, and a test
// there checks them against the syscall package's own constants — which is the
// half that cannot be written here, since syscall defines neither off Windows.
//
// DETACHED_PROCESS is the load-bearing one: without it the child inherits the
// console and dies on CTRL_CLOSE_EVENT. The pair is deliberately exactly two.
// CREATE_NO_WINDOW is documented as ignored when DETACHED_PROCESS is set, and
// DETACHED_PROCESS with CREATE_NEW_CONSOLE fails outright, so a
// belt-and-braces flag set is a startup failure rather than extra safety.
const (
	flagDetachedProcess       = 0x00000008
	flagCreateNewProcessGroup = 0x00000200
)

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
//  3. os.Executable, never os.Args[0]. The latter may be a bare PATH name, or
//     a path relative to a working directory this function is about to leave —
//     cmd.Dir is set below, so a relative argv[0] resolves against the wrong
//     directory and the spawn fails with an ENOENT naming a path that exists.
//
//     An earlier version of this comment claimed that /proc/self/exe makes
//     Linux re-exec the OLD inode after an in-place binary replacement. That
//     is wrong, and the probe is easy: os.Executable returns a path STRING and
//     exec re-resolves it at fork time, so after a rename-over the child is
//     the new binary. What IS true is that once the binary has been unlinked,
//     readlink yields "<path> (deleted)", Go strips that suffix and reports no
//     error, and the failure surfaces one line later as an ENOENT. install.sh
//     stops the daemon before replacing the binary for its own stated reason —
//     otherwise the old daemon keeps running old code and holding the
//     singleton — and not because of anything about inodes.
//
// All three descriptors point at the null device, including stderr. Handing the
// child a log file opened HERE would look tidier and would break log rotation
// permanently: the daemon would keep writing into the renamed inode while the
// fresh daemon.log stayed empty. The daemon opens its own log, and redirecting
// its own stderr onto that log — so a panic, which goes straight to descriptor
// 2 without passing through any logger — is the log's owner's job.
//
// The environment is inherited, because CCDAD_HOME and CLAUDE_CONFIG_DIR have
// to reach the daemon or it would manage a different store than the CLI that
// started it — but inherited through ChildEnv rather than wholesale, so the
// paths ccpath resolves at call time are pinned to what they resolved to HERE.
// cmd.Dir below is about to move the child to the root of the volume, which is
// what makes a relative override resolve to a different directory in the child
// than in the parent. That same inheritance is why auto-start must be
// suppressed under `go test`: an unsuppressed spawn detaches a daemon pinned to
// a t.TempDir() that is about to be deleted underneath it.
func Spawn() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating the ccdad binary: %w", err)
	}
	env, err := ChildEnv()
	if err != nil {
		return err
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("opening %s: %w", os.DevNull, err)
	}
	defer devnull.Close()

	cmd := exec.Command(exe, RunArg)
	cmd.Env = env
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

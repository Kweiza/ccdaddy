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

// ProbeArg and the three flag names are the command line one probe is run
// with. They live here for the same reason RunArg does: internal/cli imports
// internal/daemon and never the reverse, so this package is the only place a
// name can be shared by the command that DECLARES the flag and the code that
// SPELLS it. Two copies of a flag name is a rename that silently stops working.
//
// The names are written without their leading dashes because that is the form
// cobra's Flags() takes; the caller that builds an argv adds them.
const (
	ProbeArg       = "probe"
	ProbeUUIDFlag  = "uuid"
	ProbeModelFlag = "model"
	ProbeForceFlag = "force"
)

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

// SpawnFrom starts a detached daemon from exe and returns without waiting for
// it. exe may be "", which means "resolve it with os.Executable", and that is
// what Spawn below passes.
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
func SpawnFrom(exe string) error {
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return fmt.Errorf("locating the ccdad binary: %w", err)
		}
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

// Spawn starts a detached daemon from this binary, which is SpawnFrom("").
//
// The parameter exists for `ccdad update`: it has just written a new binary and
// wants the process that comes back to be THAT file rather than whatever
// os.Executable resolves to a moment later. It is not required for correctness,
// and rule 3 above says why — os.Executable hands exec a path STRING that is
// re-resolved at fork time, so a rename-over is already invisible to a later
// spawn. What it removes is the last step of indirection between the bytes that
// were just verified and the process now running them.
func Spawn() error { return SpawnFrom("") }

// lookClaude resolves the Claude Code a probe would run. It is a var for the
// same reason every other uncontrollable dependency in this tree is one: whether
// a machine has claude on it is not something a test can arrange, and a suite
// that read the real PATH would probe on a developer's laptop and not on CI.
var lookClaude = exec.LookPath

// ProbeAvailable reports whether this machine can run a probe at all.
//
// Asked separately from SpawnProbe, and that separation is the point. A caller
// records the attempt against the account's six-hour budget BEFORE it spawns —
// otherwise a spawn that never starts leaves the account probe-due on the very
// next cadence, forever. A machine with no Claude Code on it is not a failed
// attempt, though: nothing was spent and nothing was tried, so it must not
// consume that budget or the account stays unknown for six hours after claude is
// finally installed. Asking first is what keeps those two apart.
func ProbeAvailable() error {
	if _, err := lookClaude("claude"); err != nil {
		return fmt.Errorf("a probe runs one turn of Claude Code, and `claude` is not on this PATH: %w", err)
	}
	return nil
}

// probeArgv is the command line one probe is run with. It is a function of its
// own so a test can assert the spelling without starting a process.
//
// --force is not a caller being pushy. An unattended caller has already applied
// both gates that flag bypasses — it checked the window it cares about for a
// reset and it has just stamped the attempt itself — so a child that re-applied
// them would find the stamp written a moment earlier and refuse every probe the
// daemon ever asks for.
func probeArgv(uuid, model string) []string {
	args := []string{ProbeArg, "--" + ProbeUUIDFlag, uuid, "--" + ProbeForceFlag}
	if model != "" {
		args = append(args, "--"+ProbeModelFlag, model)
	}
	return args
}

// SpawnProbe starts one `ccdad probe` and returns without waiting for it.
//
// A separate PROCESS rather than a function call, and the reason is the import
// direction stated above RunArg. A probe's mechanics are `ccdad run`'s — an
// ephemeral credential home, the account's stored login seeded into it, the
// adopt-back that carries a rotated refresh token home before the directory is
// deleted — and all of that lives in internal/cli, which imports this package
// and never the reverse. Re-execing the same binary is what lets the daemon have
// that code rather than a second copy of it, and it buys two more things: a
// claude that hangs cannot stall the tick loop, and a probe that crashes takes
// nothing with it.
//
// The child is NOT detached. A probe is the daemon's own errand and should die
// with it, unlike the daemon itself, which has to outlive the terminal it was
// born in. Waited for on a goroutine of its own for the same reason: not waiting
// leaves a zombie per probe until the daemon exits, and waiting here would put a
// whole Claude Code turn on a roughly 1 Hz tick loop. `ccdad probe` gives its
// claude a deadline of its own, so this goroutine cannot outlive it.
func SpawnProbe(uuid, model string) error {
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

	cmd := exec.Command(exe, probeArgv(uuid, model)...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	// The same reason Spawn moves: the daemon has already left whatever
	// directory it was started from, and a relative working directory would
	// resolve against a different one in the child.
	cmd.Dir = filepath.VolumeName(exe) + string(os.PathSeparator)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the probe: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

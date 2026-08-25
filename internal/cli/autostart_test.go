package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/daemon"
)

// enableAutoStart puts the real policy back for one test.
//
// isolate(t) replaces it for the whole suite, and it also SCOPES the credential
// environment — which the policy itself refuses to auto-start into. Both have
// to be undone here, and undoing the second one is the reason this helper
// exists rather than a one-line assignment.
func enableAutoStart(t *testing.T) {
	t.Helper()
	saved := autoStart
	t.Cleanup(func() { autoStart = saved })
	autoStart = maybeAutoStart
	// t.Setenv's own cleanup restores whatever the process had before isolate
	// set it, so unsetting here is safe and does not leak.
	os.Unsetenv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
}

// stubDaemonRun keeps `ccdad __daemon` from actually running a daemon inside
// the test binary. The command under test is the entrypoint, not the loop.
func stubDaemonRun(t *testing.T) {
	t.Helper()
	saved := runDaemon
	t.Cleanup(func() { runDaemon = saved })
	runDaemon = func(context.Context, daemon.Options) error { return nil }
}

// The suite's own guard, asserted rather than assumed. Without it `go test
// ./...` leaves detached daemons on the developer's machine, each holding a
// lock in a t.TempDir() the framework has already deleted — and nothing about
// that failure appears in the test output.
func TestIsolateSuppressesAutoStart(t *testing.T) {
	isolate(t)
	spawns := 0
	saved := spawnDaemon
	t.Cleanup(func() { spawnDaemon = saved })
	spawnDaemon = func(string) error { spawns++; return nil }
	// isolate also scopes the credential environment, which the policy refuses
	// on its own. Unscope it, so this proves the HOOK is suppressed rather than
	// that the policy happened to decline.
	os.Unsetenv("CLAUDE_SECURESTORAGE_CONFIG_DIR")

	runRoot(t, "list")
	if spawns != 0 {
		t.Fatalf("the suite auto-started %d daemons; isolate(t) is not suppressing the hook", spawns)
	}
}

// An allow-listed ccdad command auto-starts a daemon when none is up. That is
// the feature: automatic switching with nothing for the user to run is the
// whole point.
func TestAutoStartStartsADaemonForAnAllowListedCommand(t *testing.T) {
	for _, verb := range []string{"list", "status", "which"} {
		t.Run(verb, func(t *testing.T) {
			isolate(t)
			f := stubDaemonWorld(t, &fakeDaemon{})
			enableAutoStart(t)

			runRoot(t, verb)
			if f.spawns != 1 {
				t.Fatalf("`ccdad %s` spawned %d daemons, want exactly 1", verb, f.spawns)
			}
		})
	}
}

// An ALLOW-list, never a default-on hook. Every command here breaks in its own
// way if a daemon appears underneath it, and the list is what makes a command
// added later default to safe rather than to spawning.
func TestAutoStartDoesNotFireForCommandsThatMustNotHaveOne(t *testing.T) {
	cases := []struct {
		args []string
		why  string
	}{
		{[]string{"daemon", "status"}, "a probe that starts what it probes can never answer 5, and the whole exit contract goes with it"},
		{[]string{"daemon", "stop"}, "auto-start on stop resurrects exactly what was just stopped"},
		{[]string{"daemon", "logs"}, "reading a log is not a reason to start a process"},
		{[]string{"doctor"}, "a diagnostic must not create what it is checking for"},
		{[]string{"completion", "bash"}, "a daemon per TAB press"},
		{[]string{"remove", "nobody"}, "deleting an account is not a reason to start an engine"},
		{[]string{"primary", "nobody", "on"}, "setting an account's money policy is administering ccdad " +
			"rather than using it, and the engine reads the flag on its next tick either way"},
		{[]string{"setup-path"}, "the command that runs before `ccdad` even resolves must not spawn an " +
			"engine for a machine that has no accounts yet"},
		{[]string{"hover", "off"}, "the allow-list is keyed by command path, so listing hover would " +
			"auto-start a daemon for the verb that turns the mode off"},
		{[]string{daemon.RunArg}, "the child is itself ccdad, so this one is a fork bomb rather than a bug"},
		{[]string{"run", "nobody"}, "run exports a scoped credential home into its child, and a daemon " +
			"auto-started before that would manage the live one while the user is deliberately elsewhere"},
		{[]string{"probe", "nobody"}, "a probe is the engine's errand and the daemon re-execs it, so " +
			"auto-starting here leaves the recursion guard as the only fuse"},
		{[]string{"bootstrap"}, "a container entrypoint runs bootstrap before it starts the daemon " +
			"itself, so an entry here would spawn one over a store that has not finished importing " +
			"and then race the entrypoint's own daemon start for the singleton"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			isolate(t)
			f := stubDaemonWorld(t, &fakeDaemon{})
			stubDaemonRun(t)
			enableAutoStart(t)

			runRoot(t, tc.args...)
			if f.spawns != 0 {
				t.Fatalf("`ccdad %s` auto-started %d daemons: %s", strings.Join(tc.args, " "), f.spawns, tc.why)
			}
		})
	}
}

// `ccdad mcp` is deliberately not on the allow-list, and this pins the MAP
// rather than a run.
//
// The table above cannot hold it. The server blocks on stdin, so the only
// shapes of it that return are a help flag and a bad argument — and cobra
// answers both several steps before it reaches any persistent pre-run hook, so
// a row driving either would spawn nothing whether or not the entry existed.
// This assertion fails the moment somebody adds it, which is the whole
// property the allow-list is for.
func TestTheMCPServerIsNotOnTheAutoStartAllowList(t *testing.T) {
	if autoStartCommands["ccdad mcp"] {
		t.Error("`ccdad mcp` is on the auto-start allow-list. The client starts and restarts the " +
			"server on its own schedule, so an entry here spawns a daemon every time that happens, " +
			"before any tool is called — and the tools that DO auto-start reach the hook through " +
			"the command tree anyway, one fresh root per call")
	}
}

// The recursion test the task asks for, run through the child's OWN entrypoint:
// whatever else is true, `ccdad __daemon` must start nothing.
func TestTheDaemonChildStartsNoDaemonOfItsOwn(t *testing.T) {
	isolate(t)
	f := stubDaemonWorld(t, &fakeDaemon{})
	stubDaemonRun(t)
	enableAutoStart(t)

	code, stdout, _, _ := runRoot(t, daemon.RunArg)
	if code != ExitOK {
		t.Fatalf("`ccdad %s` = %d, want %d", daemon.RunArg, code, ExitOK)
	}
	if f.spawns != 0 {
		t.Fatalf("the daemon's own entrypoint spawned %d daemons", f.spawns)
	}
	if stdout != "" {
		t.Errorf("the daemon entrypoint wrote to stdout: %q", stdout)
	}
}

// The second half of the guard, and the half that holds when a daemon's
// descendants run something that IS on the allow-list.
func TestAutoStartIsSuppressedInAProcessCcdadStarted(t *testing.T) {
	isolate(t)
	f := stubDaemonWorld(t, &fakeDaemon{})
	enableAutoStart(t)
	t.Setenv(daemon.ChildEnvVar, "1")

	runRoot(t, "list")
	if f.spawns != 0 {
		t.Fatalf("a process ccdad started auto-started %d more", f.spawns)
	}
}

// ccpath resolves the credential home at call time, and ccdad itself scopes it
// per account. A daemon auto-started from inside a scoped shell would write
// THAT shell's credential home for the rest of its life, silently — so the
// answer is to refuse, not to pin the scoped path in place permanently.
func TestAutoStartRefusesInAScopedCredentialEnvironment(t *testing.T) {
	isolate(t)
	f := stubDaemonWorld(t, &fakeDaemon{})
	enableAutoStart(t)
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", t.TempDir())

	runRoot(t, "list")
	if f.spawns != 0 {
		t.Fatalf("auto-started %d daemons into a credential environment scoped to one terminal", f.spawns)
	}
}

// Defined-but-empty is a scoping act too: Claude Code reads it as ~/.claude
// rather than as the config home, so a daemon born there manages a different
// file from the one its caller was looking at.
func TestAutoStartRefusesWhenTheCredentialHomeIsDefinedButEmpty(t *testing.T) {
	isolate(t)
	f := stubDaemonWorld(t, &fakeDaemon{})
	enableAutoStart(t)
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")

	runRoot(t, "list")
	if f.spawns != 0 {
		t.Fatalf("auto-started %d daemons with the credential home defined-but-empty", f.spawns)
	}
}

func TestAutoStartDoesNothingWhenADaemonIsAlreadyRunning(t *testing.T) {
	isolate(t)
	f := stubDaemonWorld(t, &fakeDaemon{held: true, pid: 4321, pidOK: true})
	enableAutoStart(t)

	runRoot(t, "list")
	if f.spawns != 0 {
		t.Fatalf("auto-started %d daemons alongside a running one", f.spawns)
	}
}

// The same rule the whole daemon group is built on, on the hottest path there
// is: a lock that cannot be probed is not an invitation to start a daemon per
// invocation forever.
func TestAutoStartDoesNothingWhenTheLockCannotBeProbed(t *testing.T) {
	isolate(t)
	f := stubDaemonWorld(t, &fakeDaemon{probeErr: daemon.ErrLocksUnsupported})
	enableAutoStart(t)

	runRoot(t, "list")
	if f.spawns != 0 {
		t.Fatalf("auto-started %d daemons on a filesystem that cannot answer whether one is running", f.spawns)
	}
}

// Auto-start is invisible. One stray line on stdout breaks `ccdad list --json |
// jq`, and a daemon that will not start is a degraded mode rather than an error
// for a command that was not asking for one.
func TestAutoStartIsSilentAndDoesNotFailTheCommand(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	wantCode, wantOut, wantErr, wantTop := runRoot(t, "list", "--json")

	f := stubDaemonWorld(t, &fakeDaemon{spawnErr: os.ErrPermission})
	enableAutoStart(t)
	code, stdout, stderr, top := runRoot(t, "list", "--json")

	if f.spawns != 1 {
		t.Fatalf("spawns = %d, want the attempt to have been made", f.spawns)
	}
	if code != wantCode {
		t.Errorf("a failed auto-start changed the exit code: %d, want %d", code, wantCode)
	}
	if stdout != wantOut {
		t.Errorf("a failed auto-start changed stdout:\n got %q\nwant %q", stdout, wantOut)
	}
	if stderr+top != wantErr+wantTop {
		t.Errorf("a failed auto-start wrote to stderr: %q", stderr+top)
	}
}

// Fire and forget. `ccdad daemon start` waits for the singleton because a
// caller asked for a daemon and wants to know it is there; every other command
// is doing something else, and paying for a process to reach its first lock
// would put that latency on the hot path of the whole tree.
func TestAutoStartDoesNotWaitForTheDaemonToComeUp(t *testing.T) {
	isolate(t)
	f := stubDaemonWorld(t, &fakeDaemon{takeAfter: 1 << 30})
	enableAutoStart(t)

	runRoot(t, "list")
	if f.spawns != 1 {
		t.Fatalf("spawns = %d, want exactly 1", f.spawns)
	}
	if f.probes != 1 {
		t.Fatalf("auto-start probed the lock %d times; it waited for the daemon instead of leaving it to come up", f.probes)
	}
}

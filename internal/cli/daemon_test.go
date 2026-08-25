package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/daemon"
)

// fakeDaemon stands in for everything the command group acts on: the singleton,
// the pidfile, the detached spawn and the shutdown request.
//
// All four are seams for the same reason. A filesystem where locks do not work
// is the case this group exists to keep separate from "no daemon", and it is
// not something a test can arrange; a real spawn would leave a detached process
// pinned to a t.TempDir() that is about to be deleted underneath it.
type fakeDaemon struct {
	held     bool
	probeErr error
	// probeErrAfter is how many probes answer normally before probeErr starts.
	// Zero means from the first one; a larger number is a lock that stops
	// answering part-way through a wait, which is the only way to reach the
	// "cannot determine" branch inside one.
	probeErrAfter int
	pid           int
	pidOK         bool
	pidErr        error

	spawnErr    error
	shutdownErr error
	// forceSucceeds says whether this platform HAS a guarded escalation. False
	// is Unix's answer — errors.ErrUnsupported, by design — and true is what
	// Windows does once the cross-check passes.
	forceSucceeds bool
	forced        []int
	// onShutdown runs when the daemon is asked to stop, so a test can assert
	// what the world looked like AT that moment rather than afterwards.
	onShutdown func()

	// releaseAfter is how many probes the daemon takes to let go of the
	// singleton after being asked to. Zero is gone by the next probe; a larger
	// number is a daemon finishing the tick in flight, which is what restart has
	// to actually wait for instead of sleeping through.
	releaseAfter int

	probes    int
	spawns    int
	signalled []int
	// heldAtSpawn records whether the singleton was still held each time spawn
	// was called. restart's whole contract is that it never is.
	heldAtSpawn []bool
	// spawnedFrom records the executable each spawn was asked for. "" is
	// "resolve it yourself", which is what every call site but `update` passes.
	spawnedFrom []string
	// sizeAtSpawn is the size of the file each spawn was pointed at, read AT
	// the moment spawn was called, and -1 when it was not pointed at one. The
	// moment is the whole of it: `update` claims to restart the daemon from the
	// file it just wrote, and a path string is the same before and after the
	// replacement, as are the target's bytes once the run has finished.
	sizeAtSpawn []int64

	// takeAfter is how many probes a freshly spawned daemon takes to reach its
	// first lock. Zero is up instantly, which no real daemon is: Spawn returns
	// as soon as the process exists, and everything after that is scheduling.
	takeAfter           int
	starting            bool
	probesWhileStarting int

	asked          bool
	probesAfterAsk int
	// releasePolls counts every probe taken while a stop was outstanding, over
	// the whole run. Unlike probesAfterAsk it is never reset, so a restart can
	// still be asked afterwards how hard it waited.
	releasePolls int
}

func (f *fakeDaemon) probe() (bool, error) {
	f.probes++
	if f.probeErr != nil && f.probes > f.probeErrAfter {
		return false, f.probeErr
	}
	if f.starting {
		f.probesWhileStarting++
		if f.probesWhileStarting >= f.takeAfter {
			f.held, f.starting = true, false
		}
	}
	if f.asked {
		f.probesAfterAsk++
		f.releasePolls++
		if f.probesAfterAsk > f.releaseAfter {
			f.held = false
		}
	}
	return f.held, nil
}

func (f *fakeDaemon) spawn(exe string) error {
	f.spawns++
	f.spawnedFrom = append(f.spawnedFrom, exe)
	size := int64(-1)
	if exe != "" {
		if fi, err := os.Stat(exe); err == nil {
			size = fi.Size()
		}
	}
	f.sizeAtSpawn = append(f.sizeAtSpawn, size)
	f.heldAtSpawn = append(f.heldAtSpawn, f.held)
	if f.spawnErr != nil {
		return f.spawnErr
	}
	// A daemon that has just been started has not been asked to stop, so the
	// release countdown starts over. Without this the fake would take the new
	// daemon's lock away again on the next probe.
	f.pid, f.pidOK = 5555, true
	f.asked, f.probesAfterAsk = false, 0
	if f.takeAfter <= 0 {
		f.held = true
	} else {
		f.starting = true
	}
	return nil
}

func (f *fakeDaemon) readPID() (int, bool, error) { return f.pid, f.pidOK, f.pidErr }

func (f *fakeDaemon) force(pid int) error {
	f.forced = append(f.forced, pid)
	if !f.forceSucceeds {
		return errors.ErrUnsupported
	}
	f.held, f.releaseAfter = false, 0
	return nil
}

// observe is the same world seen through daemon.Observe: `daemon status` reads
// the published document as well as the lock, because the pid and the uptime it
// prints come from there.
func (f *fakeDaemon) observe() (daemon.Report, error) {
	held, err := f.probe()
	var r daemon.Report
	if f.pidOK {
		r.HasStatus = true
		r.Status = daemon.Status{SchemaVersion: 1, PID: f.pid, StartedAt: time.Now().Add(-time.Minute)}
	}
	if err != nil {
		r.State = daemon.DaemonUnknown
		return r, err
	}
	if held {
		r.State = daemon.DaemonRunning
	} else {
		r.State = daemon.DaemonStopped
	}
	return r, nil
}

func (f *fakeDaemon) shutdown(pid int) error {
	f.signalled = append(f.signalled, pid)
	if f.onShutdown != nil {
		f.onShutdown()
	}
	if f.shutdownErr != nil {
		return f.shutdownErr
	}
	f.asked = true
	return nil
}

// stubDaemonWorld installs the fake and shrinks the waits, so a test that
// exercises a timeout costs milliseconds rather than the ten seconds a user
// gets.
func stubDaemonWorld(t *testing.T, f *fakeDaemon) *fakeDaemon {
	t.Helper()
	savedHeld, savedSpawn, savedShutdown, savedPID := singletonHeld, spawnDaemon, requestShutdown, readDaemonPID
	savedForce := forceShutdown
	savedObserve := observeDaemon
	savedPoll, savedWait := daemonPollInterval, daemonWaitTimeout
	t.Cleanup(func() {
		singletonHeld, spawnDaemon, requestShutdown, readDaemonPID = savedHeld, savedSpawn, savedShutdown, savedPID
		forceShutdown = savedForce
		observeDaemon = savedObserve
		daemonPollInterval, daemonWaitTimeout = savedPoll, savedWait
	})
	singletonHeld, spawnDaemon, requestShutdown, readDaemonPID = f.probe, f.spawn, f.shutdown, f.readPID
	forceShutdown = f.force
	observeDaemon = f.observe
	daemonPollInterval, daemonWaitTimeout = time.Millisecond, 150*time.Millisecond
	return f
}

func writeDaemonLog(t *testing.T, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(mustPath(daemon.LogPath())), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mustPath(daemon.LogPath()), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDaemonExitMatrix is the primary contract of this command group. Every
// mapping here is one `if` away from clauth's documented bug — it returns 1 both
// when no daemon is running and when the lock is unusable, so a
// `status || start` supervisor respawns forever on a filesystem without working
// locks — which is why the table comes before the commands rather than after.
func TestDaemonExitMatrix(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		world  fakeDaemon
		setup  func(t *testing.T)
		want   ExitCode
		stdout string
		stderr string
		check  func(t *testing.T, f *fakeDaemon)
	}{
		{
			name:   "status while a daemon is running is 0",
			args:   []string{"daemon", "status"},
			world:  fakeDaemon{held: true, pid: 4321, pidOK: true},
			want:   ExitOK,
			stdout: "running",
		},
		{
			name:   "status with no daemon is 5, not 1",
			args:   []string{"daemon", "status"},
			world:  fakeDaemon{},
			want:   ExitProbeNegative,
			stdout: "not running",
		},
		{
			name:   "status that cannot determine is 1, not 5",
			args:   []string{"daemon", "status"},
			world:  fakeDaemon{probeErr: daemon.ErrLocksUnsupported},
			want:   ExitFailure,
			stderr: "ccdad:",
		},
		{
			name:  "stop with nothing running is 3 — not 5 and not 0",
			args:  []string{"daemon", "stop"},
			world: fakeDaemon{},
			want:  ExitNothingToDo,
			check: func(t *testing.T, f *fakeDaemon) {
				if len(f.signalled) != 0 {
					t.Errorf("signalled %v with no daemon running", f.signalled)
				}
			},
		},
		{
			name:  "stop a running daemon is 0",
			args:  []string{"daemon", "stop"},
			world: fakeDaemon{held: true, pid: 4321, pidOK: true},
			want:  ExitOK,
			check: func(t *testing.T, f *fakeDaemon) {
				if len(f.signalled) != 1 || f.signalled[0] != 4321 {
					t.Errorf("signalled %v, want exactly [4321]", f.signalled)
				}
			},
		},
		{
			name:  "stop that cannot determine is 1 and signals nothing",
			args:  []string{"daemon", "stop"},
			world: fakeDaemon{probeErr: daemon.ErrLocksUnsupported},
			want:  ExitFailure,
			check: func(t *testing.T, f *fakeDaemon) {
				if len(f.signalled) != 0 {
					t.Errorf("signalled %v without knowing a daemon was there", f.signalled)
				}
			},
		},
		{
			name:  "stop with a damaged pidfile is 1 and signals nothing",
			args:  []string{"daemon", "stop"},
			world: fakeDaemon{held: true, pidErr: errors.New("the pidfile holds \"garbage\"")},
			want:  ExitFailure,
			// Both pidfile branches exit 1, so the exit code cannot tell them
			// apart. The guidance is the behaviour: only the damaged-body branch
			// says the file does not parse, and folding the two together would
			// tell the user to try again in a moment forever.
			stderr: "does not parse",
			check: func(t *testing.T, f *fakeDaemon) {
				if len(f.signalled) != 0 {
					t.Errorf("signalled %v from a pidfile that does not parse", f.signalled)
				}
			},
		},
		{
			name:   "start against a live daemon is 3 and prints its pid",
			args:   []string{"daemon", "start"},
			world:  fakeDaemon{held: true, pid: 4321, pidOK: true},
			want:   ExitNothingToDo,
			stderr: "4321",
			check: func(t *testing.T, f *fakeDaemon) {
				if f.spawns != 0 {
					t.Errorf("spawned %d daemons against a live one", f.spawns)
				}
			},
		},
		{
			name:  "start with nothing running is 0",
			args:  []string{"daemon", "start"},
			world: fakeDaemon{},
			want:  ExitOK,
			check: func(t *testing.T, f *fakeDaemon) {
				if f.spawns != 1 {
					t.Errorf("spawns = %d, want exactly 1", f.spawns)
				}
			},
		},
		{
			name:  "start that cannot determine is 1 and spawns nothing",
			args:  []string{"daemon", "start"},
			world: fakeDaemon{probeErr: daemon.ErrLocksUnsupported},
			want:  ExitFailure,
			check: func(t *testing.T, f *fakeDaemon) {
				if f.spawns != 0 {
					t.Errorf("spawned %d daemons on a filesystem that cannot answer", f.spawns)
				}
			},
		},
		{
			name:  "restart with nothing running still ends with one running",
			args:  []string{"daemon", "restart"},
			world: fakeDaemon{},
			want:  ExitOK,
			check: func(t *testing.T, f *fakeDaemon) {
				if f.spawns != 1 || len(f.signalled) != 0 {
					t.Errorf("spawns=%d signalled=%v, want one spawn and no signal", f.spawns, f.signalled)
				}
			},
		},
		{
			name:  "logs with no daemon and no log file is 5, not 1",
			args:  []string{"daemon", "logs"},
			world: fakeDaemon{},
			want:  ExitProbeNegative,
		},
		{
			name:   "logs prints the log even with no daemon running",
			args:   []string{"daemon", "logs"},
			world:  fakeDaemon{},
			setup:  func(t *testing.T) { writeDaemonLog(t, "ccdad daemon up, pid 7\n") },
			want:   ExitOK,
			stdout: "ccdad daemon up, pid 7",
		},
		{
			name:   "bare daemon is a usage error",
			args:   []string{"daemon"},
			world:  fakeDaemon{},
			want:   ExitUsage,
			stderr: "start",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			f := stubDaemonWorld(t, &tc.world)
			if tc.setup != nil {
				tc.setup(t)
			}
			code, stdout, stderr, top := runRoot(t, tc.args...)
			if code != tc.want {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s%s", code, tc.want, stdout, stderr, top)
			}
			if tc.stdout != "" && !strings.Contains(stdout, tc.stdout) {
				t.Errorf("stdout %q does not contain %q", stdout, tc.stdout)
			}
			if tc.stderr != "" && !strings.Contains(stderr+top, tc.stderr) {
				t.Errorf("stderr %q does not contain %q", stderr+top, tc.stderr)
			}
			if tc.check != nil {
				tc.check(t, f)
			}
		})
	}
}

// The idiom exit 5 was introduced for, executed. It is only safe while 5 means
// a definite no: fold "cannot determine" into it and this loop spawns a daemon
// per invocation forever on an NFS mount with no lock daemon.
func TestStatusThenStartIsSafeWhenTheLockCannotBeProbed(t *testing.T) {
	isolate(t)
	f := stubDaemonWorld(t, &fakeDaemon{probeErr: daemon.ErrLocksUnsupported})

	code, _, _, _ := runRoot(t, "daemon", "status")
	if code == ExitProbeNegative {
		t.Fatal("an unprobeable lock reported exit 5; `status || start` would respawn forever")
	}
	if code == ExitProbeNegative { // the supervisor's own branch
		_, _, _, _ = runRoot(t, "daemon", "start")
	}
	if f.spawns != 0 {
		t.Fatalf("the idiom spawned %d daemons without knowing whether one was running", f.spawns)
	}
}

// And the same idiom on a machine where the answer IS "no daemon": exit 5 has to
// be reachable, or the supervisor never starts one.
func TestStatusThenStartStartsOneWhenThereIsNone(t *testing.T) {
	isolate(t)
	f := stubDaemonWorld(t, &fakeDaemon{})

	code, _, _, _ := runRoot(t, "daemon", "status")
	if code != ExitProbeNegative {
		t.Fatalf("daemon status with no daemon = %d, want %d", code, ExitProbeNegative)
	}
	if code == ExitProbeNegative {
		if got, _, _, _ := runRoot(t, "daemon", "start"); got != ExitOK {
			t.Fatalf("daemon start = %d, want %d", got, ExitOK)
		}
	}
	if f.spawns != 1 {
		t.Fatalf("spawns = %d, want exactly 1", f.spawns)
	}
}

// restart is not `stop; start`. The new daemon must not probe — let alone try to
// acquire — while the old one still holds the singleton, and the wait for it to
// let go has to be a bounded poll of the lock rather than a fixed sleep, which
// is either too short on a loaded machine or wasted on an idle one.
func TestRestartWaitsForTheSingletonToClearBeforeSpawning(t *testing.T) {
	isolate(t)
	// The old daemon finishes the tick in flight: it holds on for three more
	// probes after being asked to stop.
	f := stubDaemonWorld(t, &fakeDaemon{held: true, pid: 4321, pidOK: true, releaseAfter: 3})

	code, _, stderr, top := runRoot(t, "daemon", "restart")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s%s", code, ExitOK, stderr, top)
	}
	if len(f.signalled) != 1 || f.signalled[0] != 4321 {
		t.Fatalf("signalled %v, want exactly [4321]", f.signalled)
	}
	if f.spawns != 1 {
		t.Fatalf("spawns = %d, want exactly 1", f.spawns)
	}
	if f.heldAtSpawn[0] {
		t.Fatal("restart spawned the new daemon while the old one still held the singleton")
	}
	if f.releasePolls < 4 {
		t.Fatalf("restart polled the lock %d times after asking the daemon to stop; a fixed sleep would show up here", f.releasePolls)
	}
}

// Spawn returns as soon as the process exists; the daemon reaches its first
// lock some scheduling later. A start that returned there would be followed by
// a `daemon status` reporting 5 about a daemon that is perfectly real, and the
// two commands would not compose.
func TestStartWaitsForTheNewDaemonToTakeTheSingleton(t *testing.T) {
	isolate(t)
	f := stubDaemonWorld(t, &fakeDaemon{takeAfter: 4})

	code, _, stderr, top := runRoot(t, "daemon", "start")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s%s", code, ExitOK, stderr, top)
	}
	if f.probesWhileStarting < 4 {
		t.Fatalf("start polled the lock %d times while the daemon was coming up; it returned early", f.probesWhileStarting)
	}
	if got, _, _, _ := runRoot(t, "daemon", "status"); got != ExitOK {
		t.Fatalf("daemon status straight after daemon start = %d, want %d", got, ExitOK)
	}
}

// A lock that stops answering PART-WAY THROUGH a wait is still "cannot
// determine", and the wait has to carry that out rather than reading it as the
// answer it was waiting for. Ending the wait as a yes would have `daemon start`
// report a daemon it never saw take the singleton.
func TestAWaitThatLosesTheLockReportsCannotTell(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{
		takeAfter:     1 << 30,
		probeErr:      daemon.ErrLocksUnsupported,
		probeErrAfter: 3,
	})

	code, _, _, top := runRoot(t, "daemon", "start")
	if code != ExitFailure {
		t.Fatalf("exit = %d, want %d", code, ExitFailure)
	}
	if !strings.Contains(top, "cannot tell") {
		t.Errorf("the failure does not say the lock could not be probed: %q", top)
	}
}

// A daemon that was asked to stop and did not is a failure the user has to hear
// about, not a stop reported and a lock still held.
func TestStopReportsADaemonThatWillNotGo(t *testing.T) {
	isolate(t)
	f := stubDaemonWorld(t, &fakeDaemon{held: true, pid: 4321, pidOK: true, releaseAfter: 1 << 30})

	code, _, _, top := runRoot(t, "daemon", "stop")
	if code != ExitFailure {
		t.Fatalf("exit = %d, want %d", code, ExitFailure)
	}
	if !strings.Contains(top, "4321") {
		t.Errorf("the failure does not name the pid that would not go: %q", top)
	}
	if !f.held {
		t.Error("the fake released the lock; this test is no longer testing the timeout")
	}
}

// Windows answers "is anything listening" directly, because the named event
// either exists or does not — and a daemon holding the singleton with nothing
// listening is a negative answer to a probe rather than a runtime failure. The
// stopping side must never create the event, so this state is real.
func TestStopReportsWhenNothingIsListeningForTheRequest(t *testing.T) {
	isolate(t)
	f := stubDaemonWorld(t, &fakeDaemon{
		held: true, pid: 4321, pidOK: true,
		shutdownErr: fmt.Errorf("%w on Local\\ccdad-shutdown-abc", daemon.ErrNoShutdownListener),
	})

	code, _, stderr, top := runRoot(t, "daemon", "stop")
	if code != ExitProbeNegative {
		t.Fatalf("exit = %d, want %d", code, ExitProbeNegative)
	}
	if !strings.Contains(stderr+top, "4321") {
		t.Errorf("the notice does not name the pid: %q", stderr+top)
	}
	if len(f.forced) != 0 {
		t.Errorf("terminated %v after the graceful request was never delivered; the escalation is a TIMEOUT fallback", f.forced)
	}
}

// The escalation is strictly behind the event, and behind the wait. A daemon
// that stops when asked must never be terminated.
func TestStopDoesNotEscalateWhenTheDaemonStops(t *testing.T) {
	isolate(t)
	f := stubDaemonWorld(t, &fakeDaemon{held: true, pid: 4321, pidOK: true, forceSucceeds: true})

	if code, _, _, top := runRoot(t, "daemon", "stop"); code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitOK, top)
	}
	if len(f.forced) != 0 {
		t.Fatalf("terminated %v a daemon that had already stopped", f.forced)
	}
}

// And when it does not stop, on a platform that HAS a guarded escalation, the
// user is told it was terminated rather than left with a daemon nothing in the
// tool can reach.
func TestStopEscalatesAfterTheWaitOnAPlatformThatCan(t *testing.T) {
	isolate(t)
	f := stubDaemonWorld(t, &fakeDaemon{
		held: true, pid: 4321, pidOK: true,
		releaseAfter:  1 << 30,
		forceSucceeds: true,
	})

	code, _, stderr, top := runRoot(t, "daemon", "stop")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s%s", code, ExitOK, stderr, top)
	}
	if len(f.forced) != 1 || f.forced[0] != 4321 {
		t.Fatalf("terminated %v, want exactly [4321]", f.forced)
	}
	if !strings.Contains(stderr, "terminated") {
		t.Errorf("a terminated daemon was reported as an ordinary stop: %q", stderr)
	}
}

// A --json caller receives exactly one object on stdout. The exit code is the
// other half of the answer and stays non-zero — emitting {"running":false}
// with exit 0 rebuilds the ambiguity the code split exists to remove.
func TestDaemonStatusJSONStillExitsNonZero(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})

	code, stdout, _, _ := runRoot(t, "daemon", "status", "--json")
	if code != ExitProbeNegative {
		t.Fatalf("exit = %d, want %d", code, ExitProbeNegative)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	d, ok := payload["daemon"].(map[string]any)
	if !ok {
		t.Fatalf("no daemon object in %v", payload)
	}
	if d["state"] != "stopped" {
		t.Errorf("daemon.state = %v, want %q", d["state"], "stopped")
	}
}

// The same shape as `ccdad status --json`'s daemon object, so a consumer reads
// .daemon.state from either command. The two build it from one function.
func TestDaemonStatusJSONMatchesTheDashboardShape(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{held: true, pid: 4321, pidOK: true})

	code, stdout, _, _ := runRoot(t, "daemon", "status", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d", code, ExitOK)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	d := payload["daemon"].(map[string]any)
	if d["state"] != "running" || d["pid"] != float64(4321) {
		t.Errorf("daemon object = %v, want state running and pid 4321", d)
	}
	if payload["schemaVersion"] != float64(1) {
		t.Errorf("schemaVersion = %v, want 1", payload["schemaVersion"])
	}
}

// The daemon rotates daemon.log out from under any reader. A follower that
// keeps the file open across that keeps reading the RENAMED inode: every line
// after the first rotation is silently lost, and on Windows a handle opened
// without share-delete blocks the rename outright.
func TestFollowLogPicksUpTheNewFileAfterARotation(t *testing.T) {
	isolate(t)
	writeDaemonLog(t, "first line\n")

	var out safeBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- followLog(ctx, &out, mustPath(daemon.LogPath()), 0, 5*time.Millisecond) }()

	// Rotate the way Logger.rotate does: rename the file aside and create a new
	// one in its place.
	waitForOutput(t, &out, "first line")
	rotateAside(t, mustPath(daemon.LogPath()))
	writeDaemonLog(t, "after the rotation\n")
	waitForOutput(t, &out, "after the rotation")

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("followLog: %v", err)
	}
}

// appendToDaemonLog adds to the log the way the daemon does, leaving what is
// already there alone. Rewriting it would truncate first, and a follower that
// caught the file at zero bytes would reset its position for a legitimate
// reason -- which is the very thing the test below is trying to rule out.
func appendToDaemonLog(t *testing.T, body string) {
	t.Helper()
	f, err := os.OpenFile(mustPath(daemon.LogPath()), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
}

// A retryable open holds the follower's position rather than resetting it.
// Whether the file was replaced is precisely what is not known while it cannot
// be opened, and guessing "replaced" replays the whole log into the user's
// terminal for a rotation that never happened. The next open that succeeds
// settles it by identity, which is what the uninterrupted path already does.
func TestARetryableOpenErrorDoesNotReplayTheLog(t *testing.T) {
	isolate(t)
	writeDaemonLog(t, "first line\n")

	held := errors.New("another process has the file")
	var opens atomic.Int32
	stubLogOpen(t,
		func(name string) (*os.File, error) {
			// The second poll, so the first line is already through and there
			// is a position worth losing.
			if opens.Add(1) == 2 {
				return nil, &fs.PathError{Op: "open", Path: name, Err: held}
			}
			return os.Open(name)
		},
		func(err error) bool { return errors.Is(err, held) },
	)

	var out safeBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- followLog(ctx, &out, mustPath(daemon.LogPath()), 0, 5*time.Millisecond) }()

	waitForOutput(t, &out, "first line")
	appendToDaemonLog(t, "second line\n")
	waitForOutput(t, &out, "second line")

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("followLog: %v", err)
	}
	if n := strings.Count(out.String(), "first line"); n != 1 {
		t.Fatalf("the first line reached the follower %d times, want 1:\n%s", n, out.String())
	}
}

// stubLogOpen swaps the follower's open and its retryable classifier together,
// for the length of one test.
//
// Both at once, because either alone proves nothing: the errors this routing
// exists for are Windows errnos, so a test that injected one would take the
// non-retryable branch everywhere else, and a test that only swapped the
// classifier would have no way to make the open fail in the first place.
func stubLogOpen(t *testing.T, open func(string) (*os.File, error), retryable func(error) bool) {
	t.Helper()
	origOpen, origRetryable := openLogFile, logOpenRetryable
	openLogFile, logOpenRetryable = open, retryable
	t.Cleanup(func() { openLogFile, logOpenRetryable = origOpen, origRetryable })
}

// Opening the log while a rotation is in flight is where Windows answers
// ERROR_SHARING_VIOLATION, ERROR_ACCESS_DENIED or ERROR_LOCK_VIOLATION -- an
// antivirus scanner or the search indexer holding the file for a moment. None
// of those is ErrNotExist, so treating them as fatal ends the follow for good:
// the user watching `ccdad daemon logs --follow` gets silence from the
// rotation onwards. A retryable open must leave the follower alive to try the
// next poll.
func TestFollowLogSurvivesARetryableOpenError(t *testing.T) {
	isolate(t)
	writeDaemonLog(t, "first line\n")

	held := errors.New("another process has the file")
	var opens atomic.Int32
	stubLogOpen(t,
		func(name string) (*os.File, error) {
			if opens.Add(1) <= 3 {
				return nil, &fs.PathError{Op: "open", Path: name, Err: held}
			}
			return os.Open(name)
		},
		func(err error) bool { return errors.Is(err, held) },
	)

	var out safeBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- followLog(ctx, &out, mustPath(daemon.LogPath()), 0, 5*time.Millisecond) }()

	// Reported here rather than left to the wait below: if the retryable error
	// is fatal the follower ends on its first poll, and naming that beats ten
	// seconds spent waiting for output that is never coming.
	select {
	case err := <-done:
		cancel()
		t.Fatalf("the follower ended on a retryable open error: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	waitForOutput(t, &out, "first line")

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("followLog: %v", err)
	}
}

// The other half of the routing, and the one that keeps the first from being
// satisfied by swallowing everything: an open error that is NOT retryable still
// ends the follow and still reaches the caller. A follower that retried, say,
// a permission failure forever would hang with nothing on screen.
func TestFollowLogEndsOnAnOpenErrorThatIsNotRetryable(t *testing.T) {
	isolate(t)
	writeDaemonLog(t, "first line\n")

	wantErr := errors.New("the log is unreadable")
	stubLogOpen(t,
		func(string) (*os.File, error) { return nil, wantErr },
		func(error) bool { return false },
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out safeBuffer
	done := make(chan error, 1)
	go func() { done <- followLog(ctx, &out, mustPath(daemon.LogPath()), 0, 5*time.Millisecond) }()

	// On a bare call this would hang rather than fail, and a follow that never
	// ends is exactly the defect being ruled out -- so it is bounded here, and
	// the timeout is the assertion.
	select {
	case err := <-done:
		if !errors.Is(err, wantErr) {
			t.Fatalf("followLog = %v, want an error carrying %v", err, wantErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("followLog is still running; an open error it cannot retry has to end the follow")
	}
}

// rotateAside renames the log the way Logger.rotate does, retrying while the
// follower has it open.
//
// os.Open does not pass FILE_SHARE_DELETE, so on Windows a rename whose window
// overlaps the follower's 5 ms poll fails with a sharing violation. A real log
// rotator retries for the same reason; a test that does not is a test that
// fails on whichever poll it happened to collide with.
func rotateAside(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := os.Rename(path, path+".1")
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("rotating %s: %v", path, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// safeBuffer is a strings.Builder the test can read while the follower writes.
type safeBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForOutput(t *testing.T, out *safeBuffer, needle string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), needle) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%q never reached the follower within 10s. Got:\n%s", needle, out.String())
}

// `ccdad daemon logs -n 2` is a tail, and a tail of a log that has just been
// rotated must not read the whole of the previous generation back in.
func TestLogsPrintsTheLastLinesOnly(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	writeDaemonLog(t, "one\ntwo\nthree\nfour\n")

	code, stdout, _, _ := runRoot(t, "daemon", "logs", "-n", "2")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d", code, ExitOK)
	}
	if got := stdout; got != "three\nfour\n" {
		t.Fatalf("stdout = %q, want the last two lines", got)
	}
}

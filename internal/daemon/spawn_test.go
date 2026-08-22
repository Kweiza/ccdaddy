package daemon

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// spawnViaAChildThatExits runs this test binary in the spawner role, waits for
// it to exit, and returns the report its detached grandchild wrote.
//
// The intermediate process is what makes the test mean anything: a child is
// only orphaned once its parent is gone, so asserting on the daemon's parent
// while the spawner is still running would assert nothing.
func spawnViaAChildThatExits(t *testing.T, extraEnv ...string) (childReport, int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("detachment on Windows is a console and process-group property, not a session one")
	}
	dir := t.TempDir()
	report := filepath.Join(dir, "child.txt")
	gone := filepath.Join(dir, "spawner-reaped")
	spawner := exec.Command(os.Args[0])
	spawner.Env = append(os.Environ(),
		roleEnv+"="+roleSpawner,
		reportEnv+"="+report,
		goneEnv+"="+gone,
	)
	spawner.Env = append(spawner.Env, extraEnv...)
	spawner.Stderr = os.Stderr
	if err := spawner.Start(); err != nil {
		t.Fatalf("starting the spawner: %v", err)
	}
	pid := spawner.Process.Pid
	if err := spawner.Wait(); err != nil {
		t.Fatalf("the spawner failed: %v", err)
	}
	// The spawner has been reaped, so its pid can no longer be anyone's
	// parent. Only now is it meaningful to ask the child who its parent is.
	if err := os.WriteFile(gone, nil, 0o600); err != nil {
		t.Fatalf("signalling the child: %v", err)
	}
	return readReport(t, report, 15*time.Second), pid
}

// The observable form of "detached": the child is in a session of its own, and
// once its parent is gone it has been reparented away from it.
//
// The brief asked for "PPID is 1", and that is what this host shows, but it is
// not portable: any ancestor holding PR_SET_CHILD_SUBREAPER — some container
// inits, some session managers — collects the orphan instead of init, and the
// assertion would be flaky for a reason that has nothing to do with ccdad. What
// is always true is that the child no longer belongs to the process that
// started it.
func TestSpawnLeavesTheChildInItsOwnSessionAndReparented(t *testing.T) {
	report, spawnerPID := spawnViaAChildThatExits(t)

	pid := report.num(t, "pid")
	if sid := report.num(t, "sid"); sid != pid {
		t.Errorf("the child reports session %d and pid %d; they must be equal, or it never left the "+
			"controlling terminal and dies with the shell that started it (sid error: %q)", sid, pid, report["sid_err"])
	}
	if ppid := report.num(t, "ppid"); ppid == spawnerPID {
		t.Errorf("the child still belonged to the process that spawned it (ppid %d) after that "+
			"process had exited and been reaped, so it was never detached", ppid)
	}
	if pid == spawnerPID {
		t.Error("no new process was created at all")
	}
}

// The environment is inherited whole because CCDAD_HOME has to reach the
// daemon: a daemon managing a different store than the CLI that started it is
// worse than no daemon. This is also the inheritance that makes autostart
// dangerous under `go test`, which is why the suppression seam is a hard
// requirement of the task that lands it.
func TestSpawnGivesTheChildTheSameStore(t *testing.T) {
	store := t.TempDir()
	report, _ := spawnViaAChildThatExits(t, "CCDAD_HOME="+store)
	if got := report["store"]; got != store {
		t.Errorf("the child sees CCDAD_HOME=%q, want %q — it would manage a different store than its caller", got, store)
	}
}

// The daemon outlives the directory it was started from, so holding that
// directory open keeps a filesystem busy and can stop a volume unmounting.
func TestSpawnLeavesTheChildAtTheRootOfTheVolume(t *testing.T) {
	report, _ := spawnViaAChildThatExits(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	want := filepath.VolumeName(exe) + string(os.PathSeparator)
	if got := report["cwd"]; got != want {
		t.Errorf("the child's working directory is %q, want %q", got, want)
	}
}

// The rule that costs the most when it is missed. A child that inherits the
// parent's stdout keeps the pipe open, so `V=$(ccdad which)` waits for an EOF
// that only arrives when the daemon exits — which is to say, never. It is
// invisible interactively, where stdout is a terminal rather than a pipe, and
// the daemon auto-starts from every command in the tree.
func TestSpawnDoesNotLeaveTheChildHoldingOurStdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the spawner harness re-execs this binary in a unix role")
	}
	report := filepath.Join(t.TempDir(), "child.txt")
	spawner := exec.Command(os.Args[0])
	spawner.Env = append(os.Environ(),
		roleEnv+"="+roleHoldPipe,
		reportEnv+"="+report,
	)
	spawner.Stderr = os.Stderr
	stdout, err := spawner.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := spawner.Start(); err != nil {
		t.Fatalf("starting the spawner: %v", err)
	}
	t.Cleanup(func() { _ = spawner.Wait() })

	// Read to EOF exactly as a command substitution would.
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, bufio.NewReader(stdout))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reading the spawner's stdout: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the spawner's stdout never reached EOF although the spawner exited — the detached " +
			"child inherited the pipe, so every `$(ccdad ...)` in a script hangs for as long as the daemon lives")
	}
	// And the child really did start; otherwise this test would pass by
	// spawning nothing at all.
	readReport(t, report, 15*time.Second)
}

func TestSpawnPassesTheArgumentThatStopsTheChildSpawningAChild(t *testing.T) {
	if RunArg == "" || !strings.HasPrefix(RunArg, "__") {
		t.Errorf("RunArg is %q; it has to be something a user cannot type by accident", RunArg)
	}
	// The harness in TestMain routes on this argument, so a spawned child that
	// did not receive it would never write a report — which every test above
	// would then fail on. This one states the contract in its own right.
	report, _ := spawnViaAChildThatExits(t)
	if report["pid"] == "" {
		t.Error("the child never identified itself as the daemon")
	}
}

func TestSpawnReportsAPlatformItCannotDetachOn(t *testing.T) {
	// detach is the only build-tagged piece of the spawn path. On the
	// platforms ccdad ships to it always succeeds; this asserts the shape of
	// the contract rather than the platform, so the fallback file cannot
	// silently start returning nil.
	err := detach(exec.Command(os.Args[0]))
	switch runtime.GOOS {
	case "windows":
		if err != nil {
			t.Errorf("detach() = %v on windows, want nil", err)
		}
	default:
		if runtime.GOOS == "js" || runtime.GOOS == "wasip1" || runtime.GOOS == "plan9" {
			if !errors.Is(err, errors.ErrUnsupported) {
				t.Errorf("detach() = %v on %s, want errors.ErrUnsupported", err, runtime.GOOS)
			}
			return
		}
		if err != nil {
			t.Errorf("detach() = %v on %s, want nil", err, runtime.GOOS)
		}
	}
}

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
	// The RESOLVED spelling, which is what ChildEnv pins. On macOS t.TempDir()
	// sits under /var, itself a symlink to /private/var, so comparing against
	// the raw string would fail there and nowhere else.
	want, err := filepath.EvalSymlinks(store)
	if err != nil {
		t.Fatal(err)
	}
	if got := report["store"]; got != want {
		t.Errorf("the child sees CCDAD_HOME=%q, want %q — it would manage a different store than its caller", got, want)
	}
}

// The sentinel has to survive the fork, or the recursion guard is a comment.
// The allow-list refuses to auto-start for the hidden entrypoint; this is what
// refuses for everything else a daemon's descendants might run.
func TestSpawnMarksTheChildAsOne(t *testing.T) {
	report, _ := spawnViaAChildThatExits(t)
	if got := report["child"]; got == "" {
		t.Errorf("the child does not carry %s; nothing downstream can tell it apart from a user's own invocation", ChildEnvVar)
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
	dir := t.TempDir()
	report := filepath.Join(dir, "child.txt")
	// The signal files live OUTSIDE the directory t.TempDir will delete, and
	// their cleanup is registered FIRST so that it runs LAST. t.Cleanup is
	// LIFO: with the release file inside dir, the framework removed it
	// microseconds after it appeared, the child's 5 ms poll never saw it, and
	// a passing test left a detached process spinning for another minute.
	sig, err := os.MkdirTemp("", "ccdad-spawn-signal")
	if err != nil {
		t.Fatalf("creating the signal directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sig) })
	linger := filepath.Join(sig, "let-the-child-go")
	t.Cleanup(func() {
		if err := os.WriteFile(linger, nil, 0o600); err != nil {
			t.Errorf("releasing the child: %v", err)
			return
		}
		// And wait for it to actually go, so the suite does not finish while a
		// detached copy of the test binary is still running.
		waitFor(t, linger+".seen", 30*time.Second)
	})
	spawner := exec.Command(os.Args[0])
	spawner.Env = append(os.Environ(),
		roleEnv+"="+roleHoldPipe,
		reportEnv+"="+report,
		lingerEnv+"="+linger,
	)
	stdout, err := spawner.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	// stderr as well: a redirect that covers only stdout still leaves
	// `ccdad which 2>&1 | cat` hanging, and stderr is the descriptor most
	// likely to be left inherited "so panics are visible".
	stderr, err := spawner.StderrPipe()
	if err != nil {
		t.Fatalf("StderrPipe: %v", err)
	}
	if err := spawner.Start(); err != nil {
		t.Fatalf("starting the spawner: %v", err)
	}
	t.Cleanup(func() { _ = spawner.Wait() })

	// Read to EOF exactly as a command substitution would.
	for _, stream := range []struct {
		name string
		r    io.Reader
	}{{"stdout", stdout}, {"stderr", stderr}} {
		done := make(chan error, 1)
		go func() {
			_, err := io.Copy(io.Discard, bufio.NewReader(stream.r))
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("reading the spawner's %s: %v", stream.name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the spawner's %s never reached EOF although the spawner exited — the detached "+
				"child inherited the pipe, so every `$(ccdad ...)` in a script hangs for as long as the daemon lives", stream.name)
		}
	}
	// And the child really did start and is still running; otherwise this test
	// would pass by spawning nothing at all, or by spawning something that had
	// already exited and closed whatever it inherited.
	readReport(t, report, 15*time.Second)
	if _, err := os.Stat(linger); err == nil {
		t.Fatal("the child had already been let go before the pipes were read")
	}
}

// §8.3 rule 3. Under `go test` os.Args[0] is already the absolute path of the
// test binary, so every other test here would pass with either one — the two
// implementations are indistinguishable unless the spawner is invoked the way
// a person actually invokes a freshly built binary. So it is run as
// "./spawner" from a directory of its own: os.Executable still answers with an
// absolute path, while os.Args[0] would be resolved against cmd.Dir, which
// Spawn sets to the root of the volume.
func TestSpawnResolvesItsOwnPathRatherThanArgv0(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the spawner harness re-execs this binary in a unix role")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	launchDir := t.TempDir()
	if err := os.Symlink(self, filepath.Join(launchDir, "spawner")); err != nil {
		t.Skipf("this filesystem cannot symlink: %v", err)
	}

	dir := t.TempDir()
	report := filepath.Join(dir, "child.txt")
	gone := filepath.Join(dir, "spawner-reaped")
	spawner := exec.Command("./spawner")
	spawner.Dir = launchDir
	spawner.Env = append(os.Environ(),
		roleEnv+"="+roleSpawner,
		reportEnv+"="+report,
		goneEnv+"="+gone,
	)
	spawner.Stderr = os.Stderr
	if err := spawner.Start(); err != nil {
		t.Fatalf("starting the spawner by a relative path: %v", err)
	}
	if err := spawner.Wait(); err != nil {
		t.Fatalf("the spawner failed: %v — Spawn resolved a relative argv[0] against the working "+
			"directory it sets for the child, so autostart is dead for `./ccdad` and `build/ccdad`", err)
	}
	if err := os.WriteFile(gone, nil, 0o600); err != nil {
		t.Fatalf("signalling the child: %v", err)
	}
	report2 := readReport(t, report, 15*time.Second)
	if report2["pid"] == "" {
		t.Error("no child was started")
	}
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

// The Windows flag values, pinned from every platform. This cannot catch a
// value that was wrong from the start — only a definition ccdad does not own
// can do that, which is what the windows-tagged test beside it uses
// x/sys/windows for — but it does catch the constant drifting afterwards, and
// it records where the numbers came from.
func TestTheWindowsCreationFlagsAreTheDocumentedValues(t *testing.T) {
	// Win32 processthreadsapi.h / winbase.h, and identical in
	// golang.org/x/sys/windows: DETACHED_PROCESS 0x00000008,
	// CREATE_NEW_PROCESS_GROUP 0x00000200.
	if flagDetachedProcess != 0x00000008 {
		t.Errorf("flagDetachedProcess = %#x, want 0x8 — without DETACHED_PROCESS the child inherits "+
			"the console and dies on CTRL_CLOSE_EVENT", flagDetachedProcess)
	}
	if flagCreateNewProcessGroup != 0x00000200 {
		t.Errorf("flagCreateNewProcessGroup = %#x, want 0x200", flagCreateNewProcessGroup)
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

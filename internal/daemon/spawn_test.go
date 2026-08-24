package daemon

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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
//
// It runs on every platform. What stood here was a wholesale Windows skip
// reading "detachment on Windows is a console and process-group property, not a
// session one" — true, and an answer to the question only ONE of its five
// callers asks. The other four ask about the store, the marker, the working
// directory and the argument, none of which is a unix concept, and the skip
// meant Spawn() itself executed on no Windows machine: os.OpenFile(os.DevNull),
// cmd.Start, Process.Release and the DETACHED_PROCESS creation flags were
// covered there by a test asserting a SysProcAttr struct literal. The session
// assertion now carries the skip itself.
//
// Nothing in this harness is unix-specific. The roles are selected before
// testing.Main runs and re-exec THIS binary, whatever it was built for.
//
// The worst case is bounded on purpose, because the subject is a process that
// deliberately outlives its parent and a leaked one on a shared runner is worse
// than a red job: the grandchild waits at most 20 s for the "parent reaped"
// file, and at most 30 s for the pipe test's release file, then exits on its
// own whatever the test did or failed to do.
func spawnViaAChildThatExits(t *testing.T, extraEnv ...string) (childReport, int) {
	t.Helper()
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
// The obvious assertion is "PPID is 1", and that is what this host shows, but
// it is not portable: any ancestor holding PR_SET_CHILD_SUBREAPER — some
// container inits, some session managers — collects the orphan instead of
// init, and the assertion would be flaky for a reason that has nothing to do
// with ccdad. What is always true is that the child no longer belongs to the
// process that started it.
//
// Unix only, and this is where the skip that used to sit on the harness
// belongs. A session is a unix object, and Windows does not reparent an orphan
// either: syscall.Getppid there reads the creator's pid out of a process
// snapshot and keeps answering with it long after that process is gone, so
// `ppid != spawnerPID` would be FALSE for a child that is properly detached.
// What detachment means on Windows is asserted by
// TestSpawnLeavesTheChildWithNoConsole and by the stdout test below.
func TestSpawnLeavesTheChildInItsOwnSessionAndReparented(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a session is a unix object and Windows does not reparent an orphan; see the comment above")
	}
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
//
// It runs everywhere. The skip that stood here read "the spawner harness
// re-execs this binary in a unix role", which was never true of the harness,
// and this is the assertion that carries BEST to Windows: the question there is
// handle inheritance rather than sessions, and a pipe handle duplicated into
// the detached child holds the read end open exactly as an inherited descriptor
// does. What it cannot prove there is that DETACHED_PROCESS took — Go hands
// CreateProcess an explicit handle list, so this would pass with the creation
// flags deleted — which is what TestSpawnLeavesTheChildWithNoConsole is for.
func TestSpawnDoesNotLeaveTheChildHoldingOurStdout(t *testing.T) {
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

// Re-exec os.Executable(), not os.Args[0]. Under `go test` os.Args[0] is
// already the absolute path of the test binary, so every other test here would
// pass with either one — the two implementations are indistinguishable unless
// the spawner is invoked the way a person actually invokes a freshly built
// binary. So it is run as "./spawner" from a directory of its own:
// os.Executable still answers with an absolute path, while os.Args[0] would be
// resolved against cmd.Dir, which Spawn sets to the root of the volume.
//
// On Windows the same mistake is worse rather than equivalent: os/exec resolves
// a relative Path against cmd.Dir through lookExtensions and syscall.StartProcess
// absolutises it against that directory, so `os.Args[0]` there would be looked
// for at C:\spawner.exe.
func TestSpawnResolvesItsOwnPathRatherThanArgv0(t *testing.T) {
	launchDir, argv0 := secondNameForThisBinary(t)

	dir := t.TempDir()
	report := filepath.Join(dir, "child.txt")
	gone := filepath.Join(dir, "spawner-reaped")
	spawner := exec.Command(argv0)
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

// secondNameForThisBinary copies the test binary into a directory of its own
// and returns that directory together with the RELATIVE name to start it by.
//
// A copy, where a symlink stood before. Windows creates a symlink only for a
// process holding SeCreateSymbolicLinkPrivilege or running with Developer Mode
// on, so a symlink would send this test straight back to skipping on the
// platform it was just enabled for — and a copy is exercised on every platform
// rather than only on the one where the symlink happens to fail, which is the
// same complaint that put this whole file in the queue.
//
// The .exe is not decoration. os/exec resolves a relative Path on Windows
// through lookExtensions, which appends PATHEXT and treats a name that already
// carries a listed extension as resolved; a file with no extension is not
// executable there under any spelling.
//
// The directory is deliberately NOT t.TempDir(). The grandchild is still
// running this image when the test returns, and Windows refuses to delete the
// image of a running process — so the removal is retried, the way
// internal/cclock's stealLock retries its own removal and for the same reason.
func secondNameForThisBinary(t *testing.T) (dir, argv0 string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	src, err := os.Open(self)
	if err != nil {
		t.Fatalf("opening this test binary: %v", err)
	}
	defer src.Close()

	dir, err = os.MkdirTemp("", "ccdad-spawn-launch")
	if err != nil {
		t.Fatalf("creating the launch directory: %v", err)
	}
	t.Cleanup(func() { removeWhenTheChildLetsGo(t, dir) })

	name := "spawner"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dst, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		t.Fatalf("copying this test binary: %v", err)
	}
	if err := dst.Close(); err != nil {
		t.Fatalf("closing %s: %v", name, err)
	}
	return dir, "." + string(os.PathSeparator) + name
}

// removeWhenTheChildLetsGo deletes a directory holding the image of a process
// this test started and cannot wait for. On Windows that deletion fails for as
// long as the process runs, and the grandchild here exits on its own schedule —
// bounded by the deadlines in runAsSpawnedDaemon, which is why this can be a
// bounded retry rather than a leak.
func removeWhenTheChildLetsGo(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := os.RemoveAll(dir); err == nil {
			return
		} else if time.Now().After(deadline) {
			t.Errorf("could not remove %s within 30s: %v — a process is still holding it", dir, err)
			return
		}
		time.Sleep(10 * time.Millisecond)
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

// The daemon and `ccdad probe` share one spelling of this command line through
// the constants above, and this is the assertion that the spelling the daemon
// builds is the one the command declares. A rename that touched only one side
// would leave a daemon spawning a usage error, silently, forever.
func TestTheProbeArgvIsTheOneTheCommandDeclares(t *testing.T) {
	want := []string{ProbeArg, "--" + ProbeUUIDFlag, "u-1", "--" + ProbeForceFlag, "--" + ProbeModelFlag, "opus"}
	got := probeArgv("u-1", "opus")
	if !slices.Equal(got, want) {
		t.Fatalf("probe argv = %q, want %q", got, want)
	}
	// No --model when there is no family to name: an empty flag value is a model
	// named "" rather than the default one.
	plain := probeArgv("u-1", "")
	for _, a := range plain {
		if a == "--"+ProbeModelFlag {
			t.Fatalf("probe argv = %q, want no --%s when no family was chosen", plain, ProbeModelFlag)
		}
	}
}

// Whether this machine has Claude Code on it is not something a test can
// arrange, so the resolver is a var and this is the test that says so. The
// daemon asks BEFORE it stamps an attempt, so the answer has to be an error it
// can read rather than a spawn that fails later.
func TestProbeAvailableSaysWhatIsMissing(t *testing.T) {
	saved := lookClaude
	t.Cleanup(func() { lookClaude = saved })
	lookClaude = func(string) (string, error) {
		return "", errors.New(`exec: "claude": executable file not found in $PATH`)
	}
	err := ProbeAvailable()
	if err == nil {
		t.Fatal("ProbeAvailable said a machine with no claude on it could probe")
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("the answer does not name what is missing: %v", err)
	}

	lookClaude = func(string) (string, error) { return "/usr/local/bin/claude", nil }
	if err := ProbeAvailable(); err != nil {
		t.Errorf("ProbeAvailable = %v on a machine that has claude", err)
	}
}

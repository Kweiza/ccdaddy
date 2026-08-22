package daemon

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// isolate points the store at a directory of this test's own, the way every
// other package in this tree does. CCDAD_HOME is read by ccpath.StoreHome, so
// unlike HOME it is honoured on every platform.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CCDAD_HOME", dir)
	return dir
}

// Environment variables that put a re-executed copy of this test binary into a
// role other than "run the tests".
const (
	roleEnv    = "CCDAD_TEST_ROLE"
	reportEnv  = "CCDAD_TEST_REPORT"
	goneEnv    = "CCDAD_TEST_PARENT_GONE"
	lingerEnv  = "CCDAD_TEST_CHILD_LINGER"
	readyEnv   = "CCDAD_TEST_READY"
	releaseEnv = "CCDAD_TEST_RELEASE"

	roleSpawner  = "spawner"
	roleHolder   = "singleton-holder"
	roleHoldPipe = "spawner-writes-nothing"
	roleDaemon   = "daemon"

	// stderrMarker is written to the daemon's own descriptor 2, to prove it
	// lands in daemon.log rather than in the null device Spawn hands the child.
	stderrMarker = "this line went to descriptor 2"
)

// TestMain turns this test binary into its own fixture.
//
// Detachment cannot be asserted by inspecting a SysProcAttr struct — that tests
// the struct, not the operating system. It needs a real child, and a child that
// has actually been orphaned needs a real parent that has actually exited. So
// the binary re-execs itself in one of three roles, and the roles are decided
// before testing.Main ever sees the argument list.
//
// The daemon role is selected by ARGUMENT rather than by environment on
// purpose: Spawn inherits the environment wholesale, so the spawner role would
// otherwise be inherited by the very child the spawner starts.
func TestMain(m *testing.M) {
	for _, arg := range os.Args[1:] {
		if arg == RunArg {
			os.Exit(runAsSpawnedDaemon())
		}
	}
	switch os.Getenv(roleEnv) {
	case roleSpawner, roleHoldPipe:
		if err := Spawn(); err != nil {
			fmt.Fprintln(os.Stderr, "spawner:", err)
			os.Exit(1)
		}
		// Exit immediately and leave the child orphaned. Whatever the child
		// reports about its parent is only meaningful after this returns.
		os.Exit(0)
	case roleHolder:
		os.Exit(runAsSingletonHolder())
	case roleDaemon:
		os.Exit(runAsDaemon())
	}
	// The real redirect points the TEST BINARY's file descriptor 2 at a log in a
	// temp directory, which would swallow `go test`'s own output. Neutralised
	// here rather than in each test, because a test that forgets does not fail —
	// it silently takes the output of every test after it with it. The roles
	// above have already exited, so a subprocess daemon still gets the real one,
	// and the one test that exercises it calls platformRedirectStderr directly.
	redirectStderr = func(*os.File) error { return nil }
	os.Exit(m.Run())
}

// runAsSpawnedDaemon is what Spawn actually starts. It records what it can see
// about its own process and exits.
func runAsSpawnedDaemon() int {
	report := os.Getenv(reportEnv)
	if report == "" {
		// Reached only if the environment did not propagate, which is itself
		// the thing under test — fail loudly rather than silently doing
		// nothing.
		return 3
	}
	sid, sidErr := sessionID()
	cwd, _ := os.Getwd()
	// Reparenting happens when the PARENT exits, and the parent's identity is
	// therefore only settled once the test has reaped it. Sampling ppid on
	// arrival races that: at full speed this process usually starts AFTER the
	// spawner is gone and sees the new parent, while under `go test -race` it
	// usually starts first and sees the old one. Neither ordering is wrong,
	// and an assertion that depends on which one happened is not measuring
	// detachment.
	//
	// So the test tells us when it has reaped the spawner, and only then do we
	// look. Both orderings then produce the same answer.
	startPPID := os.Getppid()
	if gone := os.Getenv(goneEnv); gone != "" {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(gone); err == nil {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	lines := []string{
		"pid=" + strconv.Itoa(os.Getpid()),
		"ppid=" + strconv.Itoa(os.Getppid()),
		"start_ppid=" + strconv.Itoa(startPPID),
		"sid=" + strconv.Itoa(sid),
		"sid_err=" + fmt.Sprint(sidErr),
		"cwd=" + cwd,
		"store=" + os.Getenv("CCDAD_HOME"),
	}
	if err := os.WriteFile(report, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return 4
	}
	// A real daemon does not exit, and that is the whole reason inheriting a
	// descriptor is fatal: the pipe stays open for as long as it lives. A
	// child that exits immediately closes any leaked descriptor at once and
	// hides the bug, so the pipe test asks this one to stay.
	if linger := os.Getenv(lingerEnv); linger != "" {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(linger); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		// Say so on the way out. A test that only writes the release file has
		// no way to know the child ever saw it, and the file lives in a
		// directory the test framework is about to delete — which is how a
		// passing run left a detached process polling for another minute after
		// `go test` printed ok.
		_ = os.WriteFile(linger+".seen", nil, 0o600)
	}
	return 0
}

// runAsDaemon is a real daemon in a process of its own. Signals cannot be
// tested any other way: delivering SIGTERM to the test binary kills the test
// binary.
func runAsDaemon() int {
	ready := os.Getenv(readyEnv)
	var once sync.Once
	err := Run(context.Background(), Options{
		Interval: 10 * time.Millisecond,
		Tick: func(context.Context) error {
			once.Do(func() {
				// Written to descriptor 2, which Run is supposed to have pointed
				// at daemon.log. Spawn hands the daemon /dev/null on all three,
				// so without that redirect this line is gone forever.
				fmt.Fprintln(os.Stderr, stderrMarker)
				_ = os.WriteFile(ready, []byte("up\n"), 0o600)
			})
			return nil
		},
		Snapshot: func() Status { return Status{ActiveUUID: "uuid-a"} },
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon:", err)
		return 1
	}
	return 0
}

// runAsSingletonHolder takes the singleton in a process of its own and holds it
// until told to let go. A second process is the only way to observe the
// singleton doing its job.
func runAsSingletonHolder() int {
	s, err := AcquireSingleton()
	if err != nil {
		fmt.Fprintln(os.Stderr, "holder:", err)
		return 1
	}
	if err := os.WriteFile(os.Getenv(readyEnv), []byte("held\n"), 0o600); err != nil {
		return 1
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(os.Getenv(releaseEnv)); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := s.Release(); err != nil {
		return 1
	}
	return 0
}

// childReport is what runAsSpawnedDaemon wrote, parsed.
type childReport map[string]string

func readReport(t *testing.T, path string, within time.Duration) childReport {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(path)
		// A report is only complete once its last line has landed; the child
		// writes it in one call, but the read can still race the write on
		// some filesystems.
		if err == nil && strings.HasSuffix(string(body), "\n") {
			out := childReport{}
			for _, line := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
				k, v, _ := strings.Cut(line, "=")
				out[k] = v
			}
			return out
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no report at %s after %s — the detached child never ran, or never got the environment naming this file", path, within)
	return nil
}

func (r childReport) num(t *testing.T, key string) int {
	t.Helper()
	n, err := strconv.Atoi(r[key])
	if err != nil {
		t.Fatalf("the child reported %s=%q, which is not a number", key, r[key])
	}
	return n
}

func waitFor(t *testing.T, path string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s never appeared within %s", path, within)
}

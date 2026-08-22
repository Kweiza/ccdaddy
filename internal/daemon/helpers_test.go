package daemon

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// isolate points the store at a directory of this test's own, the way every
// other package in this tree does. CCDAD_HOME is read by ccpath.StoreHome, so
// unlike HOME it is honoured on every platform — the HOME trap has already
// escaped a suite into a real profile once, on Windows.
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
	readyEnv   = "CCDAD_TEST_READY"
	releaseEnv = "CCDAD_TEST_RELEASE"

	roleHolder = "singleton-holder"
)

// TestMain turns this test binary into its own fixture. A singleton only means
// anything across processes, so the only way to observe it doing its job is to
// have a second process take it and hold it.
func TestMain(m *testing.M) {
	if os.Getenv(roleEnv) == roleHolder {
		os.Exit(runAsSingletonHolder())
	}
	os.Exit(m.Run())
}

// runAsSingletonHolder takes the singleton in a process of its own and holds it
// until told to let go.
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

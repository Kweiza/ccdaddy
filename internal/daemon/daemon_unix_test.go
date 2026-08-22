//go:build unix

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startDaemonProcess runs the real Run in a process of its own, which is the
// only place a signal can be delivered to it without also delivering it to the
// test binary.
func startDaemonProcess(t *testing.T, store string) (*exec.Cmd, string) {
	t.Helper()
	signals := t.TempDir()
	ready := filepath.Join(signals, "ready")

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		roleEnv+"="+roleDaemon,
		readyEnv+"="+ready,
		"CCDAD_HOME="+store,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the daemon process: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_ = cmd.Wait()
	})
	waitFor(t, ready, 20*time.Second)
	return cmd, ready
}

func waitForLogLine(t *testing.T, needle string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if body, err := os.ReadFile(mustPath(LogPath())); err == nil && strings.Contains(string(body), needle) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	body, _ := os.ReadFile(mustPath(LogPath()))
	t.Fatalf("%q never appeared in daemon.log within %s. Log:\n%s", needle, within, body)
}

// The handler sets a stop channel; it does NOT os.Exit. The difference is
// observable: exiting from the handler skips the final status flush and the
// pidfile truncation, and a tick killed mid-swap abandons Claude Code's three
// lock directories on disk — where cclock's stale windows are 60 s, 60 s and
// 15 s, so Claude Code's own token refresh wedges for up to a minute.
func TestSIGTERMShutsTheDaemonDownCleanly(t *testing.T) {
	store := isolate(t)
	cmd, _ := startDaemonProcess(t, store)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the daemon exited with %v, want a clean 0", err)
	}

	s, ok, err := ReadStatus()
	if err != nil || !ok {
		t.Fatalf("ReadStatus: ok=%v err=%v", ok, err)
	}
	if !s.Stopped {
		t.Error("the daemon did not publish a stopped document; the handler exited instead of stopping the loop")
	}
	if pid, ok, err := ReadPID(); ok || err != nil {
		t.Errorf("ReadPID() = (%d, %v, %v), want the pidfile truncated", pid, ok, err)
	}
	if _, err := os.Stat(mustPath(LockPath())); err != nil {
		t.Errorf("the lock file was removed: %v", err)
	}
	if held, err := SingletonHeld(); err != nil || held {
		t.Errorf("SingletonHeld() = (%v, %v) after the daemon exited", held, err)
	}
}

// Run points the daemon's descriptor 2 at daemon.log. Everything a panic or a
// runtime fatal ever writes goes there and nowhere else, so if the redirect is
// not made the crash leaves no trace at all.
func TestTheDaemonsOwnStderrLandsInItsLog(t *testing.T) {
	store := isolate(t)
	cmd, _ := startDaemonProcess(t, store)
	waitForLogLine(t, stderrMarker, 10*time.Second)
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
}

func TestSIGINTShutsTheDaemonDownCleanly(t *testing.T) {
	store := isolate(t)
	cmd, _ := startDaemonProcess(t, store)

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the daemon exited with %v, want a clean 0", err)
	}
	s, ok, err := ReadStatus()
	if err != nil || !ok || !s.Stopped {
		t.Errorf("after SIGINT: ok=%v err=%v stopped=%v", ok, err, s.Stopped)
	}
}

// Setsid means a closing shell will not send this, but `pkill -HUP` will, and
// SIGHUP's default disposition is to terminate. Ignoring it has to be a
// decision written down, not an omission.
func TestSIGHUPDoesNotKillTheDaemon(t *testing.T) {
	store := isolate(t)
	cmd, _ := startDaemonProcess(t, store)

	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitForLogLine(t, "hangup", 10*time.Second)

	// Still there, and still the daemon.
	if held, err := SingletonHeld(); err != nil || !held {
		t.Fatalf("SingletonHeld() = (%v, %v) after SIGHUP; the daemon died", held, err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the daemon exited with %v after SIGHUP then SIGTERM, want a clean 0", err)
	}
}

// A panic and a runtime fatal reach descriptor 2 without passing through any
// logger, and Spawn hands the child /dev/null on all three. Without this
// redirect the only trace a crash will ever leave is discarded — and rotation
// has to carry it over, or every crash after the first rotation is lost too.
func TestCaptureStderrFollowsTheLogThroughARotation(t *testing.T) {
	isolate(t)
	// TestMain neutralises the redirect for the suite. This is the one test that
	// wants the real thing, so it puts it back for its own duration.
	savedImpl := redirectStderr
	redirectStderr = platformRedirectStderr
	defer func() { redirectStderr = savedImpl }()

	saved, err := syscall.Dup(int(os.Stderr.Fd()))
	if err != nil {
		t.Fatalf("saving the test binary's stderr: %v", err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		f := os.NewFile(uintptr(saved), "saved-stderr")
		_ = platformRedirectStderr(f)
		_ = f.Close()
	}
	defer restore()

	l, err := openLog(mustPath(LogPath()), 64, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.CaptureStderr(); err != nil {
		restore()
		t.Fatalf("CaptureStderr: %v", err)
	}

	os.Stderr.WriteString("a panic before the rotation\n")
	l.Printf("%s", strings.Repeat("x", 200))
	if _, err := l.RotateIfLarge(); err != nil {
		restore()
		t.Fatalf("RotateIfLarge: %v", err)
	}
	os.Stderr.WriteString("a panic after the rotation\n")
	restore()

	if got := readFile(t, mustPath(LogPath())+".1"); !strings.Contains(got, "before the rotation") {
		t.Errorf("pre-rotation stderr did not reach the log:\n%s", got)
	}
	if got := readFile(t, mustPath(LogPath())); !strings.Contains(got, "after the rotation") {
		t.Errorf("post-rotation stderr went to the rotated inode, not the new log:\n%s", got)
	}
}

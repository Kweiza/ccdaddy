//go:build unix

package daemon

import (
	"testing"
	"time"
)

// The whole mechanism `ccdad daemon stop` is built on, against a real daemon in
// a process of its own: the request goes to the pid, and the answer to "is it
// gone" comes from the singleton, never from the pid.
func TestRequestShutdownStopsARunningDaemon(t *testing.T) {
	store := isolate(t)
	cmd, _ := startDaemonProcess(t, store)

	pid, ok, err := ReadPID()
	if err != nil || !ok {
		t.Fatalf("ReadPID() = (%d, %v, %v); a running daemon must have recorded its pid", pid, ok, err)
	}
	if pid != cmd.Process.Pid {
		t.Fatalf("the pidfile records %d, but the daemon is %d", pid, cmd.Process.Pid)
	}
	if err := RequestShutdown(pid); err != nil {
		t.Fatalf("RequestShutdown(%d): %v", pid, err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the daemon exited with %v, want a clean 0 — the request must reach the handler, not kill the process", err)
	}

	// The singleton is the only thing that answers this, and it is what `ccdad
	// daemon stop` polls. A clean stop also leaves the final document behind.
	deadline := time.Now().Add(5 * time.Second)
	for {
		held, err := SingletonHeld()
		if err != nil {
			t.Fatalf("SingletonHeld(): %v", err)
		}
		if !held {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatal("the singleton was still held five seconds after the daemon exited")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if s, ok, err := ReadStatus(); err != nil || !ok || !s.Stopped {
		t.Errorf("after RequestShutdown: ok=%v err=%v stopped=%v, want the final document", ok, err, s.Stopped)
	}
}

//go:build unix

package store

import (
	"errors"
	"syscall"
	"testing"
)

// Unix only, and a build tag rather than a runtime skip because syscall.Kill
// does not exist on Windows -- a t.Skip would still have to compile. Windows
// has no way for a process to raise a console Ctrl-C event at itself, so the
// production watch there is covered by the seam tests and by nothing else,
// which is stated here rather than left as an unexplained gap.

// TestARealSIGINTInsideATransactionDoesNotKillTheProcess is the only test here
// that exercises the production watch, and it asserts the one thing the seam
// cannot: that signal.Notify is actually installed over this span. Without it
// the kill below takes the default action and this test binary dies outright,
// which is the failure being asserted against.
//
// It does NOT pin which side of the commit the signal lands on. mutate's poll
// between fn and the save is a non-blocking read, and whether the runtime has
// forwarded the signal to the channel by then is a scheduling question this
// test has no way to settle. What IS deterministic is the sentinel: close
// drains after signal.Stop, and Stop calls signalWaitUntilIdle precisely so
// that a signal raised before it is delivered rather than discarded. The two
// sides are pinned exactly, on the seam, by the two tests above.
func TestARealSIGINTInsideATransactionDoesNotKillTheProcess(t *testing.T) {
	withStore(t)

	err := WithStore(func(s *Store) error {
		if err := s.Add(Account{UUID: "u-1"}, sampleCreds("AT-1")); err != nil {
			return err
		}
		return syscall.Kill(syscall.Getpid(), syscall.SIGINT)
	})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("WithStore() = %v, want ErrInterrupted from a real SIGINT", err)
	}
}

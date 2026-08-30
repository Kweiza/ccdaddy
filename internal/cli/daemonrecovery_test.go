package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/daemon"
)

// stubSuccessor records what the entrypoint asks for instead of detaching a
// real daemon from the test binary.
func stubSuccessor(t *testing.T, err error) *int {
	t.Helper()
	saved := spawnSuccessor
	t.Cleanup(func() { spawnSuccessor = saved })
	got := -1
	spawnSuccessor = func(next int) error {
		got = next
		return err
	}
	return &got
}

func stubWedgedRun(t *testing.T, wedge error) {
	t.Helper()
	saved := runDaemon
	t.Cleanup(func() { runDaemon = saved })
	runDaemon = func(context.Context, daemon.Options) error { return wedge }
}

// The wedged daemon replaces itself, and the entrypoint is where that happens
// rather than anywhere inside daemon.Run: the successor's first act is to take
// the singleton, and Run gives it back in a defer. Only its caller runs late
// enough for the lock to be free.
func TestTheHiddenEntrypointReplacesAWedgedDaemon(t *testing.T) {
	isolate(t)
	stubWedgedRun(t, &daemon.WedgedError{Err: errors.New("boom"), NextRecovery: 2})
	got := stubSuccessor(t, nil)

	code, _, stderr, _ := runRoot(t, daemon.RunArg)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 after handing over cleanly (stderr %q)", code, stderr)
	}
	if *got != 2 {
		t.Fatalf("spawned the successor with count %d, want the one the wedge named", *got)
	}
}

// A successor that cannot be started leaves the machine with no daemon at all,
// which is the one state the recovery rule exists to avoid. It must not exit 0,
// and it must still name the wedge -- the replacement failing is the second
// fact, not a replacement for the first.
func TestAFailedReplacementIsReported(t *testing.T) {
	isolate(t)
	stubWedgedRun(t, &daemon.WedgedError{Err: errors.New("boom"), NextRecovery: 1})
	stubSuccessor(t, errors.New("no such binary"))

	code, _, stderr, top := runRoot(t, daemon.RunArg)
	if code == ExitOK {
		t.Fatal("a replacement that could not start exited 0")
	}
	if said := stderr + top; !strings.Contains(said, "boom") {
		t.Fatalf("output = %q, want the wedge that caused it still named", said)
	}
}

// A wedge with no budget never reaches here -- Run keeps ticking instead -- so
// an ordinary failure must not be mistaken for one and spawn anything.
func TestAnOrdinaryDaemonErrorSpawnsNoSuccessor(t *testing.T) {
	isolate(t)
	stubWedgedRun(t, errors.New("ordinary"))
	got := stubSuccessor(t, nil)

	if code, _, _, _ := runRoot(t, daemon.RunArg); code == ExitOK {
		t.Fatal("an ordinary failure exited 0")
	}
	if *got != -1 {
		t.Fatalf("an ordinary failure spawned a successor with count %d", *got)
	}
}

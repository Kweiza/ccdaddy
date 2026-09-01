package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// errRefusing stands for the one classification that makes a start pointless.
// cclink.keychainFailure is unexported, so this package cannot build the real
// error; loginStoreSurvivesRestart is stubbed to recognise this one instead,
// which is why that classification is a seam of its own.
var errRefusing = errors.New("security find-generic-password: interaction-not-allowed (exit 36)")

// stubLoginStore replaces the read this gate decides from, plus the unlock it
// offers, plus the spawn it guards. reads are returned in order, so a test can
// describe "refusing, then fine after the unlock".
func stubLoginStore(t *testing.T, tty bool, reads []error, unlockErr error) *int {
	t.Helper()
	sr, su, sv, st := loginStoreRead, unlockLoginKeychain, loginStoreSurvivesRestart, stdinIsTTY
	t.Cleanup(func() {
		loginStoreRead, unlockLoginKeychain, loginStoreSurvivesRestart, stdinIsTTY = sr, su, sv, st
	})
	loginStoreSurvivesRestart = func(err error) bool { return errors.Is(err, errRefusing) }

	n := 0
	loginStoreRead = func() error {
		if n < len(reads) {
			e := reads[n]
			n++
			return e
		}
		return reads[len(reads)-1]
	}
	unlocks := 0
	unlockLoginKeychain = func(context.Context) error { unlocks++; return unlockErr }
	stdinIsTTY = func() bool { return tty }
	return &unlocks
}

// decide runs the gate alone. The wiring into startDaemonFrom is one call a
// reader can see; what needs asserting is the decision, and driving the whole
// command would spend a singleton wait per case for nothing.
func decide(t *testing.T) (error, string) {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&out)
	cmd.SetOut(&out)
	return repairOrRefuseAnUnreadableLogin(cmd), out.String()
}

// THE 12:41 CASE. `ccdad daemon restart` from a session that cannot read the
// login printed "Started the ccdad daemon (pid N)." and exited 0, over a
// process that stood down on every one of its 1 Hz ticks for the rest of its
// life. The child detaches, so its own refusal reaches daemon.log and nowhere
// else.
func TestDaemonStartRefusesASessionThatCannotReadTheLogin(t *testing.T) {
	unlocks := stubLoginStore(t, false, []error{errRefusing}, nil)

	err, out := decide(t)
	if err == nil {
		t.Fatalf("the gate allowed a daemon that could never switch:\n%s", out)
	}
	if *unlocks != 0 {
		t.Fatal("an unattended start raised a password prompt")
	}
	for _, want := range []string{"cannot read", "audit session", "unlock-keychain"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the refusal does not carry %q:\n%s", want, out)
		}
	}
}

// Attended, the gate does not merely refuse: it offers the repair that works,
// and the password never passes through ccdad -- `security` does its own asking
// on the terminal.
func TestDaemonStartUnlocksWhenAttendedAndThenStarts(t *testing.T) {
	unlocks := stubLoginStore(t, true, []error{errRefusing, nil}, nil)

	err, out := decide(t)
	if err != nil {
		t.Fatalf("the gate refused after the store became readable: %v\n%s", err, out)
	}
	if *unlocks != 1 {
		t.Fatalf("the unlock was offered %d times, want 1:\n%s", *unlocks, out)
	}
	if !strings.Contains(out, "never sees the password") {
		t.Fatalf("the user is not told who is asking:\n%s", out)
	}
}

// An unlock that did not help still refuses. Starting anyway is the behaviour
// this gate exists to remove.
func TestDaemonStartStillRefusesWhenTheUnlockDidNotHelp(t *testing.T) {
	unlocks := stubLoginStore(t, true, []error{errRefusing}, errors.New("cancelled"))

	err, out := decide(t)
	if err == nil {
		t.Fatalf("the gate allowed a start although the store still refuses:\n%s", out)
	}
	if *unlocks != 1 {
		t.Fatalf("unlocks = %d, want the repair to have been attempted once", *unlocks)
	}
}

// A store that reads fine is untouched: no prompt, no refusal, nothing printed.
func TestDaemonStartIsUntouchedWhenTheLoginReadsFine(t *testing.T) {
	unlocks := stubLoginStore(t, true, []error{nil}, nil)

	err, out := decide(t)
	if err != nil || *unlocks != 0 || out != "" {
		t.Fatalf("a readable store was interfered with: err=%v unlocks=%d out=%q", err, *unlocks, out)
	}
}

// ONLY the refusal a successor inherits. A read that failed for any other
// reason may clear on its own -- a keychain locked in a session that CAN
// interact is cleared by an unlock with the daemon still running, and the loop
// already recovers from it. Refusing there would turn a self-healing wedge into
// a machine with no daemon at all.
func TestDaemonStartDoesNotRefuseAFaultThatMightClear(t *testing.T) {
	unlocks := stubLoginStore(t, true, []error{errors.New("dial tcp: i/o timeout")}, nil)

	err, out := decide(t)
	if err != nil || *unlocks != 0 {
		t.Fatalf("a fault that is not the audit-session refusal was treated as one: err=%v unlocks=%d\n%s",
			err, *unlocks, out)
	}
}

// THE GATE HAS TO BE WIRED IN, and testing the decision alone does not say so.
// A mutation that deleted the call from startDaemonFrom left every test above
// green: they call the decision directly. This one goes through the command, and
// it stays fast because the refusal returns BEFORE the spawn and its singleton
// wait.
func TestDaemonStartRunsTheGateBeforeSpawning(t *testing.T) {
	isolate(t)
	unlocks := stubLoginStore(t, false, []error{errRefusing}, nil)
	spawns := 0
	saved := spawnDaemon
	t.Cleanup(func() { spawnDaemon = saved })
	spawnDaemon = func(string) error { spawns++; return nil }

	code, _, stderr, _ := runRoot(t, "daemon", "start")
	if spawns != 0 {
		t.Fatalf("a daemon was spawned from a session that cannot read the login (%d):\n%s", spawns, stderr)
	}
	if code == ExitOK {
		t.Fatalf("daemon start exited 0 over a daemon that could never switch:\n%s", stderr)
	}
	if *unlocks != 0 {
		t.Fatal("an unattended start raised a password prompt")
	}
	if !strings.Contains(stderr, "Not starting") {
		t.Fatalf("the terminal was not told:\n%s", stderr)
	}
}

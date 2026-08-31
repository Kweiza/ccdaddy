package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// stubSurvivesRestart makes the loop's classification answer for a plain error,
// because the one error that really answers true is built by a `security` spawn
// this suite cannot make happen.
func stubSurvivesRestart(t *testing.T, inherited error) {
	t.Helper()
	saved := survivesRestart
	t.Cleanup(func() { survivesRestart = saved })
	survivesRestart = func(err error) bool { return errors.Is(err, inherited) }
}

// THE REGRESSION. Three replacements were spent on a locked login keychain, and
// every one of them was futile: macOS scopes errSecInteractionNotAllowed to the
// audit session, a child inherits its parent's, and each successor failed 1.1
// seconds after it started. The budget then had nothing left for a wedge a
// restart COULD have cleared.
func TestALoopDoesNotSpendAReplacementOnAFaultARestartInherits(t *testing.T) {
	inherited := errors.New("security find-generic-password: interaction-not-allowed (exit 36)")
	h := newHarness(t, func(context.Context, int) error { return inherited })
	stubSurvivesRestart(t, inherited)
	h.loop.WedgedAfter = 5 * time.Minute
	h.loop.RecoveryBudget = 3

	h.tick(t)
	h.clock.advance(5 * time.Minute)
	h.tick(t)
	h.tick(t)

	if err := h.stop(t); err != nil {
		t.Fatalf("Run = %v, want a clean stop rather than a replacement no successor could use", err)
	}
	body := readFile(t, mustPath(LogPath()))
	if got := strings.Count(body, "not replacing this daemon"); got != 1 {
		t.Fatalf("the decision was logged %d times, want exactly one:\n%s", got, body)
	}
	for _, want := range []string{"audit session", "a shell that can already read the keychain"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the line does not carry %q:\n%s", want, body)
		}
	}
}

// Any other cause still buys a replacement: a fresh process really might get a
// different answer, and refusing one for every failure would spend the opposite
// mistake.
func TestALoopStillReplacesItselfForACauseARestartMightFix(t *testing.T) {
	h := newHarness(t, func(context.Context, int) error { return errors.New("boom") })
	stubSurvivesRestart(t, errors.New("a different error entirely"))
	h.loop.WedgedAfter = 5 * time.Minute
	h.loop.RecoveryBudget = 3

	h.tick(t)
	h.clock.advance(5 * time.Minute)
	h.tick(t)

	if err := h.stop(t); !errors.Is(err, ErrWedged) {
		t.Fatalf("Run = %v, want ErrWedged", err)
	}
}

// A spent budget used to be permanent, so the daemon that watched the user
// unlock the keychain kept failing for the rest of the night: the decision had
// been taken an hour before the machine changed.
func TestAWedgeThatOutlastsTheRearmWindowGetsOneMoreAttempt(t *testing.T) {
	h := newHarness(t, func(context.Context, int) error { return errors.New("boom") })
	h.loop.WedgedAfter = 5 * time.Minute
	h.loop.RecoveryRearmAfter = time.Hour
	h.loop.RecoveryBudget = 0

	h.tick(t)
	h.clock.advance(5 * time.Minute)
	h.tick(t)
	if h.loop.Health().Rearmed {
		t.Fatal("re-armed before the window; a spent budget must hold for recoveryRearmAfter")
	}
	h.clock.advance(time.Hour)
	h.tick(t)

	if err := h.stop(t); !errors.Is(err, ErrWedged) {
		t.Fatalf("Run = %v, want ErrWedged once the re-arm window has passed", err)
	}
	if !h.loop.Health().Rearmed {
		t.Fatal("Rearmed is false, so the successor would inherit a spent count and never try again")
	}
}

// The successor of a re-armed loop must start a FRESH chain, or the one extra
// attempt is the last one forever -- which is the state the re-arm exists to
// leave.
func TestARearmedWedgeHandsTheSuccessorAFreshChain(t *testing.T) {
	if got := NextRecoveryCount(maxRecoveries, true); got != 1 {
		t.Fatalf("NextRecoveryCount(spent, freshChain) = %d, want 1", got)
	}
	if got := NextRecoveryCount(1, false); got != 2 {
		t.Fatalf("NextRecoveryCount(1, false) = %d, want 2", got)
	}
}

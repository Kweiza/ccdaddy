package cli

import (
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/codexswitch"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
)

func servingNow(t *testing.T) string {
	t.Helper()
	root, err := codexRoot()
	if err != nil {
		t.Fatal(err)
	}
	uuid, _ := codexswitch.ReadServing(root)
	return uuid
}

// The headline: naming a Codex account moves the pointer and writes nothing
// else. Claude Code's credentials file must be exactly as it was.
func TestSwitchToACodexAccountMovesThePointerAndNotTheLogin(t *testing.T) {
	isolate(t)
	seedAccount(t, "cl-1", "claude@example.com")
	seedCodexAccount(t, "cx-1", "codex@example.com")

	code, _, stderr, top := runRoot(t, "switch", "codex@example.com")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)\n%s", code, top, stderr)
	}
	if got := servingNow(t); got != "cx-1" {
		t.Fatalf("serving = %q, want cx-1", got)
	}
	assertNoLiveCredentials(t)
	if !strings.Contains(stderr, "from the next new thread") {
		t.Fatalf("stderr does not say when the repoint takes effect:\n%s", stderr)
	}
}

// The never-cross line for this command, checked directly rather than by
// reading the source: a codex switch must never call store.SetActive,
// switcher.Execute or switcher.RecordSwitch. Each of those three leaves an
// observable mark -- the credentials file, the store's ActiveUUID hint, and
// the CLAUDE side of the anti-flap cooldown -- and this asserts every one of
// them is exactly as a fresh store leaves it, while the codex-side cooldown
// codexswitch.Execute is supposed to stamp DID move, which is what tells a
// no-op implementation (one that skipped Execute too) from this one.
func TestSwitchToACodexAccountTouchesNoneOfTheClaudeSwitchMachinery(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")

	code, _, stderr, top := runRoot(t, "switch", "codex@example.com")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)\n%s", code, top, stderr)
	}
	assertNoLiveCredentials(t)

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if got := s.ActiveUUID(); got != "" {
		t.Fatalf("store.ActiveUUID() = %q after a codex switch, want \"\" -- store.SetActive was called", got)
	}

	st, err := strategy.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if at, to := st.LastSwitch(); !at.IsZero() || to != "" {
		t.Fatalf("LastSwitch() = (%v, %q), want the zero value -- switcher.RecordSwitch was called", at, to)
	}
	if at, to := st.CodexLastSwitch(); at.IsZero() || to != "cx-1" {
		t.Fatalf("CodexLastSwitch() = (%v, %q), want a real stamp naming cx-1 -- codexswitch.Execute's own "+
			"cooldown must still fire even though the Claude one must not", at, to)
	}
}

// A repoint takes effect on the NEXT NEW THREAD, and the daemon is what serves
// it. With no daemon running the sentence has to say so on the same line, or a
// user starts codex and finds nothing changed.
func TestSwitchToACodexAccountSaysWhenThereIsNoDaemon(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")
	stubSingleton(t, false, nil)

	code, _, stderr, top := runRoot(t, "switch", "codex@example.com")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)", code, top)
	}
	if !strings.Contains(stderr, "once the daemon runs") {
		t.Fatalf("stderr does not name the missing daemon:\n%s", stderr)
	}
}

func TestSwitchToACodexAccountIsRunningDaemonSaysNothingExtra(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")
	stubSingleton(t, true, nil)

	_, _, stderr, _ := runRoot(t, "switch", "codex@example.com")
	if strings.Contains(stderr, "once the daemon runs") {
		t.Fatalf("stderr names a missing daemon while one is running:\n%s", stderr)
	}
}

// Exit 3 is "the world is already as you asked", and a cron job that reads 0
// here would believe it had changed something.
func TestSwitchToTheCodexAccountAlreadyServingIsNothingToDo(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")
	if code, _, _, top := runRoot(t, "switch", "codex@example.com"); code != ExitOK {
		t.Fatalf("the first switch exited %d (%s)", code, top)
	}

	code, _, stderr, _ := runRoot(t, "switch", "codex@example.com")
	if code != ExitNothingToDo {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitNothingToDo, stderr)
	}
}

// --provider is an ASSERTION about the account the caller named. It exists so a
// script that means to move codex cannot silently rewrite Claude Code's login
// because an alias moved between providers.
func TestSwitchRefusesAProviderAssertionThatDoesNotHold(t *testing.T) {
	isolate(t)
	seedAccount(t, "cl-1", "claude@example.com")

	code, _, stderr, _ := runRoot(t, "switch", "claude@example.com", "--provider", "codex")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)\n%s", code, ExitUsage, stderr)
	}
	assertNoLiveCredentials(t)
}

func TestSwitchAcceptsAProviderAssertionThatHolds(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")

	code, _, stderr, top := runRoot(t, "switch", "codex@example.com", "--provider", "codex")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)\n%s", code, top, stderr)
	}
	if got := servingNow(t); got != "cx-1" {
		t.Fatalf("serving = %q, want cx-1", got)
	}
}

func TestSwitchRefusesAProviderNameItCannotRead(t *testing.T) {
	isolate(t)
	seedAccount(t, "cl-1", "claude@example.com")

	code, _, stderr, _ := runRoot(t, "switch", "claude@example.com", "--provider", "openai")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)\n%s", code, ExitUsage, stderr)
	}
}

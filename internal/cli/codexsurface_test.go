package cli

import (
	"strings"
	"testing"
)

// All three tables go through one method, so they cannot disagree about what a
// codex row is called. This asserts the two that package cli renders.
//
// The codex account's email is deliberately "one@example.com" rather than
// something spelling out the word "codex": rowFor hands back whichever line
// mentions the label, and an email containing the word would satisfy the
// Contains check from the ACCOUNT cell alone, no matter what the TYPE cell
// said -- which is exactly the row this test exists to check.
func TestTheListTableNamesACodexRowCodex(t *testing.T) {
	isolate(t)
	seedAccount(t, "cl-1", "claude@example.com")
	seedCodexAccount(t, "cx-1", "one@example.com")

	code, stdout, _, top := runRoot(t, "list")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s)", code, top)
	}
	if !strings.Contains(rowFor(t, stdout, "one@example.com"), "codex") {
		t.Fatalf("the list row for the codex account does not say codex:\n%s", stdout)
	}
	if strings.Contains(rowFor(t, stdout, "claude@example.com"), "codex") {
		t.Fatalf("the list row for the Claude account says codex:\n%s", stdout)
	}
}

func TestTheStatusTableNamesACodexRowCodex(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "one@example.com")

	code, stdout, _, top := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s)", code, top)
	}
	if !strings.Contains(rowFor(t, stdout, "one@example.com"), "codex") {
		t.Fatalf("the status row for the codex account does not say codex:\n%s", stdout)
	}
}

// `ccdad which` answers "which account is this machine spending", and on a
// machine with two providers that has two halves. The bare label is kept when
// there is only one, because a script reading `ccdad which` predates codex.
func TestWhichNamesBothProvidersWhenCodexIsServed(t *testing.T) {
	isolate(t)
	seedAccount(t, "cl-1", "claude@example.com")
	writeLiveFile(t, liveLoginJSON("RT-cl-1", ""))
	seedCodexAccount(t, "cx-1", "codex@example.com")
	if code, _, _, top := runRoot(t, "switch", "codex@example.com"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}

	code, stdout, _, top := runRoot(t, "which")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s)", code, top)
	}
	if !strings.Contains(stdout, "Claude: claude@example.com") ||
		!strings.Contains(stdout, "Codex: codex@example.com") {
		t.Fatalf("which does not name both providers:\n%s", stdout)
	}
}

func TestWhichIsUnchangedOnAMachineWithNoCodexAccount(t *testing.T) {
	isolate(t)
	seedAccount(t, "cl-1", "claude@example.com")
	writeLiveFile(t, liveLoginJSON("RT-cl-1", ""))

	code, stdout, _, _ := runRoot(t, "which")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.TrimSpace(stdout) != "claude@example.com" {
		t.Fatalf("which printed %q, want the bare label", strings.TrimSpace(stdout))
	}
}

func TestStatusNamesBothProvidersWhenCodexIsServed(t *testing.T) {
	isolate(t)
	seedAccount(t, "cl-1", "claude@example.com")
	writeLiveFile(t, liveLoginJSON("RT-cl-1", ""))
	seedCodexAccount(t, "cx-1", "codex@example.com")
	if code, _, _, top := runRoot(t, "switch", "codex@example.com"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}

	code, stdout, _, _ := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "Codex: codex@example.com") {
		t.Fatalf("status does not name the codex account:\n%s", stdout)
	}
}

// A pointer naming an account the store no longer has reads as no pointer, on
// every surface, exactly as it does inside the proxy.
func TestAPointerNamingNoStoredAccountRendersAsNoCodex(t *testing.T) {
	isolate(t)
	seedAccount(t, "cl-1", "claude@example.com")
	writeLiveFile(t, liveLoginJSON("RT-cl-1", ""))
	seedCodexAccount(t, "cx-1", "codex@example.com")
	if code, _, _, top := runRoot(t, "switch", "codex@example.com"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}
	if code, _, _, top := runRoot(t, "remove", "codex@example.com", "--yes"); code != ExitOK {
		t.Fatalf("setup remove = %d (%s)", code, top)
	}

	_, stdout, _, _ := runRoot(t, "which")
	if strings.Contains(stdout, "Codex:") {
		t.Fatalf("which names a codex account the store no longer has:\n%s", stdout)
	}
}

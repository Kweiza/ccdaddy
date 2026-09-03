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

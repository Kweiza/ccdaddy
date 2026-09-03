package cli

import (
	"strings"
	"testing"
)

// activeLineOf is `status`'s Active: line, verbatim. A test that wants to pin
// the whole line rather than a fragment of it needs the line isolated first --
// grepping the raw multi-line stdout for a substring is exactly the weaker
// check this helper exists to replace.
func activeLineOf(t *testing.T, stdout string) string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "Active:") {
			return line
		}
	}
	t.Fatalf("no Active: line in:\n%s", stdout)
	return ""
}

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
	// == against the whole line, not Contains against a fragment of it -- see
	// the comment on TestStatusNamesBothProvidersWhenCodexIsServed.
	want := "Claude: claude@example.com · Codex: codex@example.com"
	if got := strings.TrimSpace(stdout); got != want {
		t.Fatalf("which stdout = %q, want %q", got, want)
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

// The whole line, compared with ==, and not a Contains on one fragment of it:
// a mutation that reordered the two clauses, duplicated the Active: line, or
// appended stray text after it would still contain "Codex: codex@example.com"
// and pass a Contains check that only ever asked whether one known-good
// fragment survived. == is what actually pins the one-spelling property.
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
	want := "Active:  Claude: claude@example.com · Codex: codex@example.com"
	if got := activeLineOf(t, stdout); got != want {
		t.Fatalf("Active line = %q, want %q", got, want)
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

// A machine can have no Claude login attributed at all while codex is still
// being served. That is not a reason for `which` to go silent about the half
// of the question it CAN answer: `status` and the dashboard share
// loadSnapshot and both answer this exact state with a Codex clause, and
// `which` disagreeing with them because its OWN axis came back negative is
// the two-commands-two-answers failure this task exists to remove.
func TestWhichNamesCodexEvenWhenClaudeIsUnattributed(t *testing.T) {
	isolate(t)
	seedAccount(t, "cl-1", "claude@example.com")
	seedCodexAccount(t, "cx-1", "codex@example.com")
	if code, _, _, top := runRoot(t, "switch", "codex@example.com"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}
	// No writeLiveFile: Claude Code has no login file at all, so the Claude
	// axis is unattributed on purpose -- the state the exit code is about.

	code, stdout, stderr, _ := runRoot(t, "which")
	if code != ExitProbeNegative {
		t.Fatalf("exit = %d, want %d -- codex being served does not change the Claude verdict", code, ExitProbeNegative)
	}
	want := "Claude: " + noActiveAccountLabel + " · Codex: codex@example.com"
	if got := strings.TrimSpace(stdout); got != want {
		t.Fatalf("which stdout = %q, want %q", got, want)
	}
	if !strings.Contains(stderr, "not one ccdad manages") {
		t.Fatalf("stderr = %q, want the unattributed notice still", stderr)
	}
}

// A codex account can be seeded without ever being switched to: `add` writes
// the account list, and codexServingAccount answers from a SEPARATE document,
// the pointer codexswitch.ReadServing reads. status must answer from that
// pointer and not from "does a codex account exist anywhere in the store".
func TestStatusNamesNoCodexWhenNoAccountHasBeenSwitchedTo(t *testing.T) {
	isolate(t)
	seedAccount(t, "cl-1", "claude@example.com")
	writeLiveFile(t, liveLoginJSON("RT-cl-1", ""))
	seedCodexAccount(t, "cx-1", "codex@example.com")

	code, stdout, _, _ := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(stdout, "Codex:") {
		t.Fatalf("status names a codex account nothing has switched to:\n%s", stdout)
	}
}

// The pointer names ONE account, and a codex account surviving elsewhere in
// the store must not be read as a fallback for it: that would report an
// account nobody switched to, which is a different bug from the one
// TestAPointerNamingNoStoredAccountRendersAsNoCodex covers, where no codex
// account survives at all to be mistaken for the pointer's target.
func TestStatusNamesNoCodexWhenThePointerNamesARemovedAccount(t *testing.T) {
	isolate(t)
	seedAccount(t, "cl-1", "claude@example.com")
	writeLiveFile(t, liveLoginJSON("RT-cl-1", ""))
	seedCodexAccount(t, "cx-1", "gone@example.com")
	seedCodexAccount(t, "cx-2", "other@example.com")
	if code, _, _, top := runRoot(t, "switch", "gone@example.com"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}
	if code, _, _, top := runRoot(t, "remove", "gone@example.com", "--yes"); code != ExitOK {
		t.Fatalf("setup remove = %d (%s)", code, top)
	}

	code, stdout, _, _ := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(stdout, "Codex:") {
		t.Fatalf("status names a codex account nobody switched to, after the pointer's own account was removed:\n%s", stdout)
	}
}

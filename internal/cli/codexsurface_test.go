package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// activeLineOf is `status`'s Active: line, verbatim. A test that wants to pin
// the whole line rather than a fragment of it needs the line isolated first --
// grepping the raw multi-line stdout for a substring is exactly the weaker
// check this helper exists to replace.
func activeLineOf(t *testing.T, stdout string) string {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "Active:") {
			lines = append(lines, line)
		}
	}
	// Exactly one. A renderer that printed the clause twice would otherwise
	// pass silently: the first of the two identical lines still equals what
	// the caller wants, and a helper that only ever looked at the first match
	// would never notice the second.
	if len(lines) != 1 {
		t.Fatalf("%d Active: lines in stdout, want exactly 1:\n%s", len(lines), stdout)
	}
	return lines[0]
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

// The keys a consumer writes against. codexServingUuid is CONDITIONAL, like
// activeUuid beside it: absent means there is no pointer, and a consumer that
// saw an empty string could not tell that from a pointer at an account named "".
func TestListJSONCarriesTheCodexServingUuid(t *testing.T) {
	isolate(t)
	seedAccount(t, "cl-1", "claude@example.com")
	seedCodexAccount(t, "cx-1", "codex@example.com")
	if code, _, _, top := runRoot(t, "switch", "codex@example.com"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}

	payload := decodeJSON(t, "list", "--json")
	if got := payload["codexServingUuid"]; got != "cx-1" {
		t.Fatalf("codexServingUuid = %v, want cx-1", got)
	}
}

func TestListJSONOmitsTheCodexServingUuidWithNoPointer(t *testing.T) {
	isolate(t)
	seedAccount(t, "cl-1", "claude@example.com")

	payload := decodeJSON(t, "list", "--json")
	if _, present := payload["codexServingUuid"]; present {
		t.Fatalf("codexServingUuid is present on a machine with no pointer:\n%v", payload)
	}
}

func TestStatusJSONCarriesTheCodexServingUuid(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")
	if code, _, _, top := runRoot(t, "switch", "codex@example.com"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}

	payload := decodeJSON(t, "status", "--json")
	if got := payload["codexServingUuid"]; got != "cx-1" {
		t.Fatalf("codexServingUuid = %v, want cx-1", got)
	}
}

// The omission half of the pair above. `list` has had this test; `status`
// had not, and dropping statusPayload's `!= ""` guard on CodexServingUUID
// stays fully green without it -- a consumer would then see an empty string
// on a machine with no pointer and have no way to tell that from a pointer at
// an account actually named "".
func TestStatusJSONOmitsTheCodexServingUuidWithNoPointer(t *testing.T) {
	isolate(t)
	seedAccount(t, "cl-1", "claude@example.com")

	payload := decodeJSON(t, "status", "--json")
	if _, present := payload["codexServingUuid"]; present {
		t.Fatalf("codexServingUuid is present on a machine with no pointer:\n%v", payload)
	}
}

// NEVER-CROSS. `active` and `activeUuid` answer about Claude Code's login and
// nothing else. A fleet of three Claude accounts and two Codex ones, with one
// of each in use, must produce exactly one row marked active -- the Claude one.
func TestActiveStaysClaudesWithCodexAccountsInTheStore(t *testing.T) {
	isolate(t)
	seedAccount(t, "cl-1", "one@example.com")
	seedAccount(t, "cl-2", "two@example.com")
	seedAccount(t, "cl-3", "three@example.com")
	seedCodexAccount(t, "cx-1", "cx-one@example.com")
	seedCodexAccount(t, "cx-2", "cx-two@example.com")
	writeLiveFile(t, liveLoginJSON("RT-cl-2", ""))
	if code, _, _, top := runRoot(t, "switch", "cx-one@example.com"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}

	for _, argv := range [][]string{{"list", "--json"}, {"status", "--json"}} {
		payload := decodeJSON(t, argv...)
		if got := payload["activeUuid"]; got != "cl-2" {
			t.Fatalf("%v: activeUuid = %v, want cl-2", argv, got)
		}
		accounts, _ := payload["accounts"].([]any)
		active := 0
		for _, raw := range accounts {
			row, _ := raw.(map[string]any)
			if row["active"] == true {
				active++
				if row["uuid"] != "cl-2" {
					t.Fatalf("%v: %v is marked active", argv, row["uuid"])
				}
			}
		}
		if active != 1 {
			t.Fatalf("%v: %d rows are marked active, want exactly one", argv, active)
		}
	}
}

// which's codex object is UNCONDITIONAL, and `serving: false` is the answer on a
// machine with no pointer: a consumer asking "is codex routed" must be able to
// get a no rather than an absence it has to interpret.
func TestWhichJSONCarriesACodexObjectEitherWay(t *testing.T) {
	isolate(t)
	seedAccount(t, "cl-1", "claude@example.com")
	writeLiveFile(t, liveLoginJSON("RT-cl-1", ""))

	payload := decodeJSON(t, "which", "--json")
	codex, ok := payload["codex"].(map[string]any)
	if !ok {
		t.Fatalf("which --json has no codex object:\n%v", payload)
	}
	if codex["serving"] != false {
		t.Fatalf("codex.serving = %v, want false", codex["serving"])
	}
	if _, present := codex["account"]; present {
		t.Fatalf("codex.account is present with nothing serving:\n%v", codex)
	}
}

func TestWhichJSONNamesTheServingCodexAccount(t *testing.T) {
	isolate(t)
	seedAccount(t, "cl-1", "claude@example.com")
	writeLiveFile(t, liveLoginJSON("RT-cl-1", ""))
	seedCodexAccount(t, "cx-1", "codex@example.com")
	if code, _, _, top := runRoot(t, "switch", "codex@example.com"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}

	payload := decodeJSON(t, "which", "--json")
	codex := payload["codex"].(map[string]any)
	if codex["serving"] != true {
		t.Fatalf("codex.serving = %v, want true", codex["serving"])
	}
	account := codex["account"].(map[string]any)
	if account["uuid"] != "cx-1" {
		t.Fatalf("codex.account.uuid = %v, want cx-1", account["uuid"])
	}
	if account["provider"] != "codex" {
		t.Fatalf("codex.account.provider = %v, want codex", account["provider"])
	}
}

// Exit 5 stays Claude's question. A machine whose Claude login cannot be
// attributed answers 5 whether or not codex is served.
func TestWhichStillExitsFiveWhenTheClaudeLoginIsUnattributed(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")
	if code, _, _, top := runRoot(t, "switch", "codex@example.com"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}

	code, _, _, _ := runRoot(t, "which", "--json")
	if code != ExitProbeNegative {
		t.Fatalf("exit = %d, want %d", code, ExitProbeNegative)
	}
}

// The placement test. codexObject is built OUTSIDE `if res.OK` in which.go, on
// purpose: a consumer asking "is codex routed" needs a false or an account
// whether or not Claude Code's own login can be attributed. Every test above
// that reaches a res.OK == false payload discards stdout, so a codexObject
// nested one level inside `if res.OK` -- a natural slip, since it sits right
// after `payload["account"] = ...` -- would leave this file green while
// silently dropping the codex object on every unattributed machine. This is
// the one test that reads the payload on that path.
func TestWhichJSONCarriesTheCodexObjectWhenClaudeIsUnattributed(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")
	if code, _, _, top := runRoot(t, "switch", "codex@example.com"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}

	code, stdout, _, _ := runRoot(t, "which", "--json")
	if code != ExitProbeNegative {
		t.Fatalf("exit = %d, want %d", code, ExitProbeNegative)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("which --json produced no document: %v\n%s", err, stdout)
	}
	if attributed, present := payload["attributed"]; !present || attributed != false {
		t.Fatalf("attributed = %v, want false (this test needs res.OK == false to be meaningful)", attributed)
	}
	codex, ok := payload["codex"].(map[string]any)
	if !ok {
		t.Fatalf("which --json has no codex object with an unattributed Claude login:\n%v", payload)
	}
	if codex["serving"] != true {
		t.Fatalf("codex.serving = %v, want true", codex["serving"])
	}
	account, ok := codex["account"].(map[string]any)
	if !ok {
		t.Fatalf("codex.account is missing with an unattributed Claude login:\n%v", codex)
	}
	if account["uuid"] != "cx-1" {
		t.Fatalf("codex.account.uuid = %v, want cx-1", account["uuid"])
	}
}

// decodeJSON runs a --json command and returns its one document.
func decodeJSON(t *testing.T, argv ...string) map[string]any {
	t.Helper()
	_, stdout, _, _ := runRoot(t, argv...)
	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("%v produced no document: %v\n%s", argv, err, stdout)
	}
	return payload
}

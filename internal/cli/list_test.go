package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// An empty store is exit 0: "no accounts yet" is a fact, not a failure, and the
// notice is a notice, so it belongs on stderr.
func TestListEmptyStoreExitsZeroWithTheNoticeOnStderr(t *testing.T) {
	isolate(t)

	code, out, errOut, top := runRoot(t, "list")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 for an empty store (%s)", code, top)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty: 'no accounts yet' is a notice, not data", out)
	}
	if !strings.Contains(errOut, "No accounts yet") {
		t.Fatalf("stderr = %q, want the notice", errOut)
	}
}

func TestListEmptyStoreJSONIsOneDocument(t *testing.T) {
	isolate(t)

	code, out, errOut, _ := runRoot(t, "list", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if errOut != "" {
		t.Fatalf("stderr = %q, want empty", errOut)
	}
	var payload struct {
		SchemaVersion int              `json:"schemaVersion"`
		Accounts      []map[string]any `json:"accounts"`
	}
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&payload); err != nil {
		t.Fatalf("--json is not valid JSON: %v\n%s", err, out)
	}
	if dec.More() {
		t.Fatal("--json emitted more than one document on stdout")
	}
	if payload.SchemaVersion != 1 || payload.Accounts == nil || len(payload.Accounts) != 0 {
		t.Fatalf("payload = %+v, want schemaVersion 1 and an empty (not null) accounts array", payload)
	}
}

func TestListJSONCarriesSchemaVersionAndUUIDs(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")

	code, out, _, top := runRoot(t, "list", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s), want 0", code, top)
	}
	var payload struct {
		SchemaVersion int `json:"schemaVersion"`
		Accounts      []struct {
			UUID  string `json:"uuid"`
			Idx   int    `json:"idx"`
			Email string `json:"email"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out)
	}
	if payload.SchemaVersion != 1 {
		t.Fatalf("schemaVersion = %d, want 1", payload.SchemaVersion)
	}
	if len(payload.Accounts) != 1 || payload.Accounts[0].UUID != "u-1" {
		t.Fatalf("accounts = %+v", payload.Accounts)
	}
}

// The human table goes to stdout; nothing else may, or `ccdad list --json`
// would emit two documents.
func TestListHumanOutputMentionsTheAccount(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")

	code, out, _, top := runRoot(t, "list")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s), want 0", code, top)
	}
	if !strings.Contains(out, "a@example.com") {
		t.Fatalf("list output does not mention the account:\n%s", out)
	}
}

// A store whose accounts are all disabled must say so, not print a bare header
// row with nothing under it.
func TestListWithEveryAccountDisabledSaysSo(t *testing.T) {
	isolate(t)
	seedDisabledAccount(t, "u-1", "a@example.com")

	code, out, errOut, top := runRoot(t, "list")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s), want 0", code, top)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want no table at all", out)
	}
	if !strings.Contains(errOut, "--all") {
		t.Fatalf("stderr = %q, want it to point at --all", errOut)
	}
	if _, out, _, _ := runRoot(t, "list", "--all"); !strings.Contains(out, "a@example.com") {
		t.Fatalf("list --all does not show the disabled account:\n%s", out)
	}
}

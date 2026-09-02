package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// `active` and `activeUuid` are Claude's, and they stay Claude's.
//
// They answer one question -- which account Claude Code is logged in as right
// now -- and there is no second login for them to be ambiguous about. A Codex
// account is SERVED by the proxy from its next new thread, which is a
// different fact with a different key.
//
// The fixture is deliberately the confusing one: five accounts, a Claude login
// live, and a Codex serving pointer naming one of the two Codex accounts. A
// build that let the pointer reach these keys would report two active accounts
// or name the Codex one, and both readings are what a consumer would then
// switch on.
func TestActiveAndActiveUUIDNameOnlyTheClaudeLogin(t *testing.T) {
	isolate(t)
	root := mustPath(ccpath.StoreHome())
	seedAccount(t, "u-c1", "one@example.com")
	seedAccount(t, "u-c2", "two@example.com")
	seedAccount(t, "u-c3", "three@example.com")
	seedCodexAccount(t, "u-x1", "x-one@example.com")
	seedCodexAccount(t, "u-x2", "x-two@example.com")
	writeLiveFile(t, liveLoginJSON("RT-u-c2", ""))

	// The serving pointer is just a file here. Nothing in this part reads it;
	// writing it is what makes the fixture the one a later part could break.
	if err := os.MkdirAll(filepath.Join(root, "codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "codex", "serving"), []byte("u-x1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, out, _, top := runRoot(t, "list", "--json")
	var payload struct {
		ActiveUUID string `json:"activeUuid"`
		Accounts   []struct {
			UUID     string `json:"uuid"`
			Provider string `json:"provider"`
			Active   bool   `json:"active"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("--json output is not valid JSON: %v (%s)\n%s", err, top, out)
	}
	if len(payload.Accounts) != 5 {
		t.Fatalf("payload carries %d accounts, want 5:\n%s", len(payload.Accounts), out)
	}

	var active []string
	for _, a := range payload.Accounts {
		if a.Active {
			active = append(active, a.UUID)
		}
		if a.Provider == "codex" && a.Active {
			t.Errorf("%s is a Codex account and is reported active", a.UUID)
		}
	}
	if len(active) != 1 {
		t.Fatalf("accounts reported active = %v, want exactly one", active)
	}
	if active[0] != "u-c2" {
		t.Errorf("the active account is %s, want the Claude login u-c2", active[0])
	}
	if payload.ActiveUUID != "u-c2" {
		t.Errorf("activeUuid = %q, want the Claude login u-c2", payload.ActiveUUID)
	}
}

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

	_, out, _, top := runRoot(t, "status", "--json")
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

// The Claude route through `ccdad run` ends in exec'ing Claude Code against a
// credentials file the command just wrote, and a Codex account's blob carries
// no TokenRecord -- so refuseUnscopedRun and refuseDisplacedAuth both read it
// as an OAuth login and let it through. Left to those two, the launch would
// write a Codex refresh token into a session directory in Claude Code's own
// credentials format. That directory is scoped to the session rather than the
// machine's own login, so it is not a cross into the live login, but it is
// still a Codex secret sitting in a shape and a place Claude Code owns and
// rewrites.
//
// What keeps it out of there is now the provider branch rather than a refusal:
// a Codex account starts codex through ccdad's proxy instead, and the branch
// has to sit before the blob is even read. So the disk assertion below is the
// shape TestRunRefusesBeforeItCreatesASessionDirectory pins for every run
// refusal -- no sessions/ container at all, and therefore no credentials file
// inside one -- and the launch assertions are what tell that apart from a
// command that simply did nothing.
func TestRunOnACodexAccountNeverWritesClaudeCodeCredentials(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "u-x1", "x-one@example.com")
	stub, _ := routedWorld(t, ExitOK, nil)

	code, _, errOut, top := runRoot(t, "run", "1")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if !stub.started {
		t.Fatal("nothing was started at all")
	}
	// routedWorld stubs BOTH resolvers, so the name of the child is what says
	// which route the command took: codex from resolveCodex, claude from
	// stubLookClaude.
	if got := filepath.Base(stub.spec.Path); got != "codex" {
		t.Errorf("`ccdad run` on a Codex account started %q, want codex", stub.spec.Path)
	}
	// A scoped Claude session is exactly what must not have happened. The
	// Claude route sets this variable to the session credential home it made,
	// and routedWorld leaves it unset -- so a value here is one this command
	// put there.
	for _, kv := range stub.spec.Env {
		if strings.HasPrefix(kv, "CLAUDE_SECURESTORAGE_CONFIG_DIR=") {
			t.Errorf("the codex child was handed a Claude credential home: %q", kv)
		}
	}
	container := filepath.Join(mustPath(ccpath.StoreHome()), SessionsDirName)
	if _, err := os.Stat(container); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Stat(%s) = %v, want no session container at all", container, err)
	}
}

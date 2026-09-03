package cli

import (
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/store"
)

// Removing the account codex is served from has to clear the pointer, or the
// pointer names an account nothing can find and the proxy falls through to the
// top-ranked one -- silently, and billed somewhere the user did not choose.
func TestRemovingTheServingCodexAccountClearsThePointer(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")
	seedCodexAccount(t, "cx-2", "other@example.com")
	if code, _, _, top := runRoot(t, "switch", "codex@example.com"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}

	code, _, stderr, top := runRoot(t, "remove", "codex@example.com", "--yes")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, top, stderr)
	}
	if got := servingNow(t); got != "" {
		t.Fatalf("serving = %q after removing that account, want no pointer", got)
	}
	if !strings.Contains(stderr, "codex") {
		t.Fatalf("remove did not say the codex pointer was cleared:\n%s", stderr)
	}
}

// Removing a Codex account that is NOT serving leaves the pointer alone.
func TestRemovingAnotherCodexAccountLeavesThePointer(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")
	seedCodexAccount(t, "cx-2", "other@example.com")
	if code, _, _, top := runRoot(t, "switch", "codex@example.com"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}

	if code, _, _, top := runRoot(t, "remove", "other@example.com", "--yes"); code != ExitOK {
		t.Fatalf("exit = %d (%s)", code, top)
	}
	if got := servingNow(t); got != "cx-1" {
		t.Fatalf("serving = %q, want cx-1 left alone", got)
	}
}

// Disabling is a ROTATION policy and not a per-request gate: the account goes
// on serving until the lane rotates away from it, which is the same rule the
// Claude side states about the live login.
func TestDisablingTheServingCodexAccountSaysItKeepsServing(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")
	if code, _, _, top := runRoot(t, "switch", "codex@example.com"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}

	code, _, stderr, _ := runRoot(t, "disable", "codex@example.com")
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "keeps serving") ||
		!strings.Contains(stderr, "holds it out of") {
		t.Fatalf("disable did not say the account keeps serving:\n%s", stderr)
	}
	if got := servingNow(t); got != "cx-1" {
		t.Fatalf("serving = %q; disabling must not move the pointer", got)
	}
}

// primary switches a credit ceiling off, and there is no credit axis for codex
// in this version. Storing the flag as inert would leave a setting that reads
// like it does something.
func TestPrimaryRefusesACodexAccount(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")

	code, _, stderr, _ := runRoot(t, "primary", "codex@example.com", "on")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)\n%s", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "codex") {
		t.Fatalf("the refusal does not say why:\n%s", stderr)
	}
}

// `ccdad auto` is Claude-only in this version, and its own help is the one
// place a user reads what it covers.
func TestAutoHelpSaysItIsClaudeOnly(t *testing.T) {
	isolate(t)

	_, stdout, _, _ := runRoot(t, "auto", "--help")
	if !strings.Contains(stdout, "codex") {
		t.Fatalf("auto's help does not say it leaves codex alone:\n%s", stdout)
	}
}

// `alias` names a codex account exactly as it names a Claude one, and it does so
// BY CONSTRUCTION: the command resolves its argument through store.Resolve,
// which ranges over every account whatever its provider, and it writes a field
// that has no provider half to it. Nothing had to be taught this, and that is
// precisely why it is worth pinning -- a provider filter added to the resolve
// path later would take it away with no other test in the tree going red.
func TestAliasWorksOnACodexAccount(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")

	code, _, stderr, top := runRoot(t, "alias", "codex@example.com", "cx")
	if code != ExitOK {
		t.Fatalf("alias = %d (%s), want 0\n%s", code, top, stderr)
	}

	// Read back through a FRESH store rather than off the command's own handle:
	// the assertion is that the alias reached the document on disk, not that a
	// live struct in the process was updated.
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get("cx-1")
	if !ok {
		t.Fatal("the codex account is not in the store after alias")
	}
	if got.Alias != "cx" {
		t.Fatalf("stored alias = %q, want %q", got.Alias, "cx")
	}
	// And the handle resolves, which is the half a user actually spends.
	if _, err := store.Resolve(s.Accounts(), "cx"); err != nil {
		t.Errorf("the stored alias does not resolve: %v", err)
	}
}

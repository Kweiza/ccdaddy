package cli

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/codexswitch"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// Removing the account codex is served from has to clear the pointer, or the
// pointer names an account nothing can find and the proxy falls through to the
// top-ranked one -- silently, and billed somewhere the user did not choose.
//
// Seeded as "cx@example.com" rather than "codex@example.com" on purpose: an
// email containing the literal word "codex" would satisfy a Contains(stderr,
// "codex") check off target.Label() alone, with the actual Note text -- or the
// Clear call behind it -- deleted. The assertion below checks a phrase that
// only the Note itself carries.
func TestRemovingTheServingCodexAccountClearsThePointer(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "cx@example.com")
	seedCodexAccount(t, "cx-2", "other@example.com")
	if code, _, _, top := runRoot(t, "switch", "cx@example.com"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}

	code, _, stderr, top := runRoot(t, "remove", "cx@example.com", "--yes")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, top, stderr)
	}
	if got := servingNow(t); got != "" {
		t.Fatalf("serving = %q after removing that account, want no pointer", got)
	}
	if !strings.Contains(stderr, "nothing is serving codex now") {
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

// The confirmation prompt blocks on stdin, and nothing serialises this command
// against the daemon's own tick while a human has not yet answered it. This
// replays that exact sequence: cx-1 is serving when `remove cx-1` snapshots
// who is serving, the "daemon" repoints codex to cx-2 while the prompt is
// still waiting, and only THEN does the human say yes. cx-2's pointer -- a
// legitimate, concurrently-written one -- must survive.
func TestRemoveDuringThePromptDoesNotClearAPointerThatMovedMeanwhile(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	seedCodexAccount(t, "cx-1", "cx-1@example.com")
	seedCodexAccount(t, "cx-2", "cx-2@example.com")
	if code, _, _, top := runRoot(t, "switch", "cx-1@example.com"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}

	root, err := codexRoot()
	if err != nil {
		t.Fatal(err)
	}

	cmd := newRemoveCmd()
	cmd.SetArgs(explicitArgs([]string{"cx-1@example.com"}))
	stdinR, stdinW := io.Pipe()
	cmd.SetIn(stdinR)
	stderr := &signalOnFirstWrite{ready: make(chan struct{})}
	cmd.SetErr(stderr)
	cmd.SetOut(io.Discard)

	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

	// Wait for the confirmation prompt to actually be printed -- the moment
	// right before the command blocks reading stdin -- rather than sleeping
	// and hoping the goroutine got there first.
	select {
	case <-stderr.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("remove never printed its confirmation prompt")
	}

	// The daemon's own tick, simulated directly: it repoints codex to cx-2
	// while the human at the prompt has not yet answered.
	if err := codexswitch.Execute(root, "cx-2"); err != nil {
		t.Fatal(err)
	}

	if _, err := stdinW.Write([]byte("y\n")); err != nil {
		t.Fatal(err)
	}
	_ = stdinW.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("remove returned %v\n%s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("remove did not finish after being answered")
	}

	if got := servingNow(t); got != "cx-2" {
		t.Fatalf("serving = %q, want cx-2's concurrently-written pointer left alone", got)
	}
}

// The opposite direction of the race above: codex is repointed TO the account
// being removed WHILE the prompt blocks. remove's own snapshot, taken before
// the prompt, said cx-2 was not serving -- so a fix that only re-read the
// pointer inside an `if wasServing` block never runs at all here, cx-2 is
// deleted, and the pointer is left naming a uuid nothing can resolve.
func TestRemoveClearsAPointerThatMovedOntoTheTargetDuringThePrompt(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	seedCodexAccount(t, "cx-1", "cx-1@example.com")
	seedCodexAccount(t, "cx-2", "cx-2@example.com")
	if code, _, _, top := runRoot(t, "switch", "cx-1@example.com"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}

	root, err := codexRoot()
	if err != nil {
		t.Fatal(err)
	}

	cmd := newRemoveCmd()
	cmd.SetArgs(explicitArgs([]string{"cx-2@example.com"}))
	stdinR, stdinW := io.Pipe()
	cmd.SetIn(stdinR)
	stderr := &signalOnFirstWrite{ready: make(chan struct{})}
	cmd.SetErr(stderr)
	cmd.SetOut(io.Discard)

	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

	select {
	case <-stderr.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("remove never printed its confirmation prompt")
	}

	// The daemon's own tick, simulated directly: it repoints codex TO cx-2 --
	// the account about to be removed -- while the human at the prompt has not
	// yet answered. remove's pre-prompt snapshot said cx-2 was not serving.
	if err := codexswitch.Execute(root, "cx-2"); err != nil {
		t.Fatal(err)
	}

	if _, err := stdinW.Write([]byte("y\n")); err != nil {
		t.Fatal(err)
	}
	_ = stdinW.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("remove returned %v\n%s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("remove did not finish after being answered")
	}

	if got := servingNow(t); got != "" {
		t.Fatalf("serving = %q after removing cx-2, want no pointer -- cx-2 no longer exists to be named", got)
	}
	if !strings.Contains(stderr.String(), "nothing is serving codex now") {
		t.Fatalf("remove did not say the codex pointer was cleared:\n%s", stderr.String())
	}
}

// signalOnFirstWrite closes ready the first time anything is written to it.
// remove's confirmation prompt is the only write to stderr before the command
// blocks on stdin, so this is how the test above knows precisely when the
// command has reached that block, without a sleep standing in for the fact.
type signalOnFirstWrite struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func (w *signalOnFirstWrite) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.buf.Write(p)
	w.mu.Unlock()
	w.once.Do(func() { close(w.ready) })
	return n, err
}

func (w *signalOnFirstWrite) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
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

// countEnabled used to count across both providers, so disabling the last
// enabled CLAUDE account while a codex account was still enabled looked, from
// the note's perspective, like there was still something to rotate to: the
// shared count read nonzero. The two rotations are already provider-filtered,
// so the count has to be too.
func TestDisablingTheLastEnabledClaudeAccountSaysSoWithACodexAccountStillEnabled(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	seedCodexAccount(t, "cx-1", "cx@example.com")

	code, _, stderr, _ := runRoot(t, "disable", "one@example.com")
	if code != ExitOK {
		t.Fatalf("disable = %d\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "nothing to rotate to") {
		t.Fatalf("disabling the last enabled Claude account, with a codex account still enabled, "+
			"did not say the Claude lane has nothing left:\n%s", stderr)
	}
}

// The symmetric direction: disabling the only enabled codex account, with a
// Claude account still enabled, must name the codex lane specifically rather
// than staying silent because the shared count was nonzero.
func TestDisablingTheLastEnabledCodexAccountSaysSoWithAClaudeAccountStillEnabled(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	seedCodexAccount(t, "cx-1", "cx@example.com")

	code, _, stderr, _ := runRoot(t, "disable", "cx@example.com")
	if code != ExitOK {
		t.Fatalf("disable = %d\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "codex") || !strings.Contains(stderr, "nothing to rotate to") {
		t.Fatalf("disabling the last enabled codex account, with a Claude account still enabled, "+
			"did not name the codex lane:\n%s", stderr)
	}
}

// primary switches a credit ceiling off, and there is no credit axis for codex
// in this version. Storing the flag as inert would leave a setting that reads
// like it does something.
//
// Seeded as "cx@example.com" for the same reason the remove test above is:
// "codex" in the email would satisfy Contains(stderr, "codex") off
// target.Label() alone, with the actual credit-axis explanation stripped out.
// The assertion checks a phrase only that explanation carries, and the store
// is read back afterward because store.SetPrimary has no provider guard of
// its own -- this command's early return is the ONLY thing enforcing it.
func TestPrimaryRefusesACodexAccount(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "cx@example.com")

	code, _, stderr, _ := runRoot(t, "primary", "cx@example.com", "on")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)\n%s", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "no credit axis") {
		t.Fatalf("the refusal does not say why:\n%s", stderr)
	}

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get("cx-1")
	if !ok {
		t.Fatal("the codex account is not in the store after the refusal")
	}
	if got.Primary {
		t.Fatal("primary was refused but still set the flag in the store")
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

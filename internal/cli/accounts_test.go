package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/Kweiza/ccdaddy/internal/store"
)

func TestDisableHidesFromListButNotFromAll(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	seedAccount(t, "u-2", "two@example.com")

	code, _, stderr, _ := runRoot(t, "disable", "2")
	if code != ExitOK {
		t.Fatalf("disable = %d (%s), want 0", code, stderr)
	}
	if !strings.Contains(stderr, "now disabled") {
		t.Errorf("disable said %q, want it to say the account is disabled", stderr)
	}

	_, stdout, _, _ := runRoot(t, "list")
	if strings.Contains(stdout, "two@example.com") {
		t.Errorf("list still shows the disabled account:\n%s", stdout)
	}
	_, all, _, _ := runRoot(t, "list", "--all")
	if !strings.Contains(all, "two@example.com") {
		t.Errorf("list --all does not show the disabled account:\n%s", all)
	}
}

func TestDisableTwiceIsNothingToDo(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")

	if code, _, stderr, _ := runRoot(t, "disable", "1"); code != ExitOK {
		t.Fatalf("first disable = %d (%s), want 0", code, stderr)
	}
	code, _, stderr, top := runRoot(t, "disable", "1")
	if code != ExitNothingToDo {
		t.Fatalf("second disable = %d, want %d", code, ExitNothingToDo)
	}
	if !strings.Contains(stderr, "already disabled") {
		t.Errorf("stderr = %q, want it to say the account is already disabled", stderr)
	}
	if top != "" {
		t.Errorf("ExecuteWith printed %q on top of the command's own notice", top)
	}
}

func TestEnableReturnsAnAccountToRotation(t *testing.T) {
	isolate(t)
	seedDisabledAccount(t, "u-1", "one@example.com")

	code, _, stderr, _ := runRoot(t, "enable", "u-1xxxxxx")
	if code != ExitUsage {
		t.Fatalf("enable with a bad reference = %d, want %d", code, ExitUsage)
	}

	if code, _, stderr, _ = runRoot(t, "enable", "1"); code != ExitOK {
		t.Fatalf("enable = %d (%s), want 0", code, stderr)
	}
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("u-1")
	if got.Disabled {
		t.Error("the account is still disabled after enable")
	}
	if code, _, _, _ := runRoot(t, "enable", "1"); code != ExitNothingToDo {
		t.Errorf("enabling an enabled account = %d, want %d", code, ExitNothingToDo)
	}
}

// Disabling every account is a completed action, not §9.3's 4: nothing was
// blocked. The engine reports having nothing to rotate to when it next looks.
func TestDisablingTheLastAccountIsStillSuccess(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")

	code, _, stderr, _ := runRoot(t, "disable", "1")
	if code != ExitOK {
		t.Fatalf("disable = %d, want 0", code)
	}
	if !strings.Contains(stderr, "every account is now disabled") &&
		!strings.Contains(stderr, "Every account is now disabled") {
		t.Errorf("stderr = %q, want a note that nothing is left to rotate to", stderr)
	}
}

// Disable is a policy for the auto engine, not a lock. An explicit switch to a
// disabled account still works, because naming one by hand is a clearer
// statement of intent than the flag is.
func TestSwitchStillWorksOnADisabledAccount(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	seedDisabledAccount(t, "u-2", "two@example.com")

	code, _, stderr, _ := runRoot(t, "switch", "2")
	if code != ExitOK {
		t.Fatalf("switch to a disabled account = %d (%s), want 0", code, stderr)
	}
	_, stdout, _, _ := runRoot(t, "which", "--json")
	if !strings.Contains(stdout, "u-2") {
		t.Errorf("which does not report the disabled account as live:\n%s", stdout)
	}
}

func TestDisablingTheLiveLoginSaysSo(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	seedAccount(t, "u-2", "two@example.com")
	if code, _, stderr, _ := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("switch = %d (%s), want 0", code, stderr)
	}

	_, _, stderr, _ := runRoot(t, "disable", "1")
	if !strings.Contains(stderr, "live Claude Code login") {
		t.Errorf("stderr = %q, want a note that the disabled account is the live login", stderr)
	}
}

func TestAliasSetsTheNormalizedForm(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")

	code, _, stderr, _ := runRoot(t, "alias", "1", "  Work  ")
	if code != ExitOK {
		t.Fatalf("alias = %d (%s), want 0", code, stderr)
	}
	if !strings.Contains(stderr, `"work"`) {
		t.Errorf("stderr = %q, want the NORMALIZED alias echoed back", stderr)
	}

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("u-1")
	if got.Alias != "work" {
		t.Fatalf("stored alias = %q, want %q", got.Alias, "work")
	}
	// The normalized form is what every later reference has to use, which is
	// the reason the echo-back must not show the typed one.
	if code, _, _, _ := runRoot(t, "which", "--json"); code != ExitOK && code != ExitProbeNegative {
		t.Fatalf("which = %d", code)
	}
	if _, err := store.Resolve(s.Accounts(), "work"); err != nil {
		t.Errorf("the stored alias does not resolve: %v", err)
	}
}

func TestAliasCollisionIsAUsageErrorNamingTheOtherAccount(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	seedAccount(t, "u-2", "two@example.com")
	if code, _, _, _ := runRoot(t, "alias", "1", "work"); code != ExitOK {
		t.Fatal("setting the first alias failed")
	}

	code, _, _, top := runRoot(t, "alias", "2", "WORK")
	if code != ExitUsage {
		t.Fatalf("alias collision = %d, want %d", code, ExitUsage)
	}
	// Label() prefers the alias, so the holder of "work" is named "work" — the
	// uuid is what makes that unambiguous, and it is the half an idx could not
	// provide because idx recompacts on every removal.
	if !strings.Contains(top, "u-1") {
		t.Errorf("the collision message %q does not name the other account by uuid", top)
	}
	if strings.Contains(top, "idx") {
		t.Errorf("the collision message names an idx, which recompacts: %q", top)
	}
}

func TestAliasRejectsAnEmptyArgumentRatherThanClearing(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	if code, _, _, _ := runRoot(t, "alias", "1", "work"); code != ExitOK {
		t.Fatal("setup failed")
	}

	code, _, _, top := runRoot(t, "alias", "1", "")
	if code != ExitUsage {
		t.Fatalf("alias with an empty argument = %d, want %d — a dropped shell word must not clear an alias", code, ExitUsage)
	}
	if !strings.Contains(top, "--clear") {
		t.Errorf("the refusal %q does not point at --clear", top)
	}

	s, _ := store.Open()
	if got, _ := s.Get("u-1"); got.Alias != "work" {
		t.Errorf("the alias was cleared anyway: %q", got.Alias)
	}
}

func TestAliasClear(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	if code, _, _, _ := runRoot(t, "alias", "1", "work"); code != ExitOK {
		t.Fatal("setup failed")
	}

	if code, _, _, top := runRoot(t, "alias", "1", "work", "--clear"); code != ExitUsage {
		t.Errorf("--clear with an alias argument = %d (%s), want %d", code, top, ExitUsage)
	}

	code, _, stderr, _ := runRoot(t, "alias", "1", "--clear")
	if code != ExitOK {
		t.Fatalf("alias --clear = %d (%s), want 0", code, stderr)
	}
	s, _ := store.Open()
	if got, _ := s.Get("u-1"); got.Alias != "" {
		t.Fatalf("alias = %q after --clear, want empty", got.Alias)
	}

	if code, _, _, _ := runRoot(t, "alias", "1", "--clear"); code != ExitNothingToDo {
		t.Errorf("clearing an absent alias = %d, want %d", code, ExitNothingToDo)
	}
	if code, _, _, _ := runRoot(t, "alias", "1"); code != ExitUsage {
		t.Errorf("alias with no alias and no --clear = %d, want %d", code, ExitUsage)
	}
}

// §5.1 forbids a leading '-' so an alias can never be read as a flag. pflag
// parses before Args runs, so without this the user is told about a shorthand
// flag they never typed.
func TestAliasLeadingHyphenNamesTheRule(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")

	code, _, _, top := runRoot(t, "alias", "1", "-work")
	if code != ExitUsage {
		t.Fatalf("alias -work = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(top, "may not start with '-'") {
		t.Errorf("the error %q does not name the rule", top)
	}
}

// The move trap: sortAndReindex runs on every Open and sorts by the STORED idx,
// so a move that only assigns a new number is undone the next time the store is
// read — or worse, leaves two accounts sharing an idx, where SliceStable breaks
// the tie by slice position.
func TestMoveSurvivesTheReindexOnReopen(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	seedAccount(t, "u-2", "two@example.com")
	seedAccount(t, "u-3", "three@example.com")

	code, _, stderr, _ := runRoot(t, "move", "3", "1")
	if code != ExitOK {
		t.Fatalf("move = %d (%s), want 0", code, stderr)
	}

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	seen := map[int]bool{}
	for _, a := range s.Accounts() {
		order = append(order, a.UUID)
		if seen[a.Idx] {
			t.Fatalf("two accounts share idx %d after a move: %+v", a.Idx, s.Accounts())
		}
		seen[a.Idx] = true
	}
	want := []string{"u-3", "u-1", "u-2"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order after move = %v, want %v", order, want)
	}
	for i, a := range s.Accounts() {
		if a.Idx != i+1 {
			t.Errorf("account %s has idx %d at position %d; the indices are not contiguous", a.UUID, a.Idx, i+1)
		}
	}
}

func TestMovePastTheEndClampsRatherThanFailing(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	seedAccount(t, "u-2", "two@example.com")

	code, _, stderr, _ := runRoot(t, "move", "1", "99")
	if code != ExitOK {
		t.Fatalf("move past the end = %d (%s), want 0 with a clamp", code, stderr)
	}
	// Echoing "moved to 99" would be a lie; the landed position is read back.
	if !strings.Contains(stderr, "now at 2") {
		t.Errorf("stderr = %q, want the position it actually landed at", stderr)
	}
	s, _ := store.Open()
	if got, _ := s.Get("u-1"); got.Idx != 2 {
		t.Errorf("idx = %d, want 2", got.Idx)
	}
}

func TestMoveToItsOwnPositionIsNothingToDo(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	seedAccount(t, "u-2", "two@example.com")

	code, _, stderr, _ := runRoot(t, "move", "2", "2")
	if code != ExitNothingToDo {
		t.Fatalf("move to the same position = %d (%s), want %d", code, stderr, ExitNothingToDo)
	}
}

func TestMoveRejectsANonPosition(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")

	for _, bad := range []string{"0", "first", "1.5"} {
		code, _, _, top := runRoot(t, "move", "1", bad)
		if code != ExitUsage {
			t.Errorf("move 1 %q = %d, want %d", bad, code, ExitUsage)
		}
		if !strings.Contains(top, "whole number from 1 up") {
			t.Errorf("move 1 %q said %q, want it to say what a position is", bad, top)
		}
	}
	if code, _, _, _ := runRoot(t, "move", "nope", "1"); code != ExitUsage {
		t.Errorf("move with an unknown account = %d, want %d", code, ExitUsage)
	}
}

// A CLI command that cannot get the store lock waits a bounded time and then
// exits 1 naming the daemon — it does not block forever, and it does not write
// unguarded.
func TestAWriteBlockedOnTheStoreLockExitsOne(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")

	saved := store.LockTimeout
	store.LockTimeout = 150 * time.Millisecond
	t.Cleanup(func() { store.LockTimeout = saved })

	// Another process's hold. gofrs is used directly rather than through the
	// store so this is a second open file description, which is what makes
	// flock(2) exclude it.
	held := flock.New(store.LockPath())
	locked, err := held.TryLock()
	if err != nil || !locked {
		t.Fatalf("TryLock() = %v, %v; want the lock", locked, err)
	}
	t.Cleanup(func() { _ = held.Unlock() })

	started := time.Now()
	code, _, _, top := runRoot(t, "disable", "1")
	elapsed := time.Since(started)

	if code != ExitFailure {
		t.Fatalf("a write behind a held store lock = %d, want %d", code, ExitFailure)
	}
	if elapsed < store.LockTimeout {
		t.Errorf("gave up after %s, want at least the %s wait", elapsed, store.LockTimeout)
	}
	if !strings.Contains(top, "daemon") {
		t.Errorf("the error %q does not name the daemon", top)
	}

	// And the account is untouched: a refusal is not a partial write.
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("u-1"); got.Disabled {
		t.Error("the account was disabled despite the lock being held elsewhere")
	}
}

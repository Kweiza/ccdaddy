package cli

import (
	"strings"
	"testing"
)

func TestOwnDeclaresTheSplitAndNamesBothSides(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	seedAccount(t, "u-2", "two@example.com")
	seedAccount(t, "u-3", "three@example.com")

	code, _, stderr, _ := runRoot(t, "own", "1", "2")
	if code != ExitOK {
		t.Fatalf("own = %d (%s), want 0", code, stderr)
	}
	if !strings.Contains(stderr, "one@example.com") || !strings.Contains(stderr, "two@example.com") {
		t.Errorf("own did not name what this machine drives:\n%s", stderr)
	}
	if !strings.Contains(stderr, "Another machine drives") ||
		!strings.Contains(stderr, "three@example.com") {
		t.Errorf("own did not name what it handed away:\n%s", stderr)
	}
	// The half this machine cannot check is the half worth saying out loud.
	if !strings.Contains(stderr, "other machines") {
		t.Errorf("own did not say the other machines need the same command:\n%s", stderr)
	}
}

// No arguments is a QUESTION. There is no spelling of this command that owns
// nothing, because owning nothing parks the engine with no target at all.
func TestOwnWithNoArgumentsReportsRatherThanClears(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	seedAccount(t, "u-2", "two@example.com")

	if code, _, stderr, _ := runRoot(t, "own", "1"); code != ExitOK {
		t.Fatalf("own = %d (%s), want 0", code, stderr)
	}
	code, _, stderr, _ := runRoot(t, "own")
	if code != ExitOK {
		t.Fatalf("bare own = %d (%s), want 0", code, stderr)
	}
	if !strings.Contains(stderr, "This machine drives") {
		t.Errorf("bare own did not report the split:\n%s", stderr)
	}
	// And it did not clear it.
	if !strings.Contains(stderr, "Another machine drives") {
		t.Errorf("bare own cleared the split instead of reporting it:\n%s", stderr)
	}
}

func TestOwnClearGivesEveryAccountBack(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	seedAccount(t, "u-2", "two@example.com")

	if code, _, stderr, _ := runRoot(t, "own", "1"); code != ExitOK {
		t.Fatalf("own = %d (%s), want 0", code, stderr)
	}
	code, _, stderr, _ := runRoot(t, "own", "--clear")
	if code != ExitOK {
		t.Fatalf("own --clear = %d (%s), want 0", code, stderr)
	}
	if !strings.Contains(stderr, "no split") {
		t.Errorf("own --clear did not report the split gone:\n%s", stderr)
	}
}

func TestOwnTwiceIsNothingToDo(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	seedAccount(t, "u-2", "two@example.com")

	if code, _, stderr, _ := runRoot(t, "own", "1"); code != ExitOK {
		t.Fatalf("first own = %d (%s), want 0", code, stderr)
	}
	code, _, stderr, top := runRoot(t, "own", "1")
	if code != ExitNothingToDo {
		t.Fatalf("second own = %d, want %d", code, ExitNothingToDo)
	}
	if !strings.Contains(stderr, "already the split") {
		t.Errorf("stderr = %q, want it to say the split is already this", stderr)
	}
	if top != "" {
		t.Errorf("ExecuteWith printed %q on top of the command's own notice", top)
	}
}

// Nothing is written unless every account resolves, so a typo in the middle of a
// list cannot leave the machine owning a set nobody asked for.
func TestOwnRefusesTheWholeListWhenOneAccountIsUnknown(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	seedAccount(t, "u-2", "two@example.com")

	code, _, _, _ := runRoot(t, "own", "1", "nope@example.com")
	if code != ExitUsage {
		t.Fatalf("own with an unknown account = %d, want %d", code, ExitUsage)
	}
	_, _, stderr, _ := runRoot(t, "own")
	if !strings.Contains(stderr, "no split") {
		t.Errorf("a refused own still partitioned the machine:\n%s", stderr)
	}
}

func TestOwnRefusesClearTogetherWithAList(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")

	code, _, _, _ := runRoot(t, "own", "--clear", "1")
	if code != ExitUsage {
		t.Fatalf("own --clear with a list = %d, want %d", code, ExitUsage)
	}
}

// A listing is where a reader asks why an account never gets chosen, so the
// reason has to be beside the row rather than only in `ccdad own`.
func TestListMarksAnAccountAnotherMachineDrives(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	seedAccount(t, "u-2", "two@example.com")

	if code, _, stderr, _ := runRoot(t, "own", "1"); code != ExitOK {
		t.Fatalf("own = %d (%s), want 0", code, stderr)
	}
	_, stdout, _, _ := runRoot(t, "list")
	if !strings.Contains(stdout, "another machine") {
		t.Errorf("list does not say why two@example.com is never chosen:\n%s", stdout)
	}
	// And it is still LISTED: an account this machine does not drive is not
	// hidden the way a disabled one is, because it is still a real account and
	// `ccdad switch` still names it.
	if !strings.Contains(stdout, "two@example.com") {
		t.Errorf("list hid an account another machine drives:\n%s", stdout)
	}
}

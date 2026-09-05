package tui

import (
	"strings"
	"testing"
)

// `s` switches to the account under the cursor, in one keystroke.
//
// The list it replaces was a second rendering of the list the reader was already
// looking at, opened on the row they were already pointing at, and enter on it
// chose what had already been chosen.
func TestTheSwitchKeyMovesToTheAccountUnderTheCursor(t *testing.T) {
	var ran [][]string
	o := fixtureOptions()
	o.Exec = recorder(&ran)

	a := appAt(t, o, 113, 26)
	// Off the live row, so the key has somewhere to go.
	for a.m.cursorIsLive() {
		a, _, _ = a.key(keyPress("down"))
	}
	want := a.m.Snap.Rows[a.m.Cursor].Account.UUID

	next, cmd, ok := a.key(keyPress("s"))
	if !ok {
		t.Fatal("the switch key was not handled")
	}
	drain(cmd)

	if next.scr == screenPicker {
		t.Fatal("pressing s opened a picker")
	}
	if len(ran) != 1 {
		t.Fatalf("pressing s ran %d commands, want 1: %v", len(ran), ran)
	}
	if ran[0][0] != "switch" {
		t.Errorf("pressing s ran %v, want a switch", ran[0])
	}
	// The WHOLE uuid, never the display ordinal and never a prefix: the table is
	// grouped by provider and the ordinal is recompacted when an account is
	// removed, so an argv built from the number on the screen moves whichever
	// credential now occupies that slot.
	if len(ran[0]) < 2 || ran[0][1] != want {
		t.Errorf("pressing s ran %v, want the uuid %q", ran[0], want)
	}
}

// Every row, not just the first. Cursor indexes Snap.Rows while the table is
// drawn grouped by provider, so a key that read the DISPLAY position would name
// a different account from the second row down.
func TestTheSwitchKeyNamesTheCursorsAccountOnEveryRow(t *testing.T) {
	n := len(appAt(t, fixtureOptions(), 113, 26).m.Snap.Rows)
	if n < 2 {
		t.Fatalf("the fixture has %d rows, so the cursor cannot point anywhere but the top", n)
	}
	for i := range n {
		var ran [][]string
		o := fixtureOptions()
		o.Exec = recorder(&ran)
		a := appAt(t, o, 113, 26)
		for range i {
			a, _, _ = a.key(keyPress("down"))
		}
		if a.m.Cursor != i {
			t.Fatalf("%d downs put the page's cursor on row %d", i, a.m.Cursor)
		}
		if a.m.cursorIsLive() {
			continue
		}
		want := a.m.Snap.Rows[i].Account.UUID
		_, cmd, _ := a.key(keyPress("s"))
		drain(cmd)
		if len(ran) != 1 || len(ran[0]) < 2 || ran[0][1] != want {
			t.Errorf("with the page on row %d (%s) s ran %v, want the uuid %q",
				i, a.m.Snap.Rows[i].Account.Email, ran, want)
		}
	}
}

// On the account a session is already running as, `s` says so rather than
// spending a credential rotation to arrive where it already is. A key that did
// nothing at all would read as a key that failed.
func TestTheSwitchKeyOnTheLiveRowSaysSoAndRunsNothing(t *testing.T) {
	var ran [][]string
	o := fixtureOptions()
	o.Exec = recorder(&ran)

	a := appAt(t, o, 113, 26)
	for !a.m.cursorIsLive() {
		next, _, _ := a.key(keyPress("down"))
		if next.m.Cursor == a.m.Cursor {
			t.Fatal("the fixture has no live row to land on")
		}
		a = next
	}

	next, cmd, ok := a.key(keyPress("s"))
	if !ok {
		t.Fatal("the switch key was not handled on the live row")
	}
	drain(cmd)
	if len(ran) != 0 {
		t.Fatalf("s on the live row ran %v", ran)
	}
	if next.scr != screenPanel {
		t.Fatalf("s on the live row left screen %v, want a panel saying why", next.scr)
	}
	if !strings.Contains(next.note, "already the live login") {
		t.Errorf("the panel says %q, want it to name the reason", next.note)
	}
}

// The dedicated switch screen is gone, so nothing may still build one. A list
// left behind would be a second definition of what a switch target is, free to
// disagree with the cursor about which accounts are offered.
func TestNothingBuildsASwitchListAnyMore(t *testing.T) {
	a := appAt(t, fixtureOptions(), 113, 26)
	for range len(a.m.Snap.Rows) + 2 {
		next, _, _ := a.key(keyPress("s"))
		if next.scr == screenPicker {
			t.Fatalf("s opened a picker from row %d", a.m.Cursor)
		}
		a, _, _ = a.key(keyPress("down"))
	}
}

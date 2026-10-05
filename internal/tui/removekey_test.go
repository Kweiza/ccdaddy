package tui

import (
	"slices"
	"strings"
	"testing"
)

// `X` asks before it deletes anything. Unlike `s`, which moves a credential
// that can be moved back, a removal throws a stored login away, so the key opens
// a confirmation instead of running a command.
func TestTheRemoveKeyAsksFirstAndRunsNothing(t *testing.T) {
	var ran [][]string
	o := fixtureOptions()
	o.Exec = recorder(&ran)
	a := appAt(t, o, 113, 26)
	acct := a.m.Snap.Rows[a.m.Cursor].Account

	next, cmd, ok := a.key(keyPress("X"))
	if !ok {
		t.Fatal("the remove key was not handled")
	}
	drain(cmd)
	if len(ran) != 0 {
		t.Fatalf("pressing X ran %v before anything was confirmed", ran)
	}
	if next.scr != screenPicker {
		t.Fatalf("pressing X left screen %v, want the confirmation", next.scr)
	}
	// Named with its provider and Ref: one email can be a Claude and a Codex
	// account at once, and the bare email would not say which is going.
	if body := next.body(); !strings.Contains(body, acct.Unambiguous()) {
		t.Errorf("the confirmation does not name %q:\n%s", acct.Unambiguous(), body)
	}
	// It opens on keeping the account, so X then a reflexive enter deletes
	// nothing -- the [y/N] default of `ccdad remove`'s own prompt.
	if argv := next.pick.Chosen(); argv != nil {
		t.Errorf("the confirmation opens on %v, want the choice that keeps the account", argv)
	}
}

// Enter on the confirmation's default and esc from it both go home and run
// nothing.
func TestTheRemoveConfirmationCancels(t *testing.T) {
	for _, keys := range [][]string{{"enter"}, {"esc"}, {"down", "esc"}} {
		var ran [][]string
		o := fixtureOptions()
		o.Exec = recorder(&ran)
		a := appAt(t, o, 113, 26)

		a, _, _ = a.key(keyPress("X"))
		for _, k := range keys {
			next, c, _ := a.key(keyPress(k))
			drain(c)
			a = next
		}
		if len(ran) != 0 {
			t.Errorf("X then %v ran %v", keys, ran)
		}
		if a.scr != screenPage {
			t.Errorf("X then %v left screen %v, want the page", keys, a.scr)
		}
	}
}

// Confirming removes the account under the cursor, by its whole uuid, on every
// row. Cursor indexes Snap.Rows while the table is drawn grouped by provider,
// and the ordinal is per provider, so an argv built from the number on the
// screen would delete whichever account shares it on the other side.
func TestConfirmingTheRemovalRemovesTheCursorsAccountByUUID(t *testing.T) {
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
		want := a.m.Snap.Rows[i].Account.UUID

		for _, k := range []string{"X", "down", "enter"} {
			next, cmd, _ := a.key(keyPress(k))
			drain(cmd)
			a = next
		}
		// --yes because the executor has no terminal to answer [y/N] at, and
		// the confirmation the command would have asked for is the one this
		// page just took.
		if wantArgv := []string{"remove", want, "--yes"}; len(ran) != 1 || !slices.Equal(ran[0], wantArgv) {
			t.Errorf("confirming on row %d (%s) ran %v, want %v",
				i, a.m.Snap.Rows[i].Account.Email, ran, wantArgv)
		}
	}
}

// The keybar offers it. A key nobody is told about is a feature nobody has.
func TestTheKeybarOffersRemove(t *testing.T) {
	a := appAt(t, fixtureOptions(), 113, 26)
	if !strings.Contains(a.m.Body(), "X remove") {
		t.Errorf("the page does not advertise the remove key:\n%s", a.m.Body())
	}
}

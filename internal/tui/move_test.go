package tui

import (
	"encoding/json"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/theme"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// reorderableRows is three Claude seats over two Codex ones, in the order a
// store hands them out.
//
// The addresses end in @fleet.test and not @example.com so that a line naming
// an account can be told from a summary line naming one: these tests count
// ACCOUNT ROWS on the rendered page, and the summary block above the table
// spells the live account's address too.
func reorderableRows() []view.Row {
	row := func(uuid, email string, idx int, p provider.ID) view.Row {
		return view.Row{Account: store.Account{UUID: uuid, Email: email, Idx: idx, Provider: p}}
	}
	return []view.Row{
		row("c-alpha", "alpha@fleet.test", 1, provider.Claude),
		row("c-bravo", "bravo@fleet.test", 2, provider.Claude),
		row("c-charlie", "charlie@fleet.test", 3, provider.Claude),
		row("x-delta", "delta@fleet.test", 1, provider.Codex),
		row("x-echo", "echo@fleet.test", 2, provider.Codex),
	}
}

// reorderApp is a dashboard over that fleet, with whatever the keys release
// captured into argv rather than run.
func reorderApp(t *testing.T, argv *[][]string) App {
	t.Helper()
	snap := view.Snapshot{Now: fixtureNow, Rows: reorderableRows(), Version: fixtureVersion}
	o := Options{
		Load: func(time.Time) (view.Snapshot, error) { return snap, nil },
		Now:  func() time.Time { return fixtureNow },
		Exec: func(a []string) (int, string, string) {
			if argv != nil {
				*argv = append(*argv, slices.Clone(a))
			}
			return exitOK, "", ""
		},
		Out:       io.Discard,
		Theme:     theme.None,
		GlyphSet:  "unicode",
		StderrTTY: true,
	}
	return appAt(t, o, 113, 30)
}

// order is the fleet as the page currently holds it, which is the preview while
// a row is in hand and the store's own order otherwise.
func order(a App) string {
	uuids := make([]string, 0, len(a.m.Snap.Rows))
	for _, r := range a.m.Snap.Rows {
		uuids = append(uuids, r.Account.UUID)
	}
	return strings.Join(uuids, " ")
}

// cursorRow is which DRAWN account row carries the cursor, counted from the top
// of the table, and -1 when none does.
//
// It is read off the rendered page rather than off Model.Cursor on purpose.
// Cursor is an index into Snap.Rows and the page groups those rows into
// provider sections before drawing them; the whole complaint these tests pin is
// that the two used to be different numbers, so a test that asked the model
// would have agreed with the bug.
func cursorRow(t *testing.T, m Model) int {
	t.Helper()
	at, seen := -1, 0
	for _, line := range strings.Split(m.Body(), "\n") {
		if !strings.Contains(line, "@fleet.test") {
			continue
		}
		if strings.Contains(line, m.Glyphs.Cursor+" ") || strings.Contains(line, m.Glyphs.Grabbed+" ") {
			at = seen
		}
		seen++
	}
	return at
}

// The cursor's index and its position on the page are the same number, on a
// fleet a real store produced.
//
// This is the whole of the reported bug, and it is asserted against a STORE
// rather than a hand-written slice because the bug lived in the disagreement
// between two packages. The store decides what order the accounts come out in;
// internal/view decides what order the sections are drawn in. While the store
// numbered across the whole fleet, its order was the order they were ADDED --
// claude, codex, claude, codex, claude -- and the page drew all three Claude
// rows and then both Codex ones, so pressing down once moved the marker from
// the first row of the table to the fourth and pressing it again moved the
// marker back up to the second.
//
// A test over a slice written out by hand cannot see that: it would be written
// in whichever order its author believed, and it would pass under both. This
// one adds the accounts in the order that used to break it.
func TestTheCursorsIndexIsItsPositionOnThePage(t *testing.T) {
	t.Setenv("CCDAD_HOME", filepath.Join(t.TempDir(), "ccdad"))
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []struct {
		uuid string
		p    provider.ID
	}{
		{"c-alpha", provider.Claude},
		{"x-delta", provider.Codex},
		{"c-bravo", provider.Claude},
		{"x-echo", provider.Codex},
		{"c-charlie", provider.Claude},
	} {
		blob := cclink.Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"AT"}`)}
		if a.p == provider.Codex {
			blob = cclink.Blob{"codexOAuth": json.RawMessage(
				`{"access_token":"AT","refresh_token":"RT","account_id":"acct","user_id":"u"}`)}
		}
		if err := s.Add(store.Account{UUID: a.uuid, Email: a.uuid + "@fleet.test", Provider: a.p}, blob); err != nil {
			t.Fatal(err)
		}
	}

	accounts := s.Accounts()
	rows := make([]view.Row, 0, len(accounts))
	for _, a := range accounts {
		rows = append(rows, view.Row{Account: a})
	}
	m := newModel(view.Snapshot{Now: fixtureNow, Rows: rows, Version: fixtureVersion},
		113, 30, theme.Of(theme.None), UnicodeGlyphs)

	for i := range rows {
		m.Cursor = i
		m = scrolled(m)
		if got := cursorRow(t, m); got != i {
			t.Errorf("Cursor %d draws on page row %d, want %d -- the cursor does not walk the page:\n%s",
				i, got, i, m.Body())
		}
	}
}

// m picks the row up: the marker changes, and the keybar changes with it.
//
// Both are asserted together because either alone is a mode nobody can see. The
// marker says WHICH row is in hand and the bar says what the arrow keys will
// now do, and a reader who has one without the other cannot tell a reorder from
// a cursor that has stopped moving.
func TestTheMoveKeyPicksTheRowUp(t *testing.T) {
	a := reorderApp(t, nil)
	a, _, ok := a.key(keyPress("m"))
	if !ok {
		t.Fatal("the move key was not handled")
	}
	if !a.m.Moving {
		t.Fatal("m did not pick the row up")
	}
	body := a.m.Body()
	if !strings.Contains(body, a.m.Glyphs.Grabbed+" 1 alpha@fleet.test") {
		t.Errorf("the row in hand does not carry the grabbed marker:\n%s", body)
	}
	if !strings.Contains(body, "enter place") || !strings.Contains(body, "esc cancel") {
		t.Errorf("the keybar does not offer the mode's own keys:\n%s", body)
	}
	if strings.Contains(body, "s switch") {
		t.Errorf("the keybar still advertises a key the mode swallows:\n%s", body)
	}
}

// The arrow keys carry the row, and the cursor goes with it.
//
// The cursor following is the whole difference between carrying a row and
// walking a list: left where it was, the marker would sit on the account that
// had just been displaced, and the next press would carry that one instead.
func TestTheArrowKeysCarryTheRowAndTheCursorGoesWithIt(t *testing.T) {
	a := reorderApp(t, nil)
	a, _, _ = a.key(keyPress("m"))
	a, _, _ = a.key(keyPress("down"))

	if got, want := order(a), "c-bravo c-alpha c-charlie x-delta x-echo"; got != want {
		t.Errorf("after one down = %q, want %q", got, want)
	}
	if a.m.Cursor != 1 {
		t.Errorf("the cursor is at %d, want 1 -- it did not follow the row", a.m.Cursor)
	}
	// The IDX cell is renumbered with the preview: the row that moved down is
	// drawn second AND numbered 2, and the row it displaced is numbered 1. A
	// preview that reordered the rows and left the numbers behind would state
	// two different orders in one table, in the one moment a user is choosing
	// between them.
	body := a.m.Body()
	if !strings.Contains(body, a.m.Glyphs.Grabbed+" 2 alpha@fleet.test") {
		t.Errorf("the marker is not on the row that moved, renumbered:\n%s", body)
	}
	if !strings.Contains(body, " 1 bravo@fleet.test") {
		t.Errorf("the displaced row was not renumbered:\n%s", body)
	}
}

// A row cannot be carried out of its own provider, at either end of the run.
//
// The section boundary is a real boundary and not a rendering: crossing it would
// change the account's section, its quota block and its number all at once, and
// `ccdad move` -- which this key is the dashboard's half of -- counts positions
// within one provider and cannot express the destination at all.
func TestARowCannotBeCarriedIntoTheOtherProvider(t *testing.T) {
	a := reorderApp(t, nil)
	// Down to the last Claude row, then pick it up and push.
	a, _, _ = a.key(keyPress("down"))
	a, _, _ = a.key(keyPress("down"))
	a, _, _ = a.key(keyPress("m"))
	for range 3 {
		a, _, _ = a.key(keyPress("down"))
	}
	if got, want := order(a), "c-alpha c-bravo c-charlie x-delta x-echo"; got != want {
		t.Errorf("a Claude row was pushed past the section boundary: %q, want %q", got, want)
	}

	// And the same at the top of the CODEX run.
	b := reorderApp(t, nil)
	for range 3 {
		b, _, _ = b.key(keyPress("down"))
	}
	b, _, _ = b.key(keyPress("m"))
	for range 3 {
		b, _, _ = b.key(keyPress("up"))
	}
	if got, want := order(b), "c-alpha c-bravo c-charlie x-delta x-echo"; got != want {
		t.Errorf("a Codex row was pulled above the section boundary: %q, want %q", got, want)
	}
}

// enter releases `move <uuid> <position>`, with the position counted from the
// start of the account's OWN provider.
//
// By uuid and never by the drawn position, for the reason the switch key names
// its account by uuid: the position is a number about a section and the account
// is what the command has to act on. The position is 1 here and not 4 -- the
// last Codex row was carried to the top of the CODEX section, which is the
// first position `ccdad move` counts, while 4 is where it sits in the store's
// whole slice and would name a Claude account this key never touched.
func TestEnterReleasesTheMoveCommandWithAProviderScopedPosition(t *testing.T) {
	var released [][]string
	a := reorderApp(t, &released)
	for range 4 {
		a, _, _ = a.key(keyPress("down"))
	}
	a, _, _ = a.key(keyPress("m"))
	a, _, _ = a.key(keyPress("up"))
	next, cmd, ok := a.key(keyPress("enter"))
	if !ok || cmd == nil {
		t.Fatal("enter did not release the move")
	}
	drain(cmd)

	want := []string{"move", "x-echo", "1"}
	if len(released) != 1 || !slices.Equal(released[0], want) {
		t.Fatalf("enter released %v, want one %v", released, want)
	}
	if next.m.Moving {
		t.Error("the mode is still open after the row was placed")
	}
}

// esc puts the list back the way the store still holds it.
//
// Nothing has to be undone, because nothing was done: the reorder was a preview
// and the store was never told. That is the whole reason the key can be offered
// at all.
func TestEscapeCancelsTheMoveAndRestoresTheOrder(t *testing.T) {
	var released [][]string
	a := reorderApp(t, &released)
	before := order(a)

	a, _, _ = a.key(keyPress("m"))
	a, _, _ = a.key(keyPress("down"))
	a, _, _ = a.key(keyPress("down"))
	if order(a) == before {
		t.Fatal("the rows did not move, so the cancel below would prove nothing")
	}
	a, _, _ = a.key(keyPress("esc"))

	if got := order(a); got != before {
		t.Errorf("after esc = %q, want the order it started at %q", got, before)
	}
	if a.m.Cursor != 0 {
		t.Errorf("the cursor is at %d after the cancel, want 0", a.m.Cursor)
	}
	if a.m.Moving {
		t.Error("esc left the mode open")
	}
	if len(released) != 0 {
		t.Errorf("a cancelled move released %v, want nothing", released)
	}
}

// A row put back where it started releases nothing. `ccdad move` answers that
// with exit 3 and a sentence, and spending a command -- and a panel over the
// page -- to be told that nothing happened is worse than the mode ending.
func TestPlacingARowWhereItStartedReleasesNothing(t *testing.T) {
	var released [][]string
	a := reorderApp(t, &released)
	a, _, _ = a.key(keyPress("m"))
	a, _, _ = a.key(keyPress("down"))
	a, _, _ = a.key(keyPress("up"))
	next, cmd, _ := a.key(keyPress("enter"))
	drain(cmd)

	if len(released) != 0 {
		t.Errorf("an unmoved row released %v, want nothing", released)
	}
	if next.m.Moving {
		t.Error("enter left the mode open")
	}
}

// Every key the page offers is SWALLOWED while a row is in hand.
//
// The two named here are the ones that make it a rule rather than tidiness: `s`
// moves a credential and `a` releases the terminal to a login, and both would be
// acting against a list whose order is a preview of something the store has
// never been told about.
func TestThePagesOwnKeysAreSwallowedWhileARowIsInHand(t *testing.T) {
	var released [][]string
	a := reorderApp(t, &released)
	a, _, _ = a.key(keyPress("m"))

	for _, k := range []string{"s", "a", "c", "d", "r", "?", "q"} {
		next, cmd, ok := a.key(keyPress(k))
		if !ok {
			t.Errorf("%q was not handled while a row was in hand", k)
		}
		if cmd != nil {
			t.Errorf("%q released a command while a row was in hand", k)
		}
		if next.scr != screenPage || !next.m.Moving {
			t.Errorf("%q left the move mode: screen %d, moving %v", k, next.scr, next.m.Moving)
		}
	}
	if len(released) != 0 {
		t.Errorf("a swallowed key released %v, want nothing", released)
	}
}

// Every key the moving keybar ADVERTISES is answered while a row is in hand.
//
// This is the invariant probes() holds for the five screens, stated for the one
// state that is not a screen. A bar offering a key the mode ignores is a string
// painted on a terminal that nothing else reads.
func TestTheMovingKeybarOffersOnlyKeysTheModeAnswers(t *testing.T) {
	// Picked up from the MIDDLE of the Claude run, so that both arrow keys have
	// somewhere to go: a row already at the top answers up by doing nothing,
	// which is correct and would fail this test for a reason that is not a bug.
	a := reorderApp(t, nil)
	a, _, _ = a.key(keyPress("down"))
	a, _, _ = a.key(keyPress("m"))

	for _, b := range a.keys().MovingHelp() {
		for _, name := range b.Keys() {
			fresh := reorderApp(t, nil)
			fresh, _, _ = fresh.key(keyPress("down"))
			fresh, _, _ = fresh.key(keyPress("m"))
			before := order(fresh)
			next, _, ok := fresh.key(keyPress(name))
			if !ok {
				t.Errorf("the moving keybar offers %q, which the mode does not answer", name)
				continue
			}
			// Answered means something changed: the order, the cursor, or the
			// mode itself. A key that is merely swallowed is one the bar must
			// not have named.
			if order(next) == before && next.m.Cursor == fresh.m.Cursor && next.m.Moving == fresh.m.Moving {
				t.Errorf("%q changed nothing while a row was in hand, but the keybar offers it", name)
			}
		}
	}
}

// A reload landing mid-move does not take the preview away.
//
// The reload clock keeps ticking while a row is in hand, and a load replaces
// the snapshot whole -- so without this the rows would snap back to the store's
// order under a cursor that is carrying one of them, on a ten-second timer,
// with nothing on the page to say why.
func TestAReloadDoesNotLandWhileARowIsInHand(t *testing.T) {
	a := reorderApp(t, nil)
	a, _, _ = a.key(keyPress("m"))
	a, _, _ = a.key(keyPress("down"))
	moved := order(a)

	next, cmd := a.Update(reloadMsg(fixtureNow))
	a = next.(App)
	if got := order(a); got != moved {
		t.Errorf("the reload tick replaced the preview: %q, want %q", got, moved)
	}
	// The clock itself is untouched: the tick must still schedule its
	// successor, or the page stops refreshing for the rest of the session.
	if cmd == nil {
		t.Fatal("the reload tick scheduled no successor, so the clock has stopped")
	}
	for _, msg := range drain(cmd) {
		if _, ok := msg.(loadedMsg); ok {
			t.Error("the reload tick read the store while a row was in hand")
		}
	}
}

// m on a provider holding one account says so, rather than opening a mode in
// which both arrow keys do nothing.
//
// A key that opens something that cannot move reads as a key that failed, which
// is the reason `s` answers the already-live row with a sentence instead of
// silence.
func TestTheMoveKeyRefusesAProviderWithOneAccount(t *testing.T) {
	snap := view.Snapshot{Now: fixtureNow, Version: fixtureVersion, Rows: []view.Row{
		{Account: store.Account{UUID: "c-only", Email: "alpha@fleet.test", Idx: 1, Provider: provider.Claude}},
		{Account: store.Account{UUID: "x-only", Email: "delta@fleet.test", Idx: 1, Provider: provider.Codex}},
	}}
	a := appAt(t, Options{
		Load:      func(time.Time) (view.Snapshot, error) { return snap, nil },
		Now:       func() time.Time { return fixtureNow },
		Out:       io.Discard,
		Theme:     theme.None,
		GlyphSet:  "unicode",
		StderrTTY: true,
	}, 113, 30)

	next, _, ok := a.key(keyPress("m"))
	if !ok {
		t.Fatal("the move key was not handled")
	}
	if next.m.Moving {
		t.Error("m opened a reorder on a provider with nothing to reorder")
	}
	// The sentence itself and not the drawn panel: the panel cuts to the
	// terminal it is drawn in, so a comparison against the page would be
	// reading the cut rather than the answer.
	if next.scr != screenPanel || next.note != nothingToReorder {
		t.Errorf("m said %q on screen %d, want %q on the panel", next.note, next.scr, nothingToReorder)
	}
}

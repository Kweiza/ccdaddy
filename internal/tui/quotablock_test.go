package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/view"
)

// The live dashboard drew one column headed QUOTA with "?" in every cell, on
// every fleet, for as long as the field existed.
//
// Model.Cols is DERIVED from the snapshot and was assigned in exactly one place:
// newModel. The live program's only construction site is newApp, which builds a
// Model from an EMPTY snapshot because the read is asynchronous and has not
// happened yet -- so the page was born with ColumnsOf(nil), every load replaced
// the rows and left the block behind, and view.ListColumns took its placeholder
// arm forever.
//
// Nothing in this package could see it. Every golden page calls newModel with a
// populated snapshot, which is the one path that was never broken.
func TestTheQuotaBlockFollowsTheSnapshotIntoTheLivePage(t *testing.T) {
	stubTick(t)
	a := appAt(t, fixtureOptions(), 113, 26)

	if len(a.m.Snap.Rows) == 0 {
		t.Fatal("the fixture loaded no rows, so this proves nothing about the block")
	}
	if len(a.m.Cols.Windows) == 0 {
		t.Fatalf("the page has %d rows and no quota columns; every cell renders %q under %q",
			len(a.m.Snap.Rows), view.Unreadable, view.PlaceholderHeader)
	}
}

// And the page says so: the header carries the fleet's own window names rather
// than the placeholder, and the cells carry figures rather than "?".
func TestTheLivePageDrawsTheFleetsOwnQuotaColumns(t *testing.T) {
	stubTick(t)
	a := appAt(t, fixtureOptions(), 113, 26)
	got := a.m.Body()

	if strings.Contains(got, view.PlaceholderHeader) {
		t.Errorf("the live page still draws the %q placeholder:\n%s", view.PlaceholderHeader, got)
	}
	if !strings.Contains(got, "5H") {
		t.Errorf("the live page names no window the fixture carries:\n%s", got)
	}
}

// A failed refresh keeps the last good block with the last good rows. Rebuilding
// it from a snapshot that never arrived would empty the table's columns while the
// rows it is being drawn from still have windows in them.
func TestAFailedRefreshKeepsTheQuotaBlockItWasDrawnWith(t *testing.T) {
	stubTick(t)
	a := appAt(t, fixtureOptions(), 113, 26)
	before := a.m.Cols

	after := a.m.AfterLoad(view.Snapshot{}, errors.New("store is locked"))
	if len(after.Cols.Windows) != len(before.Windows) {
		t.Errorf("a failed refresh changed the quota block from %d columns to %d",
			len(before.Windows), len(after.Cols.Windows))
	}
}

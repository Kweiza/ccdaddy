package tui

import (
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/view"
)

// ladderFootprint is what the width ladder reserved for one shared column,
// column kind by column kind, as the ladder itself spelled it.
//
// It is a TRANSCRIPT and it lives here on purpose. planWidth held a constant
// per column until the shared list published a heading and a content width for
// each; it now measures what the column says instead, which is what makes the
// two agree by construction rather than by two people keeping two tables in
// step. That leaves this test asking a question a table read out of view could
// not: are the numbers the same ones the dashboard has always reserved? Only a
// hand-written copy can answer it, so the copy is here, where a reader looking
// at a failure can see what the page used to reserve and what it reserves now.
//
// Every value is the constant planWidth carried before, verbatim: 6 for IDX and
// accountComfort+2 for ACCOUNT, which cost() spelled as literals, and the eight
// named footprints. ACCOUNT keeps its constant because accountComfort is still
// production's -- the ladder's ACCOUNT stops are not a column width the shared
// list publishes.
func ladderFootprint(c view.ListColumn) (int, bool) {
	const (
		typeFootprint  = 14 // 12 content + 2 gap
		autoFootprint  = 6  // 4 content + 2 gap
		stateFootprint = 15 // 13 content + 2 gap
		tierFootprint  = 8  // 6 content + 2 gap
		ageFootprint   = 8  // 6 content + 2 gap
		// worstFootprint held "100% " plus a header of HeaderBudget, which is
		// what the collapsed block renders.
		worstFootprint = view.HeaderBudget + 7
	)
	// The window and reset footprints were functions rather than constants,
	// because the content half is fixed and small -- a percentage is at most
	// "100%" and a countdown at most "1d16h" -- so the HEADER is what decides,
	// and a header is the fleet's own.
	switch c.Kind {
	case view.ColumnIdx:
		return 6, true
	case view.ColumnAccount:
		return accountComfort + 2, true
	case view.ColumnType:
		return typeFootprint, true
	case view.ColumnTier:
		return tierFootprint, true
	case view.ColumnWindow:
		return maxInt(ansi.StringWidth(c.Header), 4) + 2, true
	case view.ColumnReset:
		return maxInt(ansi.StringWidth(c.Header), 5) + 2, true
	case view.ColumnWorst:
		return worstFootprint, true
	case view.ColumnState:
		return stateFootprint, true
	case view.ColumnAuto:
		return autoFootprint, true
	case view.ColumnAge:
		return ageFootprint, true
	}
	return 0, false
}

// The shared column set publishes a heading and a content width so that the
// ladder can read them instead of holding a second copy of the same numbers.
// Adopting them may not move a page, which means every column has to ask for
// exactly what the ladder reserved before it did.
//
// The reservation is max(heading, content) + 2: the content, or the heading
// when it is wider, plus the standard gap that follows a column.
func TestTheLadderReservesWhatTheSharedColumnsAskFor(t *testing.T) {
	block := view.ColumnsOf(fixtureRows())
	cols := view.ListColumns(block)
	// WORST is never in the set -- collapsing is what puts it on a table -- and
	// it is the widest column here, so it is asked about explicitly.
	cols = append(cols, view.CollapseWindows(cols)[4])
	for _, c := range cols {
		want, ok := ladderFootprint(c)
		if !ok {
			t.Fatalf("%s is a column kind the ladder reserves nothing for", c.Kind)
		}
		if got := maxInt(ansi.StringWidth(c.Header), c.Content) + 2; got != want {
			t.Errorf("%s (%q) asks for %d columns, the ladder reserves %d", c.Kind, c.Header, got, want)
		}
	}
}

// The per-column check above is only as good as its own table of constants.
// This one asks the running ladder instead, and it covers the two footprints
// written as literals inside cost() that no constant names.
//
// fullAt is the width at which every optional column is already on the page,
// and it is observable from outside: below it ACCOUNT is held flat at
// accountComfort, and above it ACCOUNT grows one column per column of
// terminal. So the narrowest width whose ACCOUNT is one wider than the comfort
// stop is fullAt + 1.
//
// TIER is subtracted because the ladder has not adopted it yet: it is in the
// shared fixed order and not in planWidth's own, which is the one difference
// between the two sets while the surfaces are still being moved over.
func TestTheLaddersFullWidthIsTheSumOfWhatTheSharedColumnsAskFor(t *testing.T) {
	block := view.ColumnsOf(fixtureRows())

	want := 2 // the border
	for _, c := range view.ListColumns(block) {
		if c.Kind == view.ColumnTier {
			continue
		}
		f, ok := ladderFootprint(c)
		if !ok {
			t.Fatalf("%s is a column kind the ladder reserves nothing for", c.Kind)
		}
		want += f
	}

	got := 0
	for w := 43; w < 400; w++ {
		if Plan(block, w, 60, 4, false, false).AccountWide == accountComfort+1 {
			got = w - 1
			break
		}
	}
	if got == 0 {
		t.Fatal("no width between 43 and 400 grows ACCOUNT past its comfort stop")
	}
	if got != want {
		t.Errorf("the ladder is full at %d columns; the shared column set adds up to %d", got, want)
	}
}

package tui

import (
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/view"
)

// ladderFootprint is what the width ladder reserves for one shared column
// today, column kind by column kind.
//
// IDX and ACCOUNT are spelled here because planWidth's cost function spells
// them as literals rather than as named constants; every other arm names the
// constant itself, so a footprint that moves moves this test with it.
func ladderFootprint(c view.ListColumn) (int, bool) {
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
		return windowFootprint(c.Header), true
	case view.ColumnReset:
		return resetFootprint(c.Header), true
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

// The shared column set publishes a heading and a content width so that this
// ladder can read them instead of holding a second copy of the same numbers.
// Adopting them may not move a page, which means every column has to ask for
// exactly what the ladder already reserves.
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

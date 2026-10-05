package view

import (
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// idxColumn is the IDX column as the table plans it, with its own Content.
func idxColumn(t *testing.T) ListColumn {
	t.Helper()
	for _, c := range ListColumns(ColumnsOf([]Row{listRow()})) {
		if c.Kind == ColumnIdx {
			return c
		}
	}
	t.Fatal("the planned columns carry no IDX column")
	return ListColumn{}
}

// The IDX cell prints the Ref -- "c1", "x1" -- and not the bare number. The
// number restarts per provider, so a mixed fleet shows "1" twice, and what a
// reader types next has to be what the row says.
func TestTheIdxCellIsTheRef(t *testing.T) {
	c := idxColumn(t)
	for _, tc := range []struct {
		row  Row
		want string
	}{
		{Row{Account: store.Account{Provider: provider.Claude, Idx: 1}, Active: true}, "* c1"},
		{Row{Account: store.Account{Provider: provider.Claude, Idx: 2}}, "  c2"},
		{Row{Account: store.Account{Provider: provider.Codex, Idx: 1}}, "  x1"},
	} {
		if got := tc.row.ListCell(c, Columns{}, colNow, false); got != tc.want {
			t.Errorf("%s %d: IDX cell = %q, want %q", tc.row.Account.Provider, tc.row.Account.Idx, got, tc.want)
		}
	}
}

// The Codex account the proxy serves new threads from carries the same mark the
// live Claude login does: each is the answer to "which account would a new
// session get" for its own provider.
func TestTheServingCodexRowCarriesTheMarker(t *testing.T) {
	r := Row{Account: store.Account{Provider: provider.Codex, Idx: 2}, Serving: true}
	if got := r.Marker(); got != "*" {
		t.Errorf("Marker() on the serving codex row = %q, want %q", got, "*")
	}
	if got := r.ListCell(idxColumn(t), Columns{}, colNow, false); got != "* x2" {
		t.Errorf("IDX cell on the serving codex row = %q, want %q", got, "* x2")
	}
	if !r.Marked() {
		t.Error("Marked() is false on the serving codex row")
	}
	if (Row{Account: store.Account{Provider: provider.Codex, Idx: 2}}).Marked() {
		t.Error("Marked() is true on a codex row nobody serves from")
	}
}

// The column reserves room for a two-digit Ref behind the marker. A ladder that
// reserved less would draw a fleet of ten past its own frame.
func TestTheIdxColumnHoldsATwoDigitRef(t *testing.T) {
	c := idxColumn(t)
	cell := Row{Account: store.Account{Provider: provider.Claude, Idx: 12}, Active: true}.ListCell(c, Columns{}, colNow, false)
	if w := ansi.StringWidth(cell); w > c.Content {
		t.Errorf("IDX cell %q is %d wide; the column reserves %d", cell, w, c.Content)
	}
}

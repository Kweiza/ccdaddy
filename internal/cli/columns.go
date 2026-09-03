package cli

import (
	"io"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"

	"github.com/Kweiza/ccdaddy/internal/theme"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// columns is every one-shot table this binary prints, and it replaces
// text/tabwriter everywhere rather than sitting beside it.
//
// tabwriter measures a cell by counting runes and cannot see an SGR escape, so
// the moment one cell is coloured its column is padded for the escape bytes as
// well as for the text -- and it pads the WRONG way round, because a styled
// cell counts as wide and therefore asks for less padding than the bare cell
// beside it. Measured on this tree, three ACCOUNT cells of which only the
// header was styled: the column came out 9 display columns wide on the header
// row and 31 on both data rows. That is not degradation, it is a destroyed
// table -- and it is the PIPED case that breaks, not the terminal one, because
// colorprofile strips downstream of the layout: a redirected invocation gets
// the wrecked padding with none of the colour that would explain it.
//
// lipgloss/table measures ANSI-aware, and East-Asian-aware as well, which is a
// second bug fixed rather than a second bug avoided. Measured on an ACCOUNT
// column holding a fifteen-character ASCII address and a three-character
// Hangul name: lipgloss starts the next column at display column 17 on both
// rows, and tabwriter starts it at 17 on the ASCII row and 20 on the Hangul
// one -- it counted three runes where the terminal draws six columns, so that
// row hangs three columns right of every row above and below it.
//
// It is byte-identical to tabwriter.NewWriter(w, 0, 0, 2, ' ', 0) on every
// shape this repository prints, and where it is not identical the difference
// is trailing whitespace and only trailing whitespace: tabwriter pads a cell
// that is followed by a tab even when the cell after it is empty, so it can
// end a line in spaces, and the per-line TrimRight below cuts them. Nothing
// renders differently and no assertion in this package can see it. The trim is
// safe against colour because lipgloss closes a styled cell with its reset
// BEFORE the padding -- measured, a styled first cell arrives as
// "\x1b[32mx\x1b[m     1" -- so a trim can never cut an escape in half.
//
// style is nil for a table that takes no colour and the caller's per-cell
// style otherwise. row is 0-based over rows and table.HeaderRow for the
// header, which is why the header goes through Headers rather than being
// prepended as row 0: with the header as row 0 every data row's index is one
// too high, and a callback that indexes the caller's slice by it -- which is
// what the recipe in internal/tui/render.go does, correctly, under Headers --
// mis-styles every row and reads past the end of the last.
func columns(w io.Writer, headers []string, rows [][]string, style func(row, col int) lipgloss.Style) error {
	// A table with nothing in it writes nothing. Splitting the empty string on
	// "\n" yields one empty element, which would otherwise put a bare newline
	// on stdout where tabwriter put no byte at all.
	if len(headers) == 0 && len(rows) == 0 {
		return nil
	}

	// last is the widest row's final index, not the header's: a row longer than
	// the header would otherwise be padded on its last cell and end the line in
	// spaces the trim then has to take back off.
	last := len(headers) - 1
	clean := make([][]string, len(rows))
	for i, r := range rows {
		c := make([]string, len(r))
		for j, cell := range r {
			c[j] = tableCell.Replace(cell)
		}
		clean[i] = c
		if len(c)-1 > last {
			last = len(c) - 1
		}
	}

	t := table.New().
		Border(lipgloss.Border{}).
		BorderTop(false).BorderBottom(false).
		BorderLeft(false).BorderRight(false).
		BorderHeader(false).BorderColumn(false).BorderRow(false).
		// NOT Wrap(false), which internal/tui/render.go sets and which is right
		// there and wrong here: every cell in that table is cut to size before
		// it arrives, and nothing in this one is. Measured, a cell reading
		// "one\ntwo" under Wrap(false) renders as "on…" -- the second line is
		// gone and nothing says so. The replacer below is what makes that
		// unreachable instead.
		StyleFunc(func(row, col int) lipgloss.Style {
			st := lipgloss.NewStyle()
			if style != nil {
				st = style(row, col)
			}
			// Two columns of padding after every cell but the last, which is
			// the whole of what tabwriter's padding argument did. The last
			// column carries none, because the cell that ends a line was never
			// followed by a tab.
			if col == last {
				return st
			}
			return st.PaddingRight(2)
		}).
		Rows(clean...)
	if len(headers) > 0 {
		head := make([]string, len(headers))
		for i, cell := range headers {
			head[i] = tableCell.Replace(cell)
		}
		t = t.Headers(head...)
	}

	for _, line := range strings.Split(t.String(), "\n") {
		if _, err := io.WriteString(w, strings.TrimRight(line, " ")+"\n"); err != nil {
			return err
		}
	}
	return nil
}

// tableCell is the transformation every cell goes through, and it is not
// cosmetic tidying.
//
// lipgloss/table lays a cell out as a block. A newline inside one makes that
// cell two lines tall and pushes every other column's row out of line;
// Wrap(false) does not fix it, it DISCARDS the second line. Measured on a cell
// reading "one\ntwo": wrapped it renders as two rows, unwrapped it renders
// "one" and "two" is simply gone. Silent content loss is worse than either.
//
// A carriage return is worse than a newline because it survives the layout
// untouched and then rewrites the finished line on the terminal, and a tab
// inside a cell breaks the block the same way a newline does.
//
// None of this is hypothetical. doctor's Detail is built from err.Error() in
// seven places -- a path, an unmarshal failure, an exec error -- and an account
// alias is whatever a user typed. A control character in a cell is an input
// this has to survive, not a case that cannot arise.
var tableCell = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ")

// quotaCellStyle is the colour on `status`'s and `list`'s tables, and it is one
// function for both because the two describe one store: two copies would be two
// chances for the same account to be painted two ways on two screens.
//
// It has three arms and it deliberately has no fourth, because every arm here
// has to obey one bound: colour is never the only thing carrying a
// distinction. The header's cells are words. The active marker is a "*"
// printed in the IDX gutter, so the row says it is active with a glyph whether
// or not anything painted it. The muted arm fires only where the cell's own
// text is "?", which is itself the statement that the value could not be read.
// Strip every escape byte out of either table and nothing is lost.
//
// THE ARM THAT IS NOT HERE: an exhausted account is not painted. Row.Empty()
// is strategy.OutOfQuota, which is Headroom.MinPct <= 0 -- the LEAST raw room
// any binding window has. `list`'s LEFT column prints Headroom.Pct, read off
// the window with the least SLACK, and `status`'s USED column prints the
// REPORTED window. Those are different windows by construction, so an account
// the engine has filed as spent can print "75%" in LEFT and 40% in USED, and a
// red cell over either number would be saying something the number beside it
// contradicts. Worse, it would be saying it in colour alone: `ccdad list`
// carries no STATE column at any width and `ccdad status` carries none either,
// so there is no glyph and no word anywhere on the row for a reader to recover
// the verdict from -- exactly the sole-carrier failure a monochrome terminal,
// a NO_COLOR user and a colour-blind reader each turn into no information at
// all. The exhausted distinction stays where it keeps its word, which is the
// dashboard's STATE column. `ccdad status --json` carries the verdict for
// anything that needs it programmatically.
//
// label is the column's OWN label function -- UsedLabel for status, LeftLabel
// for list -- and the muted branch asks it rather than re-deriving the cascade
// behind it. LeftLabel's absence is not UsedLabel's: one falls back to the
// credit axis before giving up and the other does not, so a colour keyed on a
// re-derived predicate would eventually dim a cell that reads as a dollar
// figure. Asking the label is what keeps the colour describing the text beside
// it and nothing else.
// windowCellStyle is the colour on the per-window tables, and it is one
// function for `list`, `status` and `hover status` because the three describe
// one store: three copies would be three chances for the same account to be
// painted three ways on three screens.
//
// It has three arms and the bound they obey is the one quotaCellStyle stated:
// COLOUR IS NEVER THE ONLY THING CARRYING A DISTINCTION. Strip every escape
// byte out of the table and nothing is lost.
//
//   - The active marker is a "*" in the IDX gutter, so the row says it is
//     active with a glyph whether or not anything painted it.
//   - The muted arm fires only where the cell's own text is "?", which is
//     itself the statement that the value could not be read.
//   - The over arm fires only where the cell's own text is 100%, which is
//     itself the statement that the window is gone.
//
// THE ARM THAT IS NOT HERE, and it is the one this file used to argue about
// from the other side: there is no amber band. A cell reading "84%" painted
// amber would be saying "close to its threshold" in colour and nowhere else,
// and neither CLI table carries a STATE column at any width for a reader to
// recover it from -- the sole-carrier failure a monochrome terminal, a NO_COLOR
// user and a colour-blind reader each turn into no information at all. The
// dashboard has that column and takes the band there.
//
// What DID change is the over arm, and the reason is the whole of this release.
// It used to be refused because `list`'s LEFT and `status`'s USED read
// different windows by construction, so a red cell over either number was
// saying something the number beside it contradicted. There is no derived
// window any more: a cell is one window, and 100% in it is the cell's own text
// agreeing with its own colour.
func windowCellStyle(pal theme.Palette, rows []view.Row, firstWindowCol int,
	cols view.Columns) func(row, col int) lipgloss.Style {

	return func(row, col int) lipgloss.Style {
		if row == table.HeaderRow {
			return pal.Style(theme.RoleHeader)
		}
		if row < 0 || row >= len(rows) {
			return lipgloss.NewStyle()
		}
		r := rows[row]
		if col == 0 && r.Active {
			return pal.Style(theme.RoleActive)
		}
		i := col - firstWindowCol
		if i < 0 || i >= len(cols.Windows) {
			return lipgloss.NewStyle()
		}
		switch r.CellState(cols.Windows[i].Name) {
		case view.CellUnknown:
			return pal.Style(theme.RoleMuted)
		case view.CellOver:
			return pal.Style(theme.RoleGaugeOver)
		}
		return lipgloss.NewStyle()
	}
}

func quotaCellStyle(pal theme.Palette, rows []view.Row, quotaCol int,
	label func(view.Row) string) func(row, col int) lipgloss.Style {

	return func(row, col int) lipgloss.Style {
		if row == table.HeaderRow {
			return pal.Style(theme.RoleHeader)
		}
		if row < 0 || row >= len(rows) {
			return lipgloss.NewStyle()
		}
		r := rows[row]
		if col == 0 && r.Active {
			return pal.Style(theme.RoleActive)
		}
		// "Could not be read" stays distinct from "no such value" and from
		// "nothing left": three absences, three treatments, and the only one
		// that takes a colour here is the one whose cell says "?" out loud.
		if col == quotaCol && label(r) == view.Unreadable {
			return pal.Style(theme.RoleMuted)
		}
		return lipgloss.NewStyle()
	}
}

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

// windowCellStyle colours each window by its own utilization: blue, green,
// yellow, orange, then red. Unknown readings remain muted, the active marker
// remains accented, and the percentage text preserves the ordered value when
// colour is unavailable.
//
// It is handed the DISPLAY list -- internal/view's ListRow, the same type the
// table's cells were built from -- rather than the account rows, and the two
// are not always the same list. A table draws its lines by integer and styles
// them by the same integer, so a surface whose display list holds anything an
// account list does not would otherwise be painting row N with account N's
// verdict.
func windowCellStyle(pal theme.Palette, display []view.ListRow, accountCol, firstWindowCol int,
	cols view.Columns) func(row, col int) lipgloss.Style {

	return func(row, col int) lipgloss.Style {
		if row == table.HeaderRow {
			return pal.Style(theme.RoleHeader)
		}
		if row < 0 || row >= len(display) {
			return lipgloss.NewStyle()
		}
		// A section heading takes the same role as the column headings above
		// it, which is what makes the two read as one structure. It has to be
		// answered BEFORE the arms below and not beside them: a heading's
		// ListRow carries a zero view.Row, so the active arm would ask an
		// account that is not there and the window arm would band a percentage
		// nobody read.
		//
		// Only the cell HOLDING the text is painted, and every other cell of
		// that row is left plain. That is not tidiness: a heading row is one
		// word and a line of empty cells, and a style on an empty cell wraps
		// its padding in escape bytes, which puts the line's trailing spaces
		// out of reach of the TrimRight above. The coloured table would then
		// strip to something the plain table is not -- which is exactly what
		// TestAColouredTableIsThePlainTableWithColourAddedAndNothingMoved
		// forbids, and what it caught.
		if display[row].Header != "" {
			if col == accountCol {
				return pal.Style(theme.RoleHeader)
			}
			return lipgloss.NewStyle()
		}
		r := display[row].Row
		if col == 0 && r.Active {
			return pal.Style(theme.RoleActive)
		}
		i := col - firstWindowCol
		if i < 0 || i >= len(cols.Windows) {
			return lipgloss.NewStyle()
		}
		pct, state := r.WindowPct(cols.Windows[i].Name)
		switch state {
		case view.WindowUnreadable, view.WindowAbsent:
			return pal.Style(theme.RoleMuted)
		default:
			return pal.Style(theme.UtilizationRole(pct))
		}
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

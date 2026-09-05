package cli

import (
	"time"

	"charm.land/lipgloss/v2"

	"github.com/Kweiza/ccdaddy/internal/theme"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// statusLine is one line of the sectioned account table: what the line IS, and
// the quota block it was drawn under.
//
// The block travels with the line because it now varies WITHIN one table. Each
// provider's half is built from its own rows, so the cell at column i means a
// different window above and below the seam, and a style function handed one
// block for the whole table would band a Codex percentage against a Claude
// window's name.
type statusLine struct {
	// heading is the provider name on a section heading and empty on every other
	// line; header marks the column-heading row that follows it.
	//
	// Both are DATA rows rather than the table's own header, and that is what
	// makes per-section columns possible at all: a lipgloss table has exactly one
	// header row, and this table needs one per section. It is also what keeps the
	// two halves aligned -- one table sizes its columns across every row it holds,
	// where two tables would size them independently and the fleet would come out
	// under headings that do not line up.
	heading string
	header  bool
	row     view.Row
	block   view.Columns
	cols    []view.ListColumn
}

// quotaWidth is how many display columns this section's quota block takes: one
// per window and one per rollover.
func quotaWidth(cols []view.ListColumn) int {
	n := 0
	for _, c := range cols {
		if isQuota(c) {
			n++
		}
	}
	return n
}

func isQuota(c view.ListColumn) bool {
	return c.Kind == view.ColumnWindow || c.Kind == view.ColumnReset
}

// cells is this line as table cells, with the quota block padded out to the
// widest section's.
//
// The padding is what keeps STATE and AGE under one another across the seam. It
// is EMPTY STRINGS and never "-", and the difference is the one this table makes
// everywhere else: "-" is a claim that the quantity does not exist for this
// account, and these cells carry no claim at all -- they are the room a wider
// section needs and this one does not.
func (l statusLine) cells(now time.Time, hover bool) []string {
	out := make([]string, 0, len(l.cols))
	for _, c := range l.cols {
		switch {
		case l.heading != "":
			// A heading is a table row carrying its text in the ACCOUNT cell and
			// nothing anywhere else.
			if c.Kind == view.ColumnAccount {
				out = append(out, l.heading)
			} else {
				out = append(out, "")
			}
		case l.header:
			out = append(out, statusHeader(c))
		default:
			cell := l.row.ListCell(c, l.block, now, hover)
			// StatusFlags rides on the AGE cell: a suffix that belongs to one
			// account reads better beside that account's own figure than at a
			// fixed offset far to its right.
			if c.Kind == view.ColumnAge {
				cell += l.row.StatusFlags()
			}
			out = append(out, cell)
		}
	}
	return out
}

// sectionCellStyle paints a sectioned table: the column headings and the
// provider headings in the heading role, the live account's marker in the active
// role, and every quota cell in the band its own utilization falls in.
//
// It reads each line's OWN block, which is the whole reason statusLine carries
// one. The heading arms answer BEFORE the account arms, so a zero view.Row is
// never asked about an account that is not there.
func sectionCellStyle(pal theme.Palette, lines []statusLine, accountCol, firstWindowCol int) func(row, col int) lipgloss.Style {
	return func(row, col int) lipgloss.Style {
		if row < 0 || row >= len(lines) {
			return lipgloss.NewStyle()
		}
		l := lines[row]
		if col < 0 || col >= len(l.cols) {
			return lipgloss.NewStyle()
		}
		if l.header {
			// The blanks a narrower section pads with are NOT painted. A style
			// on an empty cell wraps its padding in escape bytes, which puts
			// the line's trailing spaces out of reach of the trim in columns --
			// and the coloured table would then strip to something the plain one
			// is not.
			if l.cols[col].Kind == view.ColumnBlank {
				return lipgloss.NewStyle()
			}
			return pal.Style(theme.RoleHeader)
		}
		if l.heading != "" {
			// Only the cell HOLDING the text is painted. A style on an empty
			// cell wraps its padding in escape bytes, which puts the line's
			// trailing spaces out of reach of the trim in columns -- and the
			// coloured table would then strip to something the plain one is not.
			if col == accountCol {
				return pal.Style(theme.RoleHeader)
			}
			return lipgloss.NewStyle()
		}
		if col == 0 && l.row.Active {
			return pal.Style(theme.RoleActive)
		}
		i := col - firstWindowCol
		if i < 0 || i >= len(l.block.Windows) {
			return lipgloss.NewStyle()
		}
		pct, state := l.row.WindowPct(l.block.Windows[i].Name)
		switch state {
		case view.WindowUnreadable, view.WindowAbsent:
			return pal.Style(theme.RoleMuted)
		default:
			return pal.Style(theme.UtilizationRole(pct))
		}
	}
}

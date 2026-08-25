package tui

import "github.com/charmbracelet/x/ansi"

// ColumnSet is which of the two shipping tables the page is showing. [L]
// swaps between them; the heading changes with the polarity, so USED and LEFT
// never share a heading. Plan is the first thing in this package that needs
// the distinction, so it is declared here rather than left undeclared until
// something else does.
type ColumnSet int

const (
	SetStatus ColumnSet = iota // IDX ACCOUNT TYPE USED WINDOW RESETS IN STATE AUTO
	SetList                    // IDX ACCOUNT TYPE TIER LEFT RESETS IN
)

// Column identifies one field of the two shipping tables. ColIdx through
// ColAuto are SetStatus's eight columns; ColTier and ColLeft are the two
// SetList carries instead of ColUsed, ColWindow, ColState and ColAuto.
type Column int

const (
	ColIdx Column = iota
	ColAccount
	ColType
	ColUsed
	ColWindow
	ColResets
	ColState
	ColAuto
	ColTier
	ColLeft
)

// Layout is the answer to "what fits", computed once per frame from the
// terminal size and the row count, and read by nothing but the renderer: it
// carries no styling and touches no string.
type Layout struct {
	Columns     []Column
	AccountWide int
	Collapsed   bool // USED is the bare percentage, not the gauge

	Wordmark, Tagline, Figures bool
	Notice                     bool // the "note: ..." line; see Plan's notice parameter
	Runway                     bool // the "Runway: ..." line; see Plan's runway parameter
	Border, Blanks             bool
	Title, Header              bool // Header is the Active/Strategy/Mode line

	// VisibleRows is an UPPER BOUND on how many account rows to render, not a
	// count of rows that are actually shown: at the scrolling rung the last
	// of them is spent on "+K more (j/k)" rather than an account row, and
	// below that rung it is clamped to the real row count so a renderer
	// slicing Rows[Top:Top+VisibleRows] never reads past the end.
	VisibleRows int

	TooNarrow, TooShort bool
}

// accountComfort, accountFloor and accountMax are ACCOUNT's three stops, named
// directly after the width ladder's own terms for them: a hard floor of 12
// (the narrowest ACCOUNT is ever squeezed to), a comfort minimum of 20 (the
// width it holds at while other columns are added back), and a maximum of 32
// (the width it is allowed to grow into once nothing else is left to reserve
// room for).
const (
	accountFloor   = 12
	accountComfort = 20
	accountMax     = 32
)

// windowFootprint, typeFootprint, autoFootprint and stateFootprint are each
// column's own reserved width -- its content plus the standard 2-column gap
// before the next column -- and planWidth subtracts them from fullAt to
// compute each rung's boundary directly, rather than restating the result as
// a literal that could drift from the arithmetic that produced it.
// WINDOW's 20 columns of content are the one explicit
// number in the material this was built from, sized to hold a
// 20-character scoped key; USED's 17 and 4 are the other two explicit
// numbers. TYPE, AUTO and STATE have no such source anywhere in that
// material -- their content widths (12, 4 and 13) are back-computed from the
// stated threshold boundaries themselves (91-77=14, 77-71=6, 71-56=15) rather
// than invented independently, and are only as trustworthy as that
// subtraction.
const (
	windowFootprint = 22 // 20 content + 2 gap
	typeFootprint   = 14 // 12 content + 2 gap
	autoFootprint   = 6  // 4 content + 2 gap
	stateFootprint  = 15 // 13 content + 2 gap

	// tierFootprint has no source at all, not even a back-computed one --
	// TIER appears in no width-ladder table this was built from. 8 (6
	// content, enough for "free"/"pro"/"team"-length values, plus the
	// standard 2-column gap) is a provisional estimate for SetList's own
	// ladder below, not a measured fact.
	tierFootprint = 8
)

// Plan reads nothing and calls nothing: it is a pure function of the six
// arguments, so the same (set, width, height, rows, notice, runway) always
// answers the same way and a test can walk every boundary without a terminal.
//
// notice is whether Snapshot.Notices is non-empty. Plan has no other way to
// find out: it takes no Snapshot, so a caller with a notice to show has to
// say so directly. A page with a notice needs one more row than the same
// page without one — the notice line itself — and that row is spent or
// reclaimed by the height ladder exactly like any other block.
//
// runway is whether there is a runway line to draw — the measured-burn summary,
// which exists only once enough readings have been recorded to give a rate, and
// is therefore absent on every machine for the first hours after it starts
// recording. It arrives as a bool for the reason notice does, and it costs the
// same one row. It is deliberately NOT folded into fixedRows below: that
// constant is the sum of every UNCONDITIONAL row, and raising it would shift
// every threshold on the height ladder for pages that draw no runway line at
// all.
//
// The defect this guards against was reproduced before this existed: with
// ACCOUNT computed as "whatever's left" independently at every width, going
// from 100 columns to 105 columns took ACCOUNT from 24 down to 12, because 105
// crossed the threshold that puts WINDOW back on the page and WINDOW's cost
// came out of the one flex column. Widening a terminal must never narrow an
// address. The fix implemented below is to hold ACCOUNT at its comfort width
// while columns are added back one at a time, and to only let it grow once
// every optional column for the current table is already on the page — so
// crossing a column threshold can never claim width ACCOUNT already had.
func Plan(set ColumnSet, width, height, rows int, notice, runway bool) Layout {
	var l Layout

	// 35x3 is the stated minimum viable size: below either dimension the page
	// says what it needs instead of rendering something unreadable.
	if width < 35 {
		l.TooNarrow = true
	}
	if height < 3 {
		l.TooShort = true
	}
	if l.TooNarrow || l.TooShort {
		return l
	}

	planWidth(&l, set, width)
	planHeight(&l, height, rows, notice, runway)

	return l
}

// planWidth is the width ladder: which columns survive, whether the gauge
// collapses, and how wide ACCOUNT is allowed to be.
//
// Below fullAt, ACCOUNT is held flat at accountComfort rather than growing
// with the terminal, even though there is room to spare: that slack is
// reserved for the next optional column's rung rather than spent, on purpose
// -- it is what makes the anti-narrowing invariant possible without knowing
// in advance which width will need the reservation. Only once every optional
// column for the current set is already on the page (width >= fullAt) does
// unused width finally reach ACCOUNT, growing it up to accountMax.
func planWidth(l *Layout, set ColumnSet, width int) {
	// fullAt is the width at which every optional column this set carries is
	// already on the page. Below it ACCOUNT is held at accountComfort; at and
	// above it, whatever width remains finally reaches ACCOUNT.
	fullAt := 113

	switch set {
	case SetList:
		// SetList's own width ladder. There is no normative source for it
		// anywhere in the material this was built from -- only SetStatus's
		// ladder is stated -- so this is built structurally parallel to that
		// one rather than out of invented numbers: the same three ACCOUNT
		// stops (accountFloor/accountComfort/accountMax), because it is the
		// same flex column under the same constraints, and a never-dropped
		// four that mirrors SetStatus's (IDX, ACCOUNT, USED, RESETS IN) with
		// LEFT standing in for USED -- LEFT is the answer this table exists
		// to give, the same way USED is SetStatus's.
		//
		// SetList carries six columns to SetStatus's eight, and only one of
		// them (TYPE) is optional: TIER is always shown. TYPE is the same
		// column with the same content as SetStatus's, so its cost is reused
		// rather than re-measured (typeFootprint, 14), and 77 is reused as
		// its own drop threshold for the same reason.
		l.Columns = []Column{ColIdx, ColAccount, ColType, ColTier, ColLeft, ColResets}
		fullAt = 77
		if width < 77 {
			l.Columns = withoutColumns(l.Columns, ColType)
		}
		// Using the provisional widths above (idx 4, tier 8, left 6, resets 9
		// as the last column with no trailing gap, plus accountFloor 12 and
		// the 2-column border): IDX + ACCOUNT + TIER + LEFT + RESETS IN comes
		// to 4+12+8+6+9+2 = 41 columns at the 35-column floor. That is short
		// by 6, not comfortably fitting the way SetStatus's narrower
		// four-survivor row does. This is stated here as a real finding, not
		// silently papered over: the 35-column floor was sized for
		// SetStatus's four, and nothing in this task establishes a
		// SetList-specific floor to replace it.

	default: // SetStatus
		l.Columns = []Column{ColIdx, ColAccount, ColType, ColUsed, ColWindow, ColResets, ColState, ColAuto}
		// Each rung's boundary is fullAt minus the running sum of the
		// footprints of everything already dropped, rather than a restated
		// literal, so the boundary and the footprint it is computed from
		// cannot drift apart the way a comment and a hand-copied number can.
		dropWindowAt := fullAt - windowFootprint   // 91
		dropTypeAt := dropWindowAt - typeFootprint // 77
		dropAutoAt := dropTypeAt - autoFootprint   // 71
		collapseAt := dropAutoAt - stateFootprint  // 56
		switch {
		case width >= fullAt:
			// fullAt is the full page's total width with every column shown
			// and the border included: the top rung, nothing dropped.
		case width >= dropWindowAt:
			// WINDOW (20 columns of content, sized to hold a 20-character
			// scoped key, plus its 2-column gap) no longer fits alongside
			// everything else below this rung.
			l.Columns = withoutColumns(l.Columns, ColWindow)
		case width >= dropTypeAt:
			// TYPE (12 content + 2 gap) additionally stops fitting.
			l.Columns = withoutColumns(l.Columns, ColWindow, ColType)
		case width >= dropAutoAt:
			// AUTO (4 content + 2 gap) additionally stops fitting.
			l.Columns = withoutColumns(l.Columns, ColWindow, ColType, ColAuto)
		default: // 35 up to dropAutoAt-1
			// Below dropAutoAt, STATE is dropped too, leaving the four
			// columns the width ladder never drops at any width: IDX,
			// ACCOUNT, USED and RESETS IN. collapseAt (STATE's footprint
			// below dropAutoAt) lands on the same number the table gives the
			// NEXT rung, even though that rung's actual effect is collapsing
			// the gauge rather than dropping STATE, which is already gone by
			// then.
			l.Columns = withoutColumns(l.Columns, ColWindow, ColType, ColAuto, ColState)
		}
		// Below collapseAt, the USED gauge's 17 columns ([#########.] plus
		// its percentage) no longer fit and it collapses to the bare
		// 4-column percentage instead. SetList has no gauge -- LEFT is
		// always the bare percentage -- so Collapsed is never set for it
		// and stays false.
		l.Collapsed = width < collapseAt
	}

	switch {
	case width < 43:
		// ACCOUNT itself is squeezed: accountFloor at 35, reaching
		// accountComfort at 43.
		l.AccountWide = accountFloor + (width - 35)
	case width < fullAt:
		l.AccountWide = accountComfort
	default:
		l.AccountWide = accountComfort + (width - fullAt)
		if l.AccountWide > accountMax {
			l.AccountWide = accountMax
		}
	}
}

// withoutColumns returns cols with every column in drop removed, preserving
// order. It never mutates cols.
func withoutColumns(cols []Column, drop ...Column) []Column {
	out := make([]Column, 0, len(cols))
	for _, c := range cols {
		dropped := false
		for _, d := range drop {
			if c == d {
				dropped = true
				break
			}
		}
		if !dropped {
			out = append(out, c)
		}
	}
	return out
}

// The height ladder's row budget: wordmark 5 rows, tagline 2 rows plus its
// blank, figures 6 rows plus its blank, the border 2 rows, the two remaining
// blank separators, the one-row title and the Active/Strategy/Mode line.
// Fixed rows total 22, independent of the account row count -- N of those is
// added on top, in planHeight, never folded into this constant. Neither the
// notice line nor the runway line is part of the fixed 22 either: each exists
// only when Plan is told it does, which is exactly what Plan's notice and
// runway parameters say.
const (
	fixedRows      = 22
	saveFigures    = 7 // 6 rows plus its blank
	saveNotice     = 1 // the "note: ..." line, when notice is true
	saveRunway     = 1 // the "Runway: ..." line, when runway is true
	saveTagline    = 3 // 2 rows plus its blank
	saveWordmark   = 4 // 5 rows down to titleLine's 1
	saveBorder     = 2
	saveBlanks     = 2
	saveTitle      = 1
	saveHeaderLine = 1 // the Active/Strategy/Mode line
)

// planHeight is the height ladder: which blocks survive, in the order they
// are dropped, and how many account rows are visible once nothing more can be
// dropped. The column header row, at least one account row and the footer are
// never dropped — dropping past them is what VisibleRows scrolling is for.
//
// The notice line sits between figures and tagline: it is dropped right
// after the figure block and before the tagline is even considered, so a
// tight terminal never has to carry both a figure and a notice.
//
// The runway line sits one rung below the notice, which decides what happens at
// the single height where exactly one of the two fits: the note gives and the
// runway line stays. Both cost one row and the ladder had to order them. The
// note is a report ABOUT the reading -- something could not be read -- and the
// runway line is the reading's conclusion, when the fleet stops working. A
// dashboard that dropped the conclusion to keep a caveat about it would be
// keeping the footnote and throwing away the sentence.
//
// Title and Wordmark can both be true at once, and that is not redundant:
// while Wordmark is true the version string rides on the wordmark's own last
// row for free (Fixture A draws "ccdad v0.2.0" directly after the glyph, on
// the same line), so Title costs nothing extra there. Title only becomes its
// own standalone row -- titleLine's row, actually costing the saveTitle rung
// -- once Wordmark is false. Title=false on its own therefore only ever
// happens after Wordmark is already false: it means the version string is
// gone from the page entirely, not that it moved.
func planHeight(l *Layout, height, rows int, notice, runway bool) {
	l.Wordmark, l.Tagline, l.Figures = true, true, true
	l.Notice = notice
	l.Runway = runway
	l.Border, l.Blanks = true, true
	l.Title, l.Header = true, true

	need := fixedRows + rows
	if notice {
		need += saveNotice
	}
	if runway {
		need += saveRunway
	}

	if need > height {
		need -= saveFigures
		l.Figures = false
	}
	if l.Notice && need > height {
		need -= saveNotice
		l.Notice = false
	}
	if l.Runway && need > height {
		need -= saveRunway
		l.Runway = false
	}
	if need > height {
		need -= saveTagline
		l.Tagline = false
	}
	if need > height {
		need -= saveWordmark
		l.Wordmark = false
	}
	if need > height {
		need -= saveBorder
		l.Border = false
	}
	if need > height {
		need -= saveBlanks
		l.Blanks = false
	}
	if need > height {
		need -= saveTitle
		l.Title = false
	}
	if need > height {
		need -= saveHeaderLine
		l.Header = false
	}

	// height-2 reserves one row for the column header and one for the
	// footer, whatever chrome above them survived. It is an upper bound, not
	// a target: at heights where the whole page fits comfortably, height-2
	// can be far larger than the real row count, and a renderer slicing
	// Rows[Top:Top+VisibleRows] on that unclamped number would read past the
	// end of the slice. Clamping to rows makes VisibleRows == rows whenever
	// everything already fits, and only less than that -- with the last line
	// spent on "+K more (j/k)" -- once scrolling genuinely starts.
	l.VisibleRows = height - 2
	if l.VisibleRows > rows {
		l.VisibleRows = rows
	}
}

// truncate cuts s to width display columns without ever wrapping it. A
// bordered lipgloss box soft-wraps content that is too wide for it instead of
// truncating it — forty characters inside a twenty-column frame render as
// five rows, not one — so every line handed to a frame is cut to size first.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(s, width, "")
}

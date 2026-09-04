package tui

import (
	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/view"
)

// ColumnSet is which of the two shipping tables the page is showing. [L] swaps
// between them.
//
// It used to be the polarity switch: USED and LEFT never shared a heading, so a
// page showing one could not show the other. Both columns are gone -- a row now
// carries a cell per WINDOW and derives nothing -- and what is left is a
// difference in what each surface KNOWS. The dashboard has engine state and an
// age beside every reading; `ccdad list` has neither, because view.Rows
// deliberately leaves Engine unfilled for it. SetFull is the dashboard's table
// and SetCompact is the listing's.
type ColumnSet int

const (
	SetFull    ColumnSet = iota // IDX ACCOUNT TYPE <windows> <resets> STATE AUTO
	SetCompact                  // IDX ACCOUNT TYPE TIER <windows> <resets>
)

// ColKind is what a column IS. It is separate from Column below because a
// window column cannot be named by a constant: how many there are, and which
// windows they stand for, is a fact about the fleet rather than about this
// package.
type ColKind int

const (
	ColIdx ColKind = iota
	ColAccount
	ColType
	ColTier
	// ColWindow is one window's cell, and Column.Win indexes
	// view.Columns.Windows.
	ColWindow
	// ColReset is one rollover's countdown, and Column.Win indexes
	// view.Columns.Resets.
	ColReset
	// ColWorst is the whole window block collapsed into one cell, for a
	// terminal too narrow to carry the block itself. It is what this ladder
	// does INSTEAD of dropping a window column.
	ColWorst
	ColState
	ColAuto
	ColAge
)

// Column is one rendered column: its kind, and for a window or a reset, which
// one.
//
// It is a comparable struct, so dropping a column is still `==` and the ladder
// below reads the way it did when every column had a constant of its own.
type Column struct {
	Kind ColKind
	Win  int
}

func col(k ColKind) Column   { return Column{Kind: k} }
func windowCol(i int) Column { return Column{Kind: ColWindow, Win: i} }
func resetCol(i int) Column  { return Column{Kind: ColReset, Win: i} }

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

// Each column's own reserved width: its content plus the standard 2-column gap
// before the next column. planWidth subtracts them from a full-page width to
// compute each rung's boundary directly, rather than restating the result as a
// literal that could drift from the arithmetic that produced it.
//
// TYPE, AUTO and STATE keep the content widths (12, 4 and 13) back-computed
// from the width ladder this was built on, and are only as trustworthy as that
// subtraction. The three that are new are measured against what they actually
// hold: a window cell is a percentage, "100%" at its widest, and its header is
// view.HeaderBudget; a reset cell is a HumanDuration, "1d16h" at its widest,
// under a header that is a window header plus " IN"; AGE is the same duration
// without the header.
const (
	typeFootprint  = 14 // 12 content + 2 gap
	autoFootprint  = 6  // 4 content + 2 gap
	stateFootprint = 15 // 13 content + 2 gap
	tierFootprint  = 8  // 6 content + 2 gap
	ageFootprint   = 8  // 6 content + 2 gap
	// worstFootprint holds "100% " plus a header of HeaderBudget, which is
	// what the collapsed block renders.
	worstFootprint = view.HeaderBudget + 7
)

// windowFootprint and resetFootprint are per COLUMN and depend on the header,
// which is the fleet's own and not this package's, so they are functions rather
// than constants.
//
// The content half is fixed and small -- a percentage is at most "100%" and a
// countdown at most "1d16h" -- so the header is what decides, which is why
// view.HeaderBudget exists at all.
func windowFootprint(header string) int {
	return maxInt(ansi.StringWidth(header), 4) + 2
}

func resetFootprint(header string) int {
	return maxInt(ansi.StringWidth(header), 5) + 2
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

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
func Plan(set ColumnSet, cols view.Columns, width, height, rows int, notice, runway bool) Layout {
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

	planWidth(&l, set, cols, width)
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
func planWidth(l *Layout, set ColumnSet, cols view.Columns, width int) {
	// The never-dropped four, and their absence from the drop order below IS
	// the argument: IDX and ACCOUNT say WHICH account, and every window column
	// says how much of one limit is gone. Dropping a window column would take a
	// limit off the page silently -- and the one most likely to matter is
	// exactly the one that is spent, because that is the row a reader came to
	// look at. The block collapses to ColWorst instead, which is safe where
	// dropping is not: with every cell reading percentage USED, the worst
	// window is the MAX, so nothing the collapsed cell hides is worse than what
	// it shows. A partial column set can make no such statement.
	full := []Column{col(ColIdx), col(ColAccount), col(ColType)}
	if set == SetCompact {
		full = append(full, col(ColTier))
	}
	for i := range cols.Windows {
		full = append(full, windowCol(i))
	}
	for i := range cols.Resets {
		full = append(full, resetCol(i))
	}
	if set == SetFull {
		full = append(full, col(ColState), col(ColAuto), col(ColAge))
	}

	// Drop order, lowest priority first. It is a fixed priority list walked
	// from the tail and NEVER a greedy packer: greedy is non-monotone, so a
	// column can vanish when the terminal WIDENS -- the same defect class the
	// ACCOUNT reservation below exists to prevent. The price is that at some
	// widths the page holds slack it cannot spend, and that is stated rather
	// than hidden.
	drops := []Column{}
	if set == SetCompact {
		drops = append(drops, col(ColTier))
	}
	if set == SetFull {
		drops = append(drops, col(ColAuto))
	}
	drops = append(drops, col(ColType))
	if set == SetFull {
		drops = append(drops, col(ColAge), col(ColState))
	}
	// Reset columns from the LAST plan-order one back, so the rollover a reader
	// is most likely to be waiting on -- the soonest, which sorts first -- is
	// the last to go.
	for i := len(cols.Resets) - 1; i >= 0; i-- {
		drops = append(drops, resetCol(i))
	}

	cost := func(c Column) int {
		switch c.Kind {
		case ColIdx:
			return 6
		case ColAccount:
			return accountComfort + 2
		case ColType:
			return typeFootprint
		case ColTier:
			return tierFootprint
		case ColWindow:
			return windowFootprint(cols.Windows[c.Win].Header)
		case ColReset:
			return resetFootprint(cols.Resets[c.Win].Header)
		case ColWorst:
			return worstFootprint
		case ColState:
			return stateFootprint
		case ColAuto:
			return autoFootprint
		case ColAge:
			return ageFootprint
		}
		return 0
	}
	total := func(cs []Column) int {
		n := 2 // the border
		for _, c := range cs {
			n += cost(c)
		}
		return n
	}

	// fullAt is the width at which every optional column is already on the
	// page. Below it ACCOUNT is held at accountComfort rather than growing with
	// the terminal, and that slack is RESERVED for the next rung rather than
	// spent: it is what makes "widening never removes a column, and never
	// narrows ACCOUNT" possible without knowing in advance which width needs
	// the reservation.
	fullAt := total(full)

	shown := full
	for _, d := range drops {
		if total(shown) <= width {
			break
		}
		shown = withoutColumns(shown, d)
	}
	// Still too wide with every optional column gone: collapse the whole window
	// block to one cell rather than take limits off the page one at a time.
	if total(shown) > width && len(cols.Windows) > 0 {
		var kept []Column
		placed := false
		for _, c := range shown {
			if c.Kind == ColWindow {
				if !placed {
					kept = append(kept, col(ColWorst))
					placed = true
				}
				continue
			}
			kept = append(kept, c)
		}
		shown = kept
	}
	l.Columns = shown
	l.Collapsed = len(cols.Windows) > 0 && !hasKind(shown, ColWindow)

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
	if l.AccountWide < accountFloor {
		l.AccountWide = accountFloor
	}
}

func hasKind(cs []Column, k ColKind) bool {
	for _, c := range cs {
		if c.Kind == k {
			return true
		}
	}
	return false
}

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

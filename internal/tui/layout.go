package tui

import (
	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/view"
)

// Layout is the answer to "what fits", computed once per frame from the
// terminal size and the row count, and read by nothing but the renderer: it
// carries no styling and touches no string.
//
// The columns are internal/view's, kind for kind and header for header. This
// package had its own vocabulary for them -- a ColKind enum, a Column struct
// and a switch per surface -- and every one of those was a second statement of
// something the shared list already says. What is left here is the arithmetic
// that vocabulary was for: how much room a column costs, and which ones a
// terminal has room for.
type Layout struct {
	Columns     []view.ListColumn
	AccountWide int
	Collapsed   bool // USED is the bare percentage, not the gauge

	Wordmark, Tagline, Figures bool
	Notice                     bool // the "note: ..." line; see Plan's notice parameter
	Runway                     bool // the "Runway: ..." line; see Plan's runway parameter
	Border, Blanks             bool
	Title, Header              bool // Header is the Active/Strategy/Current summary block
	// Trailer is the block printed UNDER the table -- the legend, the hover
	// sentence, the unranked note and the credit lines, which internal/view
	// hands over as one ordered slice. TrailerRows is how many lines that is.
	Trailer     bool
	TrailerRows int

	// VisibleRows is an UPPER BOUND on how many TABLE ROWS to render -- section
	// headings and account rows together -- and not a count of rows actually
	// shown: at the scrolling rung the last of them is spent on "+K more (j/k)"
	// rather than on another line of table, and below that rung it is clamped
	// to what the fleet can actually fill so a renderer slicing the display
	// list never reads past the end.
	//
	// It counts DISPLAY rows and not accounts, which is the one thing about it
	// that changed when the table gained its headings. The two counts differ by
	// a fixed sectionRows, and spending the budget in the unit the table is
	// drawn in is what keeps a heading from being written over the row below
	// the page's last line. What stays counted in ACCOUNTS is the "+K more"
	// figure and the cursor's room, because both are about accounts a reader
	// can move to and a heading is not one.
	VisibleRows int
	FooterRows  int
	RunwayRows  int

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

// footprint is what one column costs the page: its content, or its heading
// where that is wider, plus the standard 2-column gap before the next column.
// planWidth subtracts them from a full-page width to compute each rung's
// boundary directly, rather than restating the result as a literal that could
// drift from the arithmetic that produced it.
//
// BOTH NUMBERS ARE THE SHARED COLUMN'S OWN and neither is spelled here. This
// package used to hold a constant per column -- fourteen for TYPE, fifteen for
// STATE, and so on -- and each was the same reservation written a second time,
// so the day a heading grew, the ladder went on reserving the old width and the
// table drew past its own frame. A column now says how wide it is once, where
// it is defined, and this measures what it says.
func footprint(c view.ListColumn) int {
	return maxInt(ansi.StringWidth(c.Header), c.Content) + 2
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Plan reads nothing and calls nothing: it is a pure function of the six
// arguments, so the same (cols, width, height, rows, notice, runway) always
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
// every optional column is already on the page — so crossing a column
// threshold can never claim width ACCOUNT already had.
func Plan(cols view.Columns, width, height, rows int, notice, runway bool) Layout {
	runwayRows := 0
	if runway {
		runwayRows = 1
	}
	// Active and Strategy always exist. Model.Body supplies the exact count,
	// including an optional Codex active row and Current, through planWithRows.
	return planWithRows(cols, width, height, rows, notice, runway, 1, runwayRows, 2, 0)
}

// planWithRows extends Plan with the dynamic vertical blocks: the wrapped key
// bar, the one-line-per-fact runway summary, the one-line-per-fact status
// summary, and the trailer under the table.
//
// trailerRows is how many lines internal/view would print below the table, and
// zero says the page draws none. It is a COUNT and not a bool because every
// line of it is one row of the terminal, and the ladder spends rows.
func planWithRows(cols view.Columns, width, height, rows int,
	notice, runway bool, footerRows, runwayRows, summaryRows, trailerRows int) Layout {
	var l Layout
	l.FooterRows = 1
	if footerRows > 0 {
		l.FooterRows = footerRows
	}
	if runway {
		l.RunwayRows = 1
	}
	if runwayRows > 0 {
		l.RunwayRows = runwayRows
	}
	l.TrailerRows = trailerRows

	// 35 columns is the stated minimum viable width. Height also has to carry
	// the wrapped footer, the table heading and one account row.
	if width < 35 {
		l.TooNarrow = true
	}
	if height < l.FooterRows+2 {
		l.TooShort = true
	}
	if l.TooNarrow || l.TooShort {
		return l
	}

	planWidth(&l, cols, width)
	planHeight(&l, height, rows, summaryRows, notice, runway)

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
// column is already on the page (width >= fullAt) does unused width finally
// reach ACCOUNT, growing it up to accountMax.
func planWidth(l *Layout, cols view.Columns, width int) {
	// The fixed order is internal/view's, whole. TIER was held out of it while
	// the two surfaces were being moved onto the shared list, so that adopting
	// the list could be proved to move no page; it is in now, which is a change
	// to what the dashboard SHOWS and is why this commit moves every golden.
	//
	// TIER is also the first column ListDrops gives up, so the fleet-wide cost
	// is one rung rather than a column: below the full width it is not on the
	// page at all, and the widths where it is are the ones with room to spare.
	//
	// The never-dropped columns, and their absence from the drop order below IS
	// the argument: IDX and ACCOUNT say WHICH account, and every window column
	// says how much of one limit is gone. Dropping a window column would take a
	// limit off the page silently -- and the one most likely to matter is
	// exactly the one that is spent, because that is the row a reader came to
	// look at. The block collapses to one WORST cell instead, which is safe
	// where dropping is not: with every cell reading percentage USED, the worst
	// window is the MAX, so nothing the collapsed cell hides is worse than what
	// it shows. A partial column set can make no such statement.
	full := view.ListColumns(cols)

	// Drop order, lowest priority first, and internal/view's own. It is a fixed
	// priority list walked from the tail and NEVER a greedy packer: greedy is
	// non-monotone, so a column can vanish when the terminal WIDENS -- the same
	// defect class the ACCOUNT reservation below exists to prevent. The price
	// is that at some widths the page holds slack it cannot spend, and that is
	// stated rather than hidden.
	drops := view.ListDrops(cols)

	total := func(cs []view.ListColumn) int {
		n := 2 // the border
		for _, c := range cs {
			n += footprint(c)
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
	if total(shown) > width {
		shown = view.CollapseWindows(shown)
	}
	l.Columns = shown
	l.Collapsed = len(cols.Windows) > 0 && !hasKind(shown, view.ColumnWindow)

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

func hasKind(cs []view.ListColumn, k view.ColumnKind) bool {
	for _, c := range cs {
		if c.Kind == k {
			return true
		}
	}
	return false
}

// withoutColumns removes each dropped column by VALUE, which is what
// view.ListDrops publishing whole columns rather than kinds is for: a reset
// column is one of several and only the one the drop names may go.
func withoutColumns(cols []view.ListColumn, drop ...view.ListColumn) []view.ListColumn {
	out := make([]view.ListColumn, 0, len(cols))
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

// sectionRows is what the provider headings cost the page: one table row per
// section, and internal/view returns both sections whatever the fleet holds, so
// the number is a CONSTANT rather than a function of the accounts.
//
// That is the whole reason the headings can be budgeted at all. A count that
// moved with the fleet -- one heading on a Claude-only machine, two once a
// codex account is added -- would make the height ladder's rungs depend on
// which providers are logged in, and the page would lose its tagline on the day
// somebody ran `ccdad add codex`.
const sectionRows = 2

// The height ladder's row budget: wordmark 5 rows, tagline 2 rows plus its
// blank, figures 6 rows plus its blank, the border 2 rows, the two remaining
// blank separators, the one-row title and the table's column header. Fixed rows
// total 21, independent of the account row count and the summary block -- both
// are added on top in planHeight, never folded into this constant. Neither the
// notice line nor the runway line is part of the fixed 21 either: each exists
// only when Plan is told it does, which is exactly what Plan's notice and
// runway parameters say. The section headings are unconditional and could have
// been folded in, and are not: sectionRows above says what they are and why
// their count does not move, which a 23 here would hide.
const (
	fixedRows    = 21
	saveFigures  = 7 // 6 rows plus its blank
	saveNotice   = 1 // the "note: ..." line, when notice is true
	saveTagline  = 3 // 2 rows plus its blank
	saveWordmark = 4 // 5 rows down to titleLine's 1
	saveBorder   = 2
	saveBlanks   = 2
	saveTitle    = 1
)

// planHeight is the height ladder: which blocks survive, in the order they
// are dropped, and how many account rows are visible once nothing more can be
// dropped. The column header row, at least one account row and the footer are
// never dropped — dropping past them is what VisibleRows scrolling is for.
//
// The tagline is the first decorative block to go. The two blank separators
// go next, before the family art. With the multi-row summary and a complete key
// bar, that order keeps the family visible at the 80x24 design target while
// spending only vertical whitespace.
//
// The trailer goes directly after those three and ahead of the notice and the
// runway line, which is the highest a block carrying real information can sit.
// Every line of it explains a column whose HEADING is already on the page and
// readable without it -- the legend maps a heading back to its wire key, the
// unranked note says which of them nothing switches away from -- while the
// notice below it reports that something could not be READ, which changes what
// the figures in those columns mean. A page that kept the map and dropped the
// warning would be keeping the smaller answer.
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
func planHeight(l *Layout, height, rows, summaryRows int, notice, runway bool) {
	l.Wordmark, l.Tagline, l.Figures = true, true, true
	l.Notice = notice
	l.Runway = runway
	l.Trailer = l.TrailerRows > 0
	l.Border, l.Blanks = true, true
	l.Title, l.Header = true, true

	need := fixedRows + sectionRows + rows + summaryRows
	need += l.FooterRows - 1
	if notice {
		need += saveNotice
	}
	if runway {
		need += l.RunwayRows
	}
	if l.Trailer {
		need += l.TrailerRows
	}

	if need > height {
		need -= saveTagline
		l.Tagline = false
	}
	if need > height {
		need -= saveBlanks
		l.Blanks = false
	}
	if need > height {
		need -= saveFigures
		l.Figures = false
	}
	if l.Trailer && need > height {
		need -= l.TrailerRows
		l.Trailer = false
	}
	if l.Notice && need > height {
		need -= saveNotice
		l.Notice = false
	}
	if l.Runway && need > height {
		need -= l.RunwayRows
		l.Runway = false
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
		need -= saveTitle
		l.Title = false
	}
	if need > height {
		need -= summaryRows
		l.Header = false
	}

	// The subtraction reserves one row for the column header and every row in
	// the wrapped footer, whatever chrome above them survived, and every row of
	// a trailer that survived its own rung above. It is an upper bound, not
	// a target: at heights where the whole page fits comfortably, height-2
	// can be far larger than the table's real length, and a renderer slicing
	// the display list on that unclamped number would read past its end.
	// Clamping makes VisibleRows the whole table whenever everything already
	// fits, and only less than that -- with the last line spent on
	// "+K more (j/k)" -- once scrolling genuinely starts.
	//
	// The clamp is rows PLUS the headings, because that is how long the table
	// is: the ladder spends this budget in table rows and the table draws a
	// heading per section. Clamping to the account count alone would leave the
	// bottom two accounts of a fleet that fits comfortably reported as
	// scrolled off a page with room for them.
	//
	// The trailer has to be subtracted HERE as well as counted in need above,
	// and the two are not the same statement. need decides which blocks the
	// page can afford at all; this decides how much table is left once they
	// are drawn, and a trailer left out of it would be written over the bottom
	// of the table by rows the ladder had already promised room to.
	l.VisibleRows = height - 1 - l.FooterRows
	if l.Trailer {
		l.VisibleRows -= l.TrailerRows
	}
	if l.VisibleRows > rows+sectionRows {
		l.VisibleRows = rows + sectionRows
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

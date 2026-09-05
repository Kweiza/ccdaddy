package tui

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// The defect this exists to prevent, and it was reproduced before it was
// written: at 105 columns ACCOUNT was 12 and at 100 columns it was 24, because
// crossing the threshold that adds WINDOW back takes 22 columns from the one
// flex column. A user widening their terminal watched addresses get MORE
// truncated.
func TestWideningTheTerminalNeverNarrowsTheAccountColumn(t *testing.T) {
	prev := 0
	for w := 35; w <= 140; w++ {
		got := Plan(testCols(), w, 40, 4, false, false).AccountWide
		if got < prev {
			t.Fatalf("ACCOUNT went from %d columns at %d to %d columns at %d", prev, w-1, got, w)
		}
		prev = got
	}
}

// hasColumn asks by KIND, because a window column is not named by a constant:
// how many there are and which windows they stand for is a fact about the
// fleet, so a case that wants "is there a window column" has to ask that.
func hasColumn(cols []view.ListColumn, k view.ColumnKind) bool {
	for _, got := range cols {
		if got.Kind == k {
			return true
		}
	}
	return false
}

// The drop ORDER, asserted as an order rather than as a table of boundaries.
//
// The boundaries are no longer this package's to state: a window column's
// footprint is its header's, and the headers are the fleet's. A table of
// literals would encode one fixture's widths and go stale the first time an
// account carried a differently-named cap. What the ladder actually promises is
// an order, and that is what this walks.
//
// Read as: by the time TYPE is gone, AUTO and TIER already are. By the time the
// block collapses, every optional column already is.
//
// TIER leads the list, which is where the dashboard adopting it is visible as
// an order: it is on the page at the widths that have room for everything and
// is the first thing given up below them, because a plan name is a fact about
// the subscription rather than about today's fleet.
func TestTheWidthLadderDropsColumnsInTheOrderItSays(t *testing.T) {
	cols := testCols()
	// Lowest priority first, which is the order they must vanish in.
	order := []view.ColumnKind{view.ColumnTier, view.ColumnAuto, view.ColumnType, view.ColumnAge, view.ColumnState}

	// TIER is in the set at all, which the order above cannot say on its own:
	// a page that never drew it would satisfy every prefix claim below by
	// having dropped it at every width.
	if !hasColumn(Plan(cols, 200, 40, 4, false, false).Columns, view.ColumnTier) {
		t.Fatal("TIER is on no page at any width, so the order below is asserted of a column the dashboard does not draw")
	}

	for w := 35; w <= 200; w++ {
		l := Plan(cols, w, 40, 4, false, false)
		if l.TooNarrow {
			continue
		}
		// The set of dropped columns is always a PREFIX of the order, so once
		// a column is present every column after it must be too.
		seenPresent := false
		for _, k := range order {
			present := hasColumn(l.Columns, k)
			if seenPresent && !present {
				t.Fatalf("width %d: %s was dropped while something cheaper than it is still on the page: %+v",
					w, k, l.Columns)
			}
			if present {
				seenPresent = true
			}
		}
		// The block is the last thing to go, after every optional column.
		if !hasColumn(l.Columns, view.ColumnWindow) {
			for _, k := range order {
				if hasColumn(l.Columns, k) {
					t.Fatalf("width %d: the window block collapsed while %s is still on the page: %+v",
						w, k, l.Columns)
				}
			}
			if !l.Collapsed {
				t.Fatalf("width %d: the block is collapsed but Collapsed is false", w)
			}
		}
	}
}

// Widening never puts a column back and then takes it away again. A greedy
// packer does exactly that -- a cheap column fits at one width and an expensive
// one displaces it at the next -- which is why this ladder walks a fixed
// priority list instead.
func TestWideningNeverRemovesAColumn(t *testing.T) {
	cols := testCols()
	var prev []view.ListColumn
	for w := 35; w <= 200; w++ {
		l := Plan(cols, w, 40, 4, false, false)
		if l.TooNarrow {
			continue
		}
		for _, c := range prev {
			// The collapsed WORST cell is the exception, and its going is a
			// PROMOTION rather than a removal: the collapsed cell is replaced
			// by the whole window block, which is strictly more of the same
			// information. Every other column has to survive verbatim.
			if c.Kind == view.ColumnWorst {
				if !hasColumn(l.Columns, view.ColumnWorst) && !hasColumn(l.Columns, view.ColumnWindow) {
					t.Fatalf("width %d has neither the block nor its collapsed cell, and width %d had one", w, w-1)
				}
				continue
			}
			if !hasColumnExact(l.Columns, c) {
				t.Fatalf("width %d dropped %+v, which width %d showed", w, c, w-1)
			}
		}
		prev = l.Columns
	}
}

func hasColumnExact(cols []view.ListColumn, c view.ListColumn) bool {
	for _, got := range cols {
		if got == c {
			return true
		}
	}
	return false
}

// What is never dropped, at any width the page still renders at all: IDX is the
// hotkey, ACCOUNT identifies the row, and the QUOTA BLOCK is the question the
// dashboard exists to answer.
//
// The block is the part that changed. It used to be one derived column and
// could survive as itself; it is now one cell per window, and dropping any of
// them would take a limit off the page silently -- most likely the one that is
// spent, because that is the row a reader came to look at. So the ladder
// collapses the block to a single WORST cell instead, and the invariant is
// "there is always a quota cell", never "there are always N of them".
func TestTheQuotaBlockSurvivesEveryWidth(t *testing.T) {
	for w := 35; w <= 140; w++ {
		l := Plan(testCols(), w, 40, 4, false, false)
		for _, k := range []view.ColumnKind{view.ColumnIdx, view.ColumnAccount} {
			if !hasColumn(l.Columns, k) {
				t.Fatalf("width %d: %s is missing, and it must never be dropped", w, k)
			}
		}
		if !hasColumn(l.Columns, view.ColumnWindow) && !hasColumn(l.Columns, view.ColumnWorst) {
			t.Fatalf("width %d: the page carries no quota cell at all: %+v", w, l.Columns)
		}
	}
}

// The collapse is all-or-nothing. A page showing SOME of the windows would be
// the defect this change removes, rebuilt as a function of terminal width: a
// reader cannot tell a limit that is fine from one that was left out.
func TestTheWindowBlockIsNeverPartiallyShown(t *testing.T) {
	cols := testCols()
	for w := 35; w <= 140; w++ {
		l := Plan(cols, w, 40, 4, false, false)
		n := 0
		for _, c := range l.Columns {
			if c.Kind == view.ColumnWindow {
				n++
			}
		}
		if n != 0 && n != len(cols.Windows) {
			t.Fatalf("width %d: %d of %d window columns shown; the block is all or nothing",
				w, n, len(cols.Windows))
		}
	}
}

// The height ladder, rung by rung. Walked with a fixed 4 account rows, the
// tagline and then the blank separators give first, because between them they
// spend nothing but vertical whitespace.
//
// The section headings are a rung of their own now rather than a constant
// charged before the first rung fires, and this table is where that is visible
// as an ORDER: they go after the wordmark and the frame, which say nothing
// about any account, and before the title line and the summary block, which are
// the page's own facts about the fleet. Everything below their rung therefore
// fires two rows lower than it used to -- that is sectionRows handed back, and
// a rung that flipped the flag without giving the rows up would leave the title
// and the headings vanishing at the same height.
func TestTheHeightLadderDropsBlocksInTheOrderItSays(t *testing.T) {
	const rows = 4
	for _, tc := range []struct {
		height                                                              int
		wordmark, tagline, figures, border, blanks, sections, title, header bool
	}{
		{30, true, true, true, true, true, true, true, true},  // nothing dropped: 26+N
		{29, true, false, true, true, true, true, true, true}, // tagline dropped: 23+N
		{27, true, false, true, true, true, true, true, true},
		{26, true, false, true, true, false, true, true, true}, // blank separators dropped: 21+N
		{25, true, false, true, true, false, true, true, true},
		{24, true, false, false, true, false, true, true, true}, // figures also dropped: 14+N
		{18, true, false, false, true, false, true, true, true},
		{17, false, false, false, true, false, true, true, true}, // wordmark replaced: 10+N
		{14, false, false, false, true, false, true, true, true},
		{13, false, false, false, false, false, true, true, true}, // border dropped: 8+N
		{12, false, false, false, false, false, true, true, true},
		{11, false, false, false, false, false, false, true, true}, // sections dropped: 5+N
		{9, false, false, false, false, false, false, true, true},
		{8, false, false, false, false, false, false, false, true},  // title dropped: 4+N
		{7, false, false, false, false, false, false, false, false}, // summary dropped: 2+N
		{6, false, false, false, false, false, false, false, false},
	} {
		l := Plan(testCols(), 80, tc.height, rows, false, false)
		if l.Wordmark != tc.wordmark {
			t.Errorf("height %d: Wordmark=%v, want %v", tc.height, l.Wordmark, tc.wordmark)
		}
		if l.Tagline != tc.tagline {
			t.Errorf("height %d: Tagline=%v, want %v", tc.height, l.Tagline, tc.tagline)
		}
		if l.Figures != tc.figures {
			t.Errorf("height %d: Figures=%v, want %v", tc.height, l.Figures, tc.figures)
		}
		if l.Border != tc.border {
			t.Errorf("height %d: Border=%v, want %v", tc.height, l.Border, tc.border)
		}
		if l.Blanks != tc.blanks {
			t.Errorf("height %d: Blanks=%v, want %v", tc.height, l.Blanks, tc.blanks)
		}
		if l.Sections != tc.sections {
			t.Errorf("height %d: Sections=%v, want %v", tc.height, l.Sections, tc.sections)
		}
		if l.Title != tc.title {
			t.Errorf("height %d: Title=%v, want %v", tc.height, l.Title, tc.title)
		}
		if l.Header != tc.header {
			t.Errorf("height %d: Header=%v, want %v", tc.height, l.Header, tc.header)
		}
	}

	// Never dropped: the column header row, at least one account row, and the
	// footer. Below all of that, rows scroll instead: VisibleRows = height-2.
	for _, height := range []int{5, 4, 3} {
		l := Plan(testCols(), 80, height, rows, false, false)
		if l.TooShort {
			t.Fatalf("height %d tripped the short floor; the floor is below 3", height)
		}
		if want := height - 2; l.VisibleRows != want {
			t.Errorf("height %d: VisibleRows=%d, want %d", height, l.VisibleRows, want)
		}
	}
}

// The notice gives after the decorative blocks. A Snapshot with a notice
// needs one more row than the same Snapshot without one, and Plan has no way to
// know that except being told — this walks every boundary where that extra row
// changes which block survives.
func TestTheNoticeRungFollowsTheDecorativeBlocks(t *testing.T) {
	const rows = 4

	// 29 fits everything when there is no notice to show.
	without := Plan(testCols(), 80, 30, rows, false, false)
	if !without.Figures || without.Notice || !without.Tagline {
		t.Fatalf("80x30 without a notice: Figures=%v Notice=%v Tagline=%v, want true/false/true",
			without.Figures, without.Notice, without.Tagline)
	}

	// The same 29 rows no longer fit everything once notice=true shifts the
	// budget up by one: the tagline gives first and the family remains.
	with := Plan(testCols(), 80, 30, rows, true, false)
	if !with.Figures {
		t.Fatal("80x30 with a notice dropped the figure block before the tagline")
	}
	if !with.Notice {
		t.Fatal("80x30 with a notice dropped the notice line before either decorative block was gone")
	}
	if with.Tagline {
		t.Fatal("80x30 with a notice kept the tagline instead of the family")
	}

	// At 25 the family still fits after the blank separators give; at 24 it
	// gives and the notice remains.
	if l := Plan(testCols(), 80, 26, rows, true, false); !l.Figures || !l.Notice || l.Tagline {
		t.Fatalf("80x26 with a notice: Figures=%v Notice=%v Tagline=%v, want true/true/false",
			l.Figures, l.Notice, l.Tagline)
	}
	if l := Plan(testCols(), 80, 24, rows, true, false); l.Figures || !l.Notice || l.Tagline {
		t.Fatalf("80x24 with a notice: Figures=%v Notice=%v Tagline=%v, want false/true/false",
			l.Figures, l.Notice, l.Tagline)
	}

	// At 17, the notice line gives next. Both cases converge once it is gone:
	// the plain page was never carrying one, and the page with a notice just dropped
	// it, so the two arrive at the identical visible Layout.
	notice17 := Plan(testCols(), 80, 17, rows, true, false)
	plain17 := Plan(testCols(), 80, 17, rows, false, false)
	if notice17.Figures != plain17.Figures || notice17.Notice != plain17.Notice || notice17.Tagline != plain17.Tagline {
		t.Fatalf("80x17 did not converge: with notice=%+v without=%+v",
			struct{ Figures, Notice, Tagline bool }{notice17.Figures, notice17.Notice, notice17.Tagline},
			struct{ Figures, Notice, Tagline bool }{plain17.Figures, plain17.Notice, plain17.Tagline})
	}

	// Further down the ladder the notice line stays dropped, same as any
	// other block once its rung has fired.
	if l := Plan(testCols(), 80, 16, rows, true, false); l.Notice {
		t.Fatal("80x16 with a notice put the notice line back")
	}
}

// The floors. Below them the page says what it needs rather than rendering
// something unreadable.
func TestBelowTheFloorsThePageSaysWhatItNeeds(t *testing.T) {
	if l := Plan(testCols(), 34, 40, 4, false, false); !l.TooNarrow {
		t.Error("34 columns did not trip the narrow floor")
	}
	if l := Plan(testCols(), 80, 2, 4, false, false); !l.TooShort {
		t.Error("2 rows did not trip the short floor")
	}
}

// The runway rung sits directly below the notice's and after the decorative
// blocks. A
// page with runway rows needs four more rows than the same page without them,
// Plan has no way to find that out except being told.
//
// The row where the two meet is the interesting one. At 20 rows exactly one of
// them fits, and this pins which: the note is what gives. A dashboard that
// dropped the conclusion to keep a caveat about it would be keeping the
// footnote and throwing away the sentence.
func TestTheRunwayRungFollowsTheDecorativeBlocksAndNotice(t *testing.T) {
	const rows = 4

	const runwayRows = 4

	// 29 fits everything when there are no runway rows to show.
	without := Plan(testCols(), 80, 30, rows, false, false)
	if !without.Figures || without.Runway || !without.Tagline {
		t.Fatalf("80x30 without a runway line: Figures=%v Runway=%v Tagline=%v, want true/false/true",
			without.Figures, without.Runway, without.Tagline)
	}

	// Four runway rows spend the tagline and blank separators before any runway
	// fact or the family art is lost.
	with := planWithRows(testCols(), 80, 30, rows, false, true, 1, runwayRows, 2, 0)
	if !with.Figures {
		t.Fatal("80x30 with runway rows dropped the family after whitespace had already made enough room")
	}
	if !with.Runway {
		t.Fatal("80x30 with runway rows dropped them before either decorative block was gone")
	}
	if with.Tagline {
		t.Fatal("80x30 with runway rows kept the tagline instead of spending it first")
	}

	// Down to 21, all runway rows still fit after every decorative block is gone.
	if l := planWithRows(testCols(), 80, 22, rows, false, true, 1, runwayRows, 2, 0); l.Figures || !l.Runway || l.Tagline {
		t.Fatalf("80x22 with runway rows: Figures=%v Runway=%v Tagline=%v, want false/true/false",
			l.Figures, l.Runway, l.Tagline)
	}

	// At 20 the runway block gives next.
	if l := planWithRows(testCols(), 80, 20, rows, false, true, 1, runwayRows, 2, 0); l.Figures || l.Runway || l.Tagline {
		t.Fatalf("80x20 with runway rows: Figures=%v Runway=%v Tagline=%v, want false/false/false",
			l.Figures, l.Runway, l.Tagline)
	}

	// The notice adds a fifth conditional row. At 23 both blocks fit after the
	// decorative blocks are gone; at 22 the notice gives and runway remains.
	if l := planWithRows(testCols(), 80, 23, rows, true, true, 1, runwayRows, 2, 0); !l.Notice || !l.Runway {
		t.Fatalf("80x23 with both blocks: Notice=%v Runway=%v, want true/true", l.Notice, l.Runway)
	}
	if l := planWithRows(testCols(), 80, 22, rows, true, true, 1, runwayRows, 2, 0); l.Notice || !l.Runway {
		t.Fatalf("80x22 with both blocks: Notice=%v Runway=%v, want false/true — the note gives before runway does",
			l.Notice, l.Runway)
	}

	// A page with no runway line never plans one, at any rung of the ladder.
	for _, h := range []int{28, 23, 22, 21, 14, 6, 3} {
		if l := Plan(testCols(), 80, h, rows, true, false); l.Runway {
			t.Errorf("height %d planned a runway line for a page that has none", h)
		}
	}
}

// The trailer's two places on the height ladder.
//
// It is asked about HERE rather than through a page because a rendered page can
// only ever exercise the trailer its own fixture pool produces: Model.Body
// passes the length internal/view hands it, which for that pool is one line.
// Three lines is what makes the arithmetic visible, and an arithmetic claim
// that only ever sees the number one is a claim nobody has checked.
//
// Four claims, and the first is what every golden page under testdata rests
// on.
func TestATrailerCostsItsRowsAndIsGivenUpBeforeTheNotice(t *testing.T) {
	const (
		rows        = 4
		trailerRows = 3
	)

	// A trailer of no lines is not a block: nothing is reserved for it, no rung
	// fires for it, and the page is the page it was before there was a trailer
	// at all.
	for h := 4; h <= 40; h++ {
		if l := Plan(testCols(), 80, h, rows, true, false); l.Trailer || l.TrailerRows != 0 {
			t.Fatalf("at 80x%d a page with no trailer planned one: Trailer=%v TrailerRows=%d",
				h, l.Trailer, l.TrailerRows)
		}
	}

	// A trailer that IS drawn costs exactly its own rows, and the way to see
	// that from outside is a block below it on the ladder: the tagline is given
	// up at a page three rows taller.
	taglineGoesAt := func(trailer int) int {
		for h := 40; h >= 4; h-- {
			if !planWithRows(testCols(), 80, h, rows, true, false, 1, 0, 2, trailer).Tagline {
				return h
			}
		}
		return 0
	}
	if got, want := taglineGoesAt(trailerRows), taglineGoesAt(0)+trailerRows; got != want {
		t.Errorf("with a %d-line trailer the tagline is spent at %d rows, want %d",
			trailerRows, got, want)
	}

	// The trailer goes before the notice, and while it is on the page the table
	// is not scrolling. The second half is why a trailer can never be drawn
	// over an account row: its rung sits high enough that a page keeping one
	// still has room for every row it was asked about, so the reservation
	// VisibleRows makes for it is arithmetic that agrees rather than a row
	// taken from the table.
	sawTheDrop := false
	for h := 4; h <= 40; h++ {
		l := planWithRows(testCols(), 80, h, rows, true, false, 1, 0, 2, trailerRows)
		if l.TooShort {
			continue
		}
		if l.Trailer && !l.Notice {
			t.Errorf("at 80x%d the notice was given up while the trailer above it stayed", h)
		}
		if l.Trailer && l.VisibleRows != rows+sectionRows {
			t.Errorf("at 80x%d a trailer is planned over a scrolling table: %d of %d table rows visible",
				h, l.VisibleRows, rows+sectionRows)
		}
		if !l.Trailer && l.Notice {
			sawTheDrop = true
		}
	}
	if !sawTheDrop {
		t.Fatal("no height between 4 and 40 drops the trailer and keeps the notice, so the order above is asserted of nothing")
	}

	// And it goes AFTER the whitespace and BEFORE the family art, which is the
	// other half of the order and the half that was reversed. The claim above
	// bounds the trailer from below and these bound it from above: without them,
	// a ladder that spent the trailer before the tagline would still drop it
	// before the notice and would still pass.
	//
	// The witness is a height that has spent the legend and still draws the
	// drawing. Every line the trailer holds is printed in full by `ccdad status`
	// at any width; the art is on no other command at all, so the block that can
	// be read elsewhere is the one given up first.
	sawTheArtOutliveIt := false
	for h := 4; h <= 40; h++ {
		l := planWithRows(testCols(), 80, h, rows, true, false, 1, 0, 2, trailerRows)
		if l.TooShort {
			continue
		}
		if l.Trailer && !l.Figures {
			t.Errorf("at 80x%d the family art was given up while the trailer, which goes first, stayed", h)
		}
		if !l.Trailer && (l.Tagline || l.Blanks) {
			t.Errorf("at 80x%d the trailer was given up while whitespace below it stayed: "+
				"Tagline=%v Blanks=%v", h, l.Tagline, l.Blanks)
		}
		if !l.Trailer && l.Figures {
			sawTheArtOutliveIt = true
		}
	}
	if !sawTheArtOutliveIt {
		t.Fatal("no height between 4 and 40 spends the trailer and keeps the family art, so the order above is asserted of nothing")
	}
}

// The section headings' rung, from both sides, and what it hands back.
//
// The order first. They go AFTER the wordmark and the frame, which say nothing
// about any account, and BEFORE the title line and the summary block, which are
// what the page knows about the fleet: a short terminal keeps the facts and
// loses the grouping. Both bounds are walked rather than sampled, because a
// rung placed one line either side of where it belongs still passes at most
// heights.
//
// Then the arithmetic, and it is the half a boolean cannot show. The title's
// rung fires exactly sectionRows below the headings' own, which is only true if
// the rung SUBTRACTS what it gives up: a rung that flipped Sections and left
// need alone would take the title away at the same height as the headings, and
// every page under it would be planned two rows taller than it is drawn.
//
// And VisibleRows follows, which is the same fact at the other end of the
// function. It is an upper bound on TABLE rows, and the table is two rows
// shorter once the headings are gone -- a bound that went on counting them
// would hand a renderer two lines the list cannot fill.
func TestTheSectionHeadingsAreGivenUpBeforeTheTitleAndTheSummary(t *testing.T) {
	const rows = 4

	sectionsGoAt, titleGoesAt := 0, 0
	for h := 40; h >= 4; h-- {
		l := Plan(testCols(), 80, h, rows, false, false)
		if l.TooShort {
			continue
		}
		if !l.Sections && (l.Wordmark || l.Border) {
			t.Errorf("at 80x%d the headings were given up while the chrome above them stayed: Wordmark=%v Border=%v",
				h, l.Wordmark, l.Border)
		}
		if l.Sections && (!l.Title || !l.Header) {
			t.Errorf("at 80x%d the page kept its headings and gave up a fact under them: Title=%v Header=%v",
				h, l.Title, l.Header)
		}
		if sectionsGoAt == 0 && !l.Sections {
			sectionsGoAt = h
		}
		if titleGoesAt == 0 && !l.Title {
			titleGoesAt = h
		}
		want := rows
		if l.Sections {
			want += sectionRows
		}
		if l.VisibleRows > want {
			t.Errorf("at 80x%d VisibleRows is %d, more than the %d rows the table draws", h, l.VisibleRows, want)
		}
	}
	if sectionsGoAt == 0 || titleGoesAt == 0 {
		t.Fatalf("no height between 4 and 40 gives up the headings (%d) or the title (%d), so the order above is asserted of nothing",
			sectionsGoAt, titleGoesAt)
	}
	// The rung hands back sectionRows LESS headerRows, because a page without
	// sections still draws one row of column names -- the sections and that row
	// are alternatives, and only the difference is freed.
	if want := sectionsGoAt - (sectionRows - headerRows); titleGoesAt != want {
		t.Errorf("the headings go at %d rows and the title at %d, want %d -- the rung did not hand its %d rows back",
			sectionsGoAt, titleGoesAt, want, sectionRows-headerRows)
	}
}

// testCols is the quota block every layout case is planned against: the shape a
// Claude fleet actually carries — a five-hour window, an all-model weekly, and
// one model-scoped cap sharing the weekly's rollover.
func testCols() view.Columns {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	win := func(pct float64, d time.Duration) usage.Window {
		at := now.Add(d)
		return usage.NewWindow(&pct, &at)
	}
	at := now.Add(40 * time.Hour)
	p := 100.0
	s := &usage.Snapshot{
		FiveHour: win(20, 2*time.Hour),
		SevenDay: win(62, 40*time.Hour),
		Limits: []usage.Limit{usage.LimitFor(usage.LimitInput{
			Kind: "weekly_scoped", Model: "Fable", Percent: &p, ResetsAt: &at,
		})},
	}
	return view.ColumnsOf([]view.Row{{HasEntry: true, Entry: usage.Entry{Snapshot: s}}})
}

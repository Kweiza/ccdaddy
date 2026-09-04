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
		got := Plan(SetFull, testCols(), w, 40, 4, false, false).AccountWide
		if got < prev {
			t.Fatalf("ACCOUNT went from %d columns at %d to %d columns at %d", prev, w-1, got, w)
		}
		prev = got
	}
}

// The same anti-narrowing invariant, for SetCompact. It carries the identical
// accountFloor/accountComfort/accountMax constants and the identical
// hold-then-grow mechanism, keyed to its own fullAt (77, where TYPE -- its
// one optional column -- is fully back on the page) rather than SetFull's
// 113, so this is not redundant with the test above: it is the same claim
// checked against a different fullAt.
func TestWideningTheTerminalNeverNarrowsTheAccountColumnForSetCompactToo(t *testing.T) {
	prev := 0
	for w := 35; w <= 140; w++ {
		got := Plan(SetCompact, testCols(), w, 40, 4, false, false).AccountWide
		if got < prev {
			t.Fatalf("SetCompact: ACCOUNT went from %d columns at %d to %d columns at %d", prev, w-1, got, w)
		}
		prev = got
	}
}

// hasColumn asks by KIND, because a window column is not named by a constant:
// how many there are and which windows they stand for is a fact about the
// fleet, so a case that wants "is there a window column" has to ask that.
func hasColumn(cols []Column, k ColKind) bool {
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
func TestTheWidthLadderDropsColumnsInTheOrderItSays(t *testing.T) {
	cols := testCols()
	// Lowest priority first, which is the order they must vanish in.
	order := []ColKind{ColAuto, ColType, ColAge, ColState}

	for w := 35; w <= 200; w++ {
		l := Plan(SetFull, cols, w, 40, 4, false, false)
		if l.TooNarrow {
			continue
		}
		// The set of dropped columns is always a PREFIX of the order, so once
		// a column is present every column after it must be too.
		seenPresent := false
		for _, k := range order {
			present := hasColumn(l.Columns, k)
			if seenPresent && !present {
				t.Fatalf("width %d: kind %d was dropped while something cheaper than it is still on the page: %+v",
					w, k, l.Columns)
			}
			if present {
				seenPresent = true
			}
		}
		// The block is the last thing to go, after every optional column.
		if !hasColumn(l.Columns, ColWindow) {
			for _, k := range order {
				if hasColumn(l.Columns, k) {
					t.Fatalf("width %d: the window block collapsed while kind %d is still on the page: %+v",
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
	var prev []Column
	for w := 35; w <= 200; w++ {
		l := Plan(SetFull, cols, w, 40, 4, false, false)
		if l.TooNarrow {
			continue
		}
		for _, c := range prev {
			// ColWorst is the exception, and it is a PROMOTION rather than a
			// removal: the collapsed cell is replaced by the whole window
			// block, which is strictly more of the same information. Every
			// other column has to survive verbatim.
			if c.Kind == ColWorst {
				if !hasColumn(l.Columns, ColWorst) && !hasColumn(l.Columns, ColWindow) {
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

func hasColumnExact(cols []Column, c Column) bool {
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
	for _, set := range []ColumnSet{SetFull, SetCompact} {
		for w := 35; w <= 140; w++ {
			l := Plan(set, testCols(), w, 40, 4, false, false)
			for _, k := range []ColKind{ColIdx, ColAccount} {
				if !hasColumn(l.Columns, k) {
					t.Fatalf("set %d width %d: kind %d is missing, and it must never be dropped", set, w, k)
				}
			}
			if !hasColumn(l.Columns, ColWindow) && !hasColumn(l.Columns, ColWorst) {
				t.Fatalf("set %d width %d: the page carries no quota cell at all: %+v", set, w, l.Columns)
			}
		}
	}
}

// The collapse is all-or-nothing. A page showing SOME of the windows would be
// the defect this change removes, rebuilt as a function of terminal width: a
// reader cannot tell a limit that is fine from one that was left out.
func TestTheWindowBlockIsNeverPartiallyShown(t *testing.T) {
	cols := testCols()
	for w := 35; w <= 140; w++ {
		l := Plan(SetFull, cols, w, 40, 4, false, false)
		n := 0
		for _, c := range l.Columns {
			if c.Kind == ColWindow {
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
// tagline and then the blank separators give first so the family remains
// visible at the 80x24 design target after the summary gained its own rows.
func TestTheHeightLadderDropsBlocksInTheOrderItSays(t *testing.T) {
	const rows = 4
	for _, tc := range []struct {
		height                                                    int
		wordmark, tagline, figures, border, blanks, title, header bool
	}{
		{27, true, true, true, true, true, true, true},  // nothing dropped: 23+N
		{26, true, false, true, true, true, true, true}, // tagline dropped: 20+N
		{24, true, false, true, true, true, true, true},
		{23, true, false, true, true, false, true, true}, // blank separators dropped: 18+N
		{22, true, false, true, true, false, true, true},
		{21, true, false, false, true, false, true, true}, // figures also dropped: 11+N
		{15, true, false, false, true, false, true, true},
		{14, false, false, false, true, false, true, true}, // wordmark replaced: 7+N
		{11, false, false, false, true, false, true, true},
		{10, false, false, false, false, false, true, true}, // border dropped: 5+N
		{9, false, false, false, false, false, true, true},
		{8, false, false, false, false, false, false, true},  // title dropped: 4+N
		{7, false, false, false, false, false, false, false}, // summary dropped: 2+N
		{6, false, false, false, false, false, false, false},
	} {
		l := Plan(SetFull, testCols(), 80, tc.height, rows, false, false)
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
		l := Plan(SetFull, testCols(), 80, height, rows, false, false)
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

	// 27 fits everything when there is no notice to show.
	without := Plan(SetFull, testCols(), 80, 27, rows, false, false)
	if !without.Figures || without.Notice || !without.Tagline {
		t.Fatalf("80x27 without a notice: Figures=%v Notice=%v Tagline=%v, want true/false/true",
			without.Figures, without.Notice, without.Tagline)
	}

	// The same 27 rows no longer fit everything once notice=true shifts the
	// budget up by one: the tagline gives first and the family remains.
	with := Plan(SetFull, testCols(), 80, 27, rows, true, false)
	if !with.Figures {
		t.Fatal("80x27 with a notice dropped the figure block before the tagline")
	}
	if !with.Notice {
		t.Fatal("80x27 with a notice dropped the notice line before either decorative block was gone")
	}
	if with.Tagline {
		t.Fatal("80x27 with a notice kept the tagline instead of the family")
	}

	// At 23 the family still fits after the blank separators give; at 22 it
	// gives and the notice remains.
	if l := Plan(SetFull, testCols(), 80, 23, rows, true, false); !l.Figures || !l.Notice || l.Tagline {
		t.Fatalf("80x23 with a notice: Figures=%v Notice=%v Tagline=%v, want true/true/false",
			l.Figures, l.Notice, l.Tagline)
	}
	if l := Plan(SetFull, testCols(), 80, 22, rows, true, false); l.Figures || !l.Notice || l.Tagline {
		t.Fatalf("80x22 with a notice: Figures=%v Notice=%v Tagline=%v, want false/true/false",
			l.Figures, l.Notice, l.Tagline)
	}

	// At 15, the notice line gives next. Both cases converge once it is gone:
	// the plain page was never carrying one, and the page with a notice just dropped
	// it, so the two arrive at the identical visible Layout.
	notice15 := Plan(SetFull, testCols(), 80, 15, rows, true, false)
	plain15 := Plan(SetFull, testCols(), 80, 15, rows, false, false)
	if notice15.Figures != plain15.Figures || notice15.Notice != plain15.Notice || notice15.Tagline != plain15.Tagline {
		t.Fatalf("80x15 did not converge: with notice=%+v without=%+v",
			struct{ Figures, Notice, Tagline bool }{notice15.Figures, notice15.Notice, notice15.Tagline},
			struct{ Figures, Notice, Tagline bool }{plain15.Figures, plain15.Notice, plain15.Tagline})
	}

	// Further down the ladder the notice line stays dropped, same as any
	// other block once its rung has fired.
	if l := Plan(SetFull, testCols(), 80, 14, rows, true, false); l.Notice {
		t.Fatal("80x14 with a notice put the notice line back")
	}
}

// The floors. Below them the page says what it needs rather than rendering
// something unreadable.
func TestBelowTheFloorsThePageSaysWhatItNeeds(t *testing.T) {
	if l := Plan(SetFull, testCols(), 34, 40, 4, false, false); !l.TooNarrow {
		t.Error("34 columns did not trip the narrow floor")
	}
	if l := Plan(SetFull, testCols(), 80, 2, 4, false, false); !l.TooShort {
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

	// 27 fits everything when there are no runway rows to show.
	without := Plan(SetFull, testCols(), 80, 27, rows, false, false)
	if !without.Figures || without.Runway || !without.Tagline {
		t.Fatalf("80x27 without a runway line: Figures=%v Runway=%v Tagline=%v, want true/false/true",
			without.Figures, without.Runway, without.Tagline)
	}

	// Four runway rows spend the tagline and blank separators before any runway
	// fact or the family art is lost.
	with := planWithRows(SetFull, testCols(), 80, 27, rows, false, true, 1, runwayRows, 2)
	if !with.Figures {
		t.Fatal("80x27 with runway rows dropped the family after whitespace had already made enough room")
	}
	if !with.Runway {
		t.Fatal("80x27 with runway rows dropped them before either decorative block was gone")
	}
	if with.Tagline {
		t.Fatal("80x27 with runway rows kept the tagline instead of spending it first")
	}

	// Down to 19, all runway rows still fit after every decorative block is gone.
	if l := planWithRows(SetFull, testCols(), 80, 19, rows, false, true, 1, runwayRows, 2); l.Figures || !l.Runway || l.Tagline {
		t.Fatalf("80x19 with runway rows: Figures=%v Runway=%v Tagline=%v, want false/true/false",
			l.Figures, l.Runway, l.Tagline)
	}

	// At 18 the runway block gives next.
	if l := planWithRows(SetFull, testCols(), 80, 18, rows, false, true, 1, runwayRows, 2); l.Figures || l.Runway || l.Tagline {
		t.Fatalf("80x18 with runway rows: Figures=%v Runway=%v Tagline=%v, want false/false/false",
			l.Figures, l.Runway, l.Tagline)
	}

	// The notice adds a fifth conditional row. At 20 both blocks fit after the
	// decorative blocks are gone; at 19 the notice gives and runway remains.
	if l := planWithRows(SetFull, testCols(), 80, 20, rows, true, true, 1, runwayRows, 2); !l.Notice || !l.Runway {
		t.Fatalf("80x20 with both blocks: Notice=%v Runway=%v, want true/true", l.Notice, l.Runway)
	}
	if l := planWithRows(SetFull, testCols(), 80, 19, rows, true, true, 1, runwayRows, 2); l.Notice || !l.Runway {
		t.Fatalf("80x19 with both blocks: Notice=%v Runway=%v, want false/true — the note gives before runway does",
			l.Notice, l.Runway)
	}

	// A page with no runway line never plans one, at any rung of the ladder.
	for _, h := range []int{26, 21, 20, 19, 12, 6, 3} {
		if l := Plan(SetFull, testCols(), 80, h, rows, true, false); l.Runway {
			t.Errorf("height %d planned a runway line for a page that has none", h)
		}
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

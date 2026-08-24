package tui

import "testing"

// The defect this exists to prevent, and it was reproduced before it was
// written: at 105 columns ACCOUNT was 12 and at 100 columns it was 24, because
// crossing the threshold that adds WINDOW back takes 22 columns from the one
// flex column. A user widening their terminal watched addresses get MORE
// truncated.
func TestWideningTheTerminalNeverNarrowsTheAccountColumn(t *testing.T) {
	prev := 0
	for w := 35; w <= 140; w++ {
		got := Plan(SetStatus, w, 40, 4, false).AccountWide
		if got < prev {
			t.Fatalf("ACCOUNT went from %d columns at %d to %d columns at %d", prev, w-1, got, w)
		}
		prev = got
	}
}

// The same anti-narrowing invariant, for SetList. It carries the identical
// accountFloor/accountComfort/accountMax constants and the identical
// hold-then-grow mechanism, keyed to its own fullAt (77, where TYPE -- its
// one optional column -- is fully back on the page) rather than SetStatus's
// 113, so this is not redundant with the test above: it is the same claim
// checked against a different fullAt.
func TestWideningTheTerminalNeverNarrowsTheAccountColumnForSetListToo(t *testing.T) {
	prev := 0
	for w := 35; w <= 140; w++ {
		got := Plan(SetList, w, 40, 4, false).AccountWide
		if got < prev {
			t.Fatalf("SetList: ACCOUNT went from %d columns at %d to %d columns at %d", prev, w-1, got, w)
		}
		prev = got
	}
}

func hasColumn(cols []Column, c Column) bool {
	for _, got := range cols {
		if got == c {
			return true
		}
	}
	return false
}

// Every rung of the width ladder, at its own boundary. A ladder written as a
// chain of >= comparisons is off by one somewhere, and the only way to find out
// which one is to walk them.
func TestTheWidthLadderDropsColumnsInTheOrderItSays(t *testing.T) {
	for _, tc := range []struct {
		width                               int
		window, typ, auto, state, collapsed bool
	}{
		// >= 113: all eight.
		{140, true, true, true, true, false},
		{113, true, true, true, true, false},
		// 91-112: drop WINDOW.
		{112, false, true, true, true, false},
		{91, false, true, true, true, false},
		// 77-90: also drop TYPE.
		{90, false, false, true, true, false},
		{77, false, false, true, true, false},
		// 71-76: also drop AUTO.
		{76, false, false, false, true, false},
		{71, false, false, false, true, false},
		// 56-70: also drop STATE.
		{70, false, false, false, false, false},
		{56, false, false, false, false, false},
		// 43-55: same four columns, gauge collapsed.
		{55, false, false, false, false, true},
		{43, false, false, false, false, true},
		// 35-42: same four columns, gauge collapsed, ACCOUNT shrinking.
		{42, false, false, false, false, true},
		{35, false, false, false, false, true},
	} {
		l := Plan(SetStatus, tc.width, 40, 4, false)
		if got := hasColumn(l.Columns, ColWindow); got != tc.window {
			t.Errorf("width %d: WINDOW present=%v, want %v", tc.width, got, tc.window)
		}
		if got := hasColumn(l.Columns, ColType); got != tc.typ {
			t.Errorf("width %d: TYPE present=%v, want %v", tc.width, got, tc.typ)
		}
		if got := hasColumn(l.Columns, ColAuto); got != tc.auto {
			t.Errorf("width %d: AUTO present=%v, want %v", tc.width, got, tc.auto)
		}
		if got := hasColumn(l.Columns, ColState); got != tc.state {
			t.Errorf("width %d: STATE present=%v, want %v", tc.width, got, tc.state)
		}
		if l.Collapsed != tc.collapsed {
			t.Errorf("width %d: Collapsed=%v, want %v", tc.width, l.Collapsed, tc.collapsed)
		}
	}
}

// Four columns are never dropped, at any width the page still renders at all.
// IDX is the hotkey; without ACCOUNT there is nothing to identify; USED and
// RESETS IN are the two questions the dashboard exists to answer.
func TestFourColumnsSurviveEveryWidth(t *testing.T) {
	for w := 35; w <= 140; w++ {
		l := Plan(SetStatus, w, 40, 4, false)
		for _, c := range []Column{ColIdx, ColAccount, ColUsed, ColResets} {
			if !hasColumn(l.Columns, c) {
				t.Fatalf("width %d: column %d is missing, and it must never be dropped", w, c)
			}
		}
	}
}

// SetList's own never-dropped four, mirroring the test above: IDX is still
// the hotkey and ACCOUNT still identifies the row, but LEFT stands in for
// USED as the answer this table exists to give.
func TestFourColumnsSurviveEveryWidthForSetListToo(t *testing.T) {
	for w := 35; w <= 140; w++ {
		l := Plan(SetList, w, 40, 4, false)
		for _, c := range []Column{ColIdx, ColAccount, ColLeft, ColResets} {
			if !hasColumn(l.Columns, c) {
				t.Fatalf("SetList width %d: column %d is missing, and it must never be dropped", w, c)
			}
		}
	}
}

// The height ladder, rung by rung, including the one that was added when the
// header line was resolved into existence. Walked with a fixed 4 account rows,
// so every threshold below is 22+4, 15+4, 12+4, 8+4, 6+4, 4+4, 3+4 and 2+4.
func TestTheHeightLadderDropsBlocksInTheOrderItSays(t *testing.T) {
	const rows = 4
	for _, tc := range []struct {
		height                                                    int
		wordmark, tagline, figures, border, blanks, title, header bool
	}{
		{26, true, true, true, true, true, true, true},    // nothing dropped: 22+N
		{19, true, true, false, true, true, true, true},   // figures dropped: 15+N
		{18, true, false, false, true, true, true, true},  // tagline also dropped
		{16, true, false, false, true, true, true, true},  // 12+N still fits without tagline
		{15, false, false, false, true, true, true, true}, // wordmark replaced: 8+N
		{12, false, false, false, true, true, true, true},
		{11, false, false, false, false, true, true, true}, // border dropped: 6+N
		{10, false, false, false, false, true, true, true},
		{9, false, false, false, false, false, true, true}, // blanks dropped: 4+N
		{8, false, false, false, false, false, true, true},
		{7, false, false, false, false, false, false, true},  // title dropped: 3+N
		{6, false, false, false, false, false, false, false}, // header (Active/Strategy/Mode) dropped: 2+N
	} {
		l := Plan(SetStatus, 80, tc.height, rows, false)
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
		l := Plan(SetStatus, 80, height, rows, false)
		if l.TooShort {
			t.Fatalf("height %d tripped the short floor; the floor is below 3", height)
		}
		if want := height - 2; l.VisibleRows != want {
			t.Errorf("height %d: VisibleRows=%d, want %d", height, l.VisibleRows, want)
		}
	}
}

// The notice rung sits between figures and tagline: dropped right after the
// figure block, before the tagline is even considered, so a tight terminal
// never carries both. A Snapshot with a notice needs one more row than the
// same Snapshot without one, and Plan has no way to know that except being
// told — this walks the boundary where that extra row starts, and stops,
// mattering.
func TestTheNoticeRungSitsBetweenFiguresAndTagline(t *testing.T) {
	const rows = 4

	// 26 fits everything when there is no notice to show.
	without := Plan(SetStatus, 80, 26, rows, false)
	if !without.Figures || without.Notice || !without.Tagline {
		t.Fatalf("80x26 without a notice: Figures=%v Notice=%v Tagline=%v, want true/false/true",
			without.Figures, without.Notice, without.Tagline)
	}

	// The same 26 rows no longer fit everything once notice=true shifts the
	// budget up by one: the figure block is what gives, not the notice line.
	with := Plan(SetStatus, 80, 26, rows, true)
	if with.Figures {
		t.Fatal("80x26 with a notice still shows the figure block; the extra row was not accounted for")
	}
	if !with.Notice {
		t.Fatal("80x26 with a notice dropped the notice line before the figure block was even gone")
	}
	if !with.Tagline {
		t.Fatal("80x26 with a notice dropped the tagline too early")
	}

	// One row later (25 down to 20), the notice line still fits alongside a
	// dropped figure block.
	if l := Plan(SetStatus, 80, 20, rows, true); l.Figures || !l.Notice || !l.Tagline {
		t.Fatalf("80x20 with a notice: Figures=%v Notice=%v Tagline=%v, want false/true/true",
			l.Figures, l.Notice, l.Tagline)
	}

	// At 19, the notice line is what gives next — one row short of 20.
	if l := Plan(SetStatus, 80, 19, rows, true); l.Figures || l.Notice || !l.Tagline {
		t.Fatalf("80x19 with a notice: Figures=%v Notice=%v Tagline=%v, want false/false/true",
			l.Figures, l.Notice, l.Tagline)
	}

	// Both cases converge once the notice line itself is gone: 80x19 without
	// a notice was never carrying one, and 80x19 with a notice just dropped
	// it, so the two arrive at the identical visible Layout.
	notice19 := Plan(SetStatus, 80, 19, rows, true)
	plain19 := Plan(SetStatus, 80, 19, rows, false)
	if notice19.Figures != plain19.Figures || notice19.Notice != plain19.Notice || notice19.Tagline != plain19.Tagline {
		t.Fatalf("80x19 did not converge: with notice=%+v without=%+v",
			struct{ Figures, Notice, Tagline bool }{notice19.Figures, notice19.Notice, notice19.Tagline},
			struct{ Figures, Notice, Tagline bool }{plain19.Figures, plain19.Notice, plain19.Tagline})
	}

	// Further down the ladder the notice line stays dropped, same as any
	// other block once its rung has fired.
	if l := Plan(SetStatus, 80, 16, rows, true); l.Figures {
		t.Fatal("80x16 with a notice still shows the figure block")
	}
}

// The floors. Below them the page says what it needs rather than rendering
// something unreadable.
func TestBelowTheFloorsThePageSaysWhatItNeeds(t *testing.T) {
	if l := Plan(SetStatus, 34, 40, 4, false); !l.TooNarrow {
		t.Error("34 columns did not trip the narrow floor")
	}
	if l := Plan(SetStatus, 80, 2, 4, false); !l.TooShort {
		t.Error("2 rows did not trip the short floor")
	}
}

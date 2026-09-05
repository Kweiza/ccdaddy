package tui

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/forecast"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/theme"
	"github.com/Kweiza/ccdaddy/internal/usage"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// The version in a fixture is a fixed constant and never the build stamp, or
// every release reddens the golden.
const fixtureVersion = "v0.2.0"

// The eight pages live under testdata and are regenerated with -update rather
// than retyped: `go test ./internal/tui -run TestThePage -update -count=1`
// writes each file from what the renderer produced, and the diff it leaves is
// reviewed like any other change to a file. One of them, the 80x24 design
// target, was drawn BY HAND first and then checked against the renderer, which
// is the opposite discipline and is used exactly once: a golden a human writes
// is a golden that agrees with whoever wrote it last, so the hand-drawn page is
// the acceptance criterion for the glyph swap and every other page is a
// transcription of what the renderer then did.
//
// THE FIXTURE-DIFF LEDGER. The reference blocks these pages replace were
// drawn by a probe that predates the width ladder, the height ladder and the
// hand-transcribed chrome. Every differing byte was measured against those
// three and falls into one of five classes, none of which is a renderer bug:
//
//  1. AMENDED ON PURPOSE, and stated as such. Active, Strategy, and Current
//     each own a row directly above the column header, so a long value cannot
//     push a later fact off the right edge. The daemon footer takes the wording
//     `ccdad daemon status` already prints, so "running  pid 8123  up 2h05m"
//     is "running (pid 8123, up 2h05m)".
//  2. THE REFERENCE IS STALE ON ACCOUNT WIDTH. Its 80-column blocks render
//     ACCOUNT at 23 columns. The width ladder gives 20 and the ladder is
//     right: the reference was drawn by the probe that still had the defect
//     where WIDENING a terminal narrowed the address column, and holding
//     ACCOUNT at its comfort width is the fix for it.
//  3. THE REFERENCE IS STALE ON THE HEIGHT-DROP ORDER. The ladder spends the
//     tagline and the blank separators, then the trailer, then the family art.
//     Farther down it drops the frame, then the section headings, then the
//     title, and the summary block is the last thing to give -- which at 56x10
//     and 43x9 it does, on a fleet of five accounts. At 80x13 the headings are
//     what gives and the facts under them stay.
//  4. THE FIGURE BLOCK IS ANCHORED, NOT INDENTED. The chrome transcribes the
//     block against its own leftmost content across all six rows rather than
//     against the reference's first column, so it sits one column further
//     left. That is a transcription decision with its own test, not drift.
//  5. THE REFERENCE DROPS THE WRONG HALF OF THE FOOTER. Its 56-column block
//     keeps the whole keybar and shows no daemon indicator at all -- while
//     its own 43-column block does show one. The rule is that the indicator
//     is never truncated away and the keybar is what loses bindings, so at
//     56 columns the keybar is cut and the indicator stays.
//
// A sixth class would be a renderer bug. There is not one.

// The six fixtures are the acceptance criterion. They are compared whole
// rather than by keyword: a near-miss passes any "contains a table" check and
// then drifts on the first change to either side.
//
// The tall page leads them, and it is the only one that draws the whole page:
// the wordmark, the tagline, the family art, both blank separators, the
// sectioned table and the trailer under it. Every rung below it is that page
// with something taken away, and a set whose tallest member had already lost a
// block could not say which rung took it.
func TestThePageRendersByteForByteAtEveryLadderRung(t *testing.T) {
	for _, tc := range []struct {
		name          string
		width, height int
		file          string
	}{
		{"every block, trailer included", 113, 34, goldenTrailer},
		{"every column the ladder fits, legend and art spent", 113, 26, goldenFullPage},
		{"the design target, legend and art spent", 80, 24, goldenDesignTarget},
		{"the frame and the headings gone, the facts kept", 80, 13, goldenShort},
		{"the frame dropped", 56, 10, goldenNarrow},
		{"the block collapsed and the headings spent", 43, 9, goldenCollapsed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkGolden(t, tc.file, fixtureModel(tc.width, tc.height).Body())
		})
	}
}

// Every page fits in the terminal it was planned for. The height ladder
// computes a budget and the renderer spends it, and nothing else compares the
// two -- a block emitted outside the budget, or a rung that saves the wrong
// number of rows, shows up here and nowhere else.
func TestEveryFixtureFitsTheTerminalItWasPlannedFor(t *testing.T) {
	for _, tc := range []struct{ w, h int }{{113, 34}, {113, 26}, {80, 24}, {80, 13}, {56, 10}, {43, 9}, {80, 23}, {80, 5}, {35, 3}} {
		got := len(strings.Split(fixtureModel(tc.w, tc.h).Body(), "\n"))
		if got > tc.h {
			t.Errorf("at %dx%d the page is %d rows, which is %d more than the terminal has", tc.w, tc.h, got, got-tc.h)
		}
	}
}

// A bordered lipgloss box soft-wraps overlong content rather than truncating,
// so a single line one column too wide costs a row and pushes the page past
// its height budget with no error anywhere. The check is cheap and it is the
// only thing that would notice.
//
// The table walks every SHAPE a golden page under testdata was drawn at, and
// the two prep functions are what it costs to say that rather than a taste for
// table-driven tests. The five width-ladder rungs were already covered twice
// over: this guard renders the same page checkGolden compares against a file,
// so a file too wide would mean a render too wide and this would be red before
// the comparison was. The notice page and the zero-accounts page were not
// covered at all, because neither is drawn at a shape this table held -- one is
// 80x23 with a notice set, the other 80x13 with the rows taken away -- so two
// of the eight files had nothing independent bounding their width, and a
// regeneration would have blessed whatever came out of the renderer.
//
// The nil-rows case builds its model the way the zero-accounts golden does --
// fixtureModel first, rows removed after -- and not as an empty Model. Body
// asks Plan about at least one row whatever Snap.Rows holds, and the report and
// the summary block come from the fixture either way, so a page assembled from
// scratch would be a different page than the one on disk and would bound the
// wrong thing.
func TestThePageNeverScrollsHorizontally(t *testing.T) {
	for _, tc := range []struct {
		name string
		w, h int
		prep func(*Model)
	}{
		{"every block, trailer included", 113, 34, nil},
		{"every column the ladder fits, legend and art spent", 113, 26, nil},
		{"the design target, legend and art spent", 80, 24, nil},
		{"one notice", 80, 23, func(m *Model) {
			m.Snap.Notices = []string{"hover thresholds could not be read"}
		}},
		{"the frame and the headings gone, the facts kept", 80, 13, nil},
		{"zero accounts", 80, 13, func(m *Model) { m.Snap.Rows = nil }},
		{"the frame dropped", 56, 10, nil},
		{"the block collapsed and the headings spent", 43, 9, nil},
		{"the smallest page that still renders", 35, 3, nil},
	} {
		m := fixtureModel(tc.w, tc.h)
		if tc.prep != nil {
			tc.prep(&m)
		}
		for i, line := range strings.Split(m.Body(), "\n") {
			if got := ansi.StringWidth(line); got > tc.w {
				t.Errorf("%s at %dx%d: line %d is %d columns wide: %q", tc.name, tc.w, tc.h, i, got, line)
			}
		}
	}
}

// The None theme emits no escape byte, and that is what makes a fixture
// comparison against testdata worth running at all -- and what lets the rest of
// this file, and model_test.go, go on asserting on raw rendered bytes with no
// ansi.Strip anywhere.
//
// So both halves are asserted, and the App's page is asserted beside the
// Model's. The None theme must carry none; the Dark theme must carry some. The
// second is what turns the first from a property of this package into a
// property of the THEME, and it is the assertion that goes red the day somebody
// deletes a Style call and every colour test above it goes on passing against a
// page that has quietly stopped being coloured.
func TestTheNoneThemeEmitsNoEscapeBytesAndTheDarkThemeDoes(t *testing.T) {
	if strings.ContainsRune(fixtureModel(113, 26).Body(), 0x1b) {
		t.Fatal("the None theme carries an escape byte, so no fixture here can be trusted")
	}
	if strings.ContainsRune(newApp(fixtureOptions()).m.Body(), 0x1b) {
		t.Fatal("the App's own page carries an escape byte under the None theme")
	}
	if !strings.ContainsRune(darkModel(113, 26).Body(), 0x1b) {
		t.Fatal("the Dark theme carries no escape byte, so the assertion above proves nothing")
	}
}

// The floors. Below them the page says what it needs, rather than rendering
// something unreadable or panicking on a negative width.
func TestBelowTheFloorsThePageRendersWhatItNeeds(t *testing.T) {
	if got := fixtureModel(30, 24).Body(); !strings.Contains(got, "ccdad needs 35 columns") {
		t.Errorf("at 30 columns: %q", got)
	}
	if got := fixtureModel(80, 2).Body(); !strings.Contains(got, "ccdad needs 4 rows") {
		t.Errorf("at 2 rows: %q", got)
	}
	for _, tc := range []struct{ w, h int }{{0, 0}, {1, 1}, {-1, -1}} {
		fixtureModel(tc.w, tc.h).Body() // must not panic
	}
}

// The scrolling rung. A table that stops at the bottom of the terminal is one
// a reader takes for the whole list, so the last visible line names what is
// off the page instead of carrying one more account.
func TestTheScrollingRungNamesWhatIsOffThePage(t *testing.T) {
	m := fixtureModel(80, 5)
	body := m.Body()
	lines := strings.Split(body, "\n")
	if len(lines) != 5 {
		t.Fatalf("at 80x5 the page is %d rows:\n%s", len(lines), body)
	}
	if !strings.Contains(body, "+4 more") {
		t.Fatalf("four accounts fell off the page and nothing said so:\n%s", body)
	}
	if !strings.Contains(body, "(j/k)") {
		t.Fatalf("the count does not say how to reach the rest:\n%s", body)
	}
	if !strings.Contains(body, "work@example.com") {
		t.Fatalf("the rung spent the page on a count and drew no accounts:\n%s", body)
	}
}

// With room for exactly one row the account wins over the count. A page showing
// a count of four and no accounts at all has stopped being a dashboard -- and
// j/k, which the count advertises, would have had nothing to move through.
//
// It wins over a section heading too, and at this size that is now the LADDER's
// doing rather than the window's: a page this short gave the headings up rungs
// ago, so the list under it is accounts and there is no heading left to take the
// only line. The assertion stays, because what it pins is the page -- one line,
// and an address on it -- and it would catch either mechanism failing. It is
// made on the ADDRESS and not merely on the absence of a count for that reason:
// a window that put a provider's name on the page and no account at all is the
// one arrangement this rung exists to refuse, wherever it came from.
func TestWithRoomForOneRowTheAccountWinsAndTheCountGoes(t *testing.T) {
	body := fixtureModel(35, 5).Body()
	lines := strings.Split(body, "\n")
	if len(lines) != 5 {
		t.Fatalf("at 35x5 the page is %d rows:\n%s", len(lines), body)
	}
	if strings.Contains(body, "more") {
		t.Fatalf("the one row left was spent on a count instead of an account:\n%s", body)
	}
	if strings.Contains(body, view.ClaudeSection) {
		t.Fatalf("the one row left was spent on a section heading instead of an account:\n%s", body)
	}
	// The address is cut to the ACCOUNT column's hard floor at this width, so
	// the head is what there is to look for -- which is the half the
	// head-preserving truncation exists to keep.
	if !strings.Contains(body, "work@examp") {
		t.Fatalf("the never-dropped account row was dropped:\n%s", body)
	}
}

// Both headings are drawn, and both are drawn as TABLE ROWS: the text sits in
// the ACCOUNT column, under the ACCOUNT heading, with every other cell of the
// row empty.
//
// The column offset is the whole assertion. A heading printed as a line above
// the table would satisfy any Contains check and would sit flush left, out of
// line with the addresses it is a heading for -- which is the arrangement the
// table-row shape was chosen over, because a line above the table cannot know
// what the width ladder gave the ACCOUNT column.
func TestEachProviderHeadingIsATableRowInTheAccountColumn(t *testing.T) {
	lines := strings.Split(fixtureModel(113, 34).Body(), "\n")
	head := -1
	for i, line := range lines {
		if strings.Contains(line, view.AccountHeader) && strings.Contains(line, view.IdxHeader) {
			head = i
			break
		}
	}
	if head < 0 {
		t.Fatalf("no column heading row:\n%s", strings.Join(lines, "\n"))
	}
	at := strings.Index(lines[head], view.AccountHeader)
	for _, want := range []string{view.ClaudeSection, view.CodexSection} {
		found := false
		for _, line := range lines[head+1:] {
			i := strings.Index(line, want)
			if i < 0 {
				continue
			}
			found = true
			if i != at {
				t.Errorf("%s starts at column %d and ACCOUNT at %d; the heading is not in the account column",
					want, i, at)
			}
			// Everything else on the row is whitespace and the frame. A
			// heading that carried an index, a state or an age would be
			// claiming those facts about a provider.
			if got := strings.Fields(strings.Trim(line, "│ ")); len(got) != 1 || got[0] != want {
				t.Errorf("the %s row carries %q; a heading row is one word and empty cells", want, got)
			}
		}
		if !found {
			t.Errorf("no %s heading under the column heading row:\n%s", want, strings.Join(lines, "\n"))
		}
	}
}

// A heading over NO accounts still draws, and the empty-store sentence is
// printed ONCE under both of them rather than once under each.
//
// The two halves are one decision. A heading over nothing says the provider
// exists and this machine has no account on it, which is what a reader of a
// one-provider fleet is asking; and "no accounts" is a fact about the STORE
// rather than about either section, so a copy under each heading would be the
// page claiming two separate findings where there is one.
func TestAnEmptyStoreDrawsBothHeadingsAndSaysItOnce(t *testing.T) {
	m := fixtureModel(80, 13)
	m.Snap.Rows = nil
	body := m.Body()
	for _, want := range []string{view.ClaudeSection, view.CodexSection} {
		if strings.Count(body, want) != 1 {
			t.Errorf("%s appears %d times on an empty store, want once:\n%s",
				want, strings.Count(body, want), body)
		}
	}
	if n := strings.Count(body, "no accounts"); n != 1 {
		t.Errorf("the empty-store sentence appears %d times, want once:\n%s", n, body)
	}
}

// A page below the headings' rung draws no heading and still draws every
// account, in the order the grouping put them in.
//
// The order is the half a check for the absent words would miss. Losing the
// headings is losing the LABELS, and the drawable list is filtered rather than
// rebuilt from Snap.Rows, so the codex seat stays at the bottom where the
// heading used to be. A renderer that went back to the store's own order would
// move an account up the page as the terminal got shorter, and would do it at
// exactly the heights where nothing is left on the page to explain the move.
//
// 80x13 is the shortest page that keeps every account, which is what makes it
// the one to ask: the two lines the headings were spending are the two that pay
// for the title and the summary block one rung further down.
func TestAPageBelowTheHeadingsRungKeepsEveryAccountAndItsOrder(t *testing.T) {
	m := fixtureModel(80, 13)
	if l := m.plan(); l.Sections {
		t.Fatal("80x13 still plans its section headings, so this asks nothing")
	}
	// The codex seat is moved to the FRONT of the store, which is what makes the
	// order claim measurable at all: the fixture pool already lists it last, so
	// against that pool a page reading Snap.Rows and one reading the grouping
	// draw the same five lines. Moved here, the grouping still puts it under
	// every Claude account and the store no longer agrees.
	stored := m.Snap.Rows
	m.Snap.Rows = append([]view.Row{stored[len(stored)-1]}, stored[:len(stored)-1]...)
	body := m.Body()
	for _, gone := range []string{view.ClaudeSection, view.CodexSection} {
		if strings.Contains(body, gone) {
			t.Errorf("the page draws %s at a height that gave the headings up:\n%s", gone, body)
		}
	}
	// From the column heading down, so the Active lines above the table -- which
	// name two of these accounts in a fixed order of their own -- cannot answer
	// for the rows.
	lines := strings.Split(body, "\n")
	head := 0
	for i, line := range lines {
		if strings.Contains(line, view.AccountHeader) && strings.Contains(line, view.IdxHeader) {
			head = i + 1
			break
		}
	}
	at := 0
	for _, r := range fixtureRows() {
		name, _, _ := strings.Cut(r.Account.Email, "@")
		row := -1
		for i, line := range lines[head:] {
			if strings.Contains(line, name) {
				row = i
				break
			}
		}
		if row < 0 {
			t.Fatalf("%s is not in the table:\n%s", r.Account.Email, body)
		}
		if row < at {
			t.Errorf("%s is drawn above the account before it; the grouping's order did not survive:\n%s",
				r.Account.Email, body)
		}
		at = row
	}
}

// A fleet with one provider still gets BOTH headings, which is the case the
// sections were added for: a machine with four Claude accounts and no codex one
// would otherwise render exactly as a build that has never heard of Codex. It
// is asked at 113x34, which is above the headings' rung: what that rung decides
// is whether a terminal has room for the pair, never how many there are.
func TestAOneProviderFleetStillDrawsTheOtherProvidersHeading(t *testing.T) {
	m := fixtureModel(113, 34)
	m.Snap.Rows = m.Snap.Rows[:4] // the four Claude accounts
	m.Snap.CodexServingLabel, m.Snap.CodexServingUUID = "", ""
	body := m.Body()
	if !strings.Contains(body, view.CodexSection) {
		t.Fatalf("a Claude-only fleet drew no CODEX heading, so the page cannot be told from one with no codex support:\n%s", body)
	}
}

// The count of what is off the page is in ACCOUNTS, and so is the window's own
// capacity. Both are what a reader can move the cursor to, and a section
// heading is not one.
//
// It is asked of window and not of a page, and the reason is the sections rung
// rather than a preference for unit tests. The rung fires while the ladder is
// still trying to fit the whole table, so every page that reaches the scrolling
// one has already handed its headings back and its display list is accounts --
// which makes the two counts agree on every page this package can draw, and
// leaves nothing on any of them able to tell a correct count from one that
// included the headings. window takes a Layout rather than a height exactly so
// the question can still be put to it.
//
// Five table rows over a list that HAS its headings is the case that tells them
// apart: one line is spent on the count and one on the CLAUDE heading, so three
// accounts of five are drawn. Counting display rows instead would say three are
// missing -- seven lines less the four drawn -- and would promise a press of j
// that does not exist.
func TestTheCountOfHiddenRowsIsInAccountsAndNotInTableRows(t *testing.T) {
	m := fixtureModel(43, 9)
	shown, more := m.window(Layout{Sections: true, VisibleRows: 5})
	if more != 2 {
		t.Errorf("the window says %d accounts are off the page, want the 2 it did not draw", more)
	}
	if len(shown) != 4 {
		t.Fatalf("the window drew %d of its 5 table rows, want 4 and a count: %+v", len(shown), shown)
	}
	if got := accountsIn(shown); got != 3 {
		t.Errorf("the window drew %d accounts, want 3 -- the fourth line is the CLAUDE heading", got)
	}
}

// The cursor cannot land on a section heading, and the reason is STRUCTURAL
// rather than a guard: Model.Cursor indexes Snap.Rows, headings are not in
// Snap.Rows, and the marker column is drawn from ListRow.At -- which a heading
// carries as -1 and no cursor can be.
//
// It is walked over every account rather than asserted once, because the
// failure this rules out is off-by-one: a page that drew the cursor at a
// DISPLAY position would put it one row above the account it names for the four
// Claude seats and two rows above for the codex one, landing it on CLAUDE for
// the first and on the last Claude account for the fifth.
func TestTheCursorLandsOnAnAccountAndNeverOnASectionHeading(t *testing.T) {
	m := fixtureModel(113, 34)
	for i, r := range fixtureRows() {
		m.Cursor = i
		var marked []string
		for _, line := range strings.Split(m.Body(), "\n") {
			if strings.Contains(line, m.Glyphs.Cursor) {
				marked = append(marked, line)
			}
		}
		if r.Active {
			// The live account keeps its own marker; see noCursor for why it
			// wins that cell.
			if len(marked) != 0 {
				t.Errorf("cursor %d is on the live account and something drew a cursor glyph: %q", i, marked)
			}
			continue
		}
		if len(marked) != 1 {
			t.Fatalf("cursor %d is drawn on %d rows, want exactly 1: %q", i, len(marked), marked)
		}
		// The head of the address, because ACCOUNT is cut to 20 columns at this
		// width once TIER is in the fixed order.
		if head, _, _ := strings.Cut(r.Account.Email, "@"); !strings.Contains(marked[0], head) {
			t.Errorf("cursor %d is drawn on %q, which is not %s", i, marked[0], r.Account.Email)
		}
	}
}

// Scrolling names the account the cursor is on, at every offset the cursor can
// take.
//
// The cursor's room is measured in ACCOUNTS -- see scrolled -- and this is what
// that buys. A room counted in table rows would be one too many at every offset
// inside a section and two too many across the boundary, so pressing j to the
// bottom of the list would leave the cursor pointing at an account the page has
// already scrolled past.
//
// 80x7 and not 80x8, which is where this used to be asked: the headings' rung
// hands two rows back to the table, and at 8 rows the whole fleet fits again
// and there is nothing left to scroll. A test named for scrolling that renders
// a page holding every account passes without asking anything.
func TestScrollingKeepsTheCursorsAccountOnThePage(t *testing.T) {
	rows := fixtureRows()
	for i, r := range rows {
		m := fixtureModel(80, 7)
		m.Cursor = i
		m = scrolled(m)
		body := m.Body()
		if head, _, _ := strings.Cut(r.Account.Email, "@"); !strings.Contains(body, head) {
			t.Errorf("the cursor is on %s (row %d) and the page does not draw it:\n%s", r.Account.Email, i, body)
		}
	}
}

// An account no daemon has ever published, an account with no reading, and an
// account at 100% all appear in the fixtures. This names the ones the fixtures
// would silently stop covering if the placeholder data were edited.
func TestTheFixtureDataStillCoversTheUnreadableRowAndTheEmptyState(t *testing.T) {
	var unreadable, full, unknownToTheEngine bool
	cols := view.ColumnsOf(fixtureRows())
	for _, r := range fixtureRows() {
		for _, w := range cols.Windows {
			switch r.WindowCell(w.Name) {
			case view.Unreadable:
				unreadable = true
			case "100%":
				full = true
			}
		}
		if r.Engine.State == daemon.StateUnknown {
			unknownToTheEngine = true
		}
	}
	if !unreadable {
		t.Error("no fixture row is unreadable, so the question-mark-and-no-bar rule is no longer drawn anywhere")
	}
	if !full {
		t.Error("no fixture row is at 100%, so the full bar is no longer drawn anywhere")
	}
	if !unknownToTheEngine {
		t.Error("no fixture row is unknown to the engine, so the state column's own absence arm is no longer drawn")
	}
}

// Snapshot.Notices would otherwise be consumed by nobody -- the most
// consequential entry it ever carries is the hover-thresholds-unreadable
// warning, and dropping it silently would mean the rows are measured
// differently than the text implies with no way to tell.
//
// It is drawn at 20 rows rather than at the 13 the short fixture uses, and
// that is the height ladder's own arithmetic rather than a preference: the
// notice rung is dropped high on the ladder, before the tagline is even
// considered, so at 13 rows a notice never survives to be rendered at all.
// Twenty-two is the shortest terminal that keeps one, and this renders at 23
// for the reason golden_test.go gives beside the file name. A fixture at 13
// would have pinned the absence and called it the presence.
func TestANonEmptyNoticeRendersDirectlyAboveTheColumnHeader(t *testing.T) {
	m := fixtureModel(80, 23)
	m.Snap.Notices = []string{"hover thresholds could not be read"}
	got := m.Body()
	if !strings.Contains(got, "note: hover thresholds could not be read") {
		t.Fatalf("a non-empty Notices did not render a note line:\n%s", got)
	}
	lines := strings.Split(got, "\n")
	for i, line := range lines {
		if strings.Contains(line, "note: ") {
			if i+1 >= len(lines) || !strings.Contains(lines[i+1], "IDX ACCOUNT") {
				t.Fatalf("the note line is not directly above the column header:\n%s", got)
			}
		}
	}
	checkGolden(t, goldenNotice, got)
}

// More notices than the one line can carry are counted rather than dropped: a
// page that shows the first of four and says nothing about the other three
// tells a user the first is all there was.
func TestMoreNoticesThanFitAreCountedRatherThanDropped(t *testing.T) {
	m := fixtureModel(80, 23)
	m.Snap.Notices = []string{"hover thresholds could not be read", "b", "c"}
	if got := m.Body(); !strings.Contains(got, "(+2 more)") {
		t.Fatalf("three notices rendered no count of what did not fit:\n%s", got)
	}
}

// The five reference fixtures all carry an empty Notices, and this is the
// test that would have caught it if the height ladder's notice rung rendered
// an empty line instead of nothing.
//
// It asserts against the same page with the note line deleted rather than
// against a constant of its own: that is the difference between "no line" and
// "no line and no gap", and a second golden would have pinned whichever of the
// two the renderer happened to produce.
func TestAnEmptyNoticesRendersNoLineAndNoGap(t *testing.T) {
	quiet := fixtureModel(80, 23)
	quiet.Snap.Notices = nil
	got := quiet.Body()
	if strings.Contains(got, "note:") {
		t.Fatalf("an empty Notices rendered a note line anyway:\n%s", got)
	}
	var want []string
	for _, line := range strings.Split(readGolden(t, goldenNotice), "\n") {
		if !strings.Contains(line, "note: ") {
			want = append(want, line)
		}
	}
	if joined := strings.Join(want, "\n"); got != joined {
		t.Fatalf("an empty Notices did more than remove the line:\ngot:\n%s\nwant:\n%s", got, joined)
	}
}

// Zero accounts is a valid state -- a fresh install, or every account
// removed -- and it is tested here rather than left to the pipe test to
// discover by accident. The table renders its header and an explicit row,
// never an empty bordered box with nothing inside it.
func TestZeroAccountsRendersAnExplicitRowNotAnEmptyBox(t *testing.T) {
	m := fixtureModel(80, 13)
	m.Snap.Rows = nil
	got := m.Body()
	if !strings.Contains(got, "no accounts") {
		t.Fatalf("zero accounts did not render an explicit row:\n%s", got)
	}
	checkGolden(t, goldenZeroAccounts, got)
}

// A fleet nobody could read still has a quota block, and this page draws the
// same one `ccdad status` does: ONE column headed QUOTA whose cells all say
// "?".
//
// It is a page this dashboard did not draw before it read its columns from
// internal/view. The ladder built the block out of the fleet's windows, so a
// fleet with none had no quota column at all -- a table that reads as a build
// with no quota feature rather than as a fleet nobody could read, and one whose
// columns the two surfaces disagreed about at exactly the moment a reader is
// asking why there are no numbers.
//
// No golden covers it, which is why it is asserted here. It is also the only
// case that reaches the placeholder's INDEX: that column stands for no window,
// so it indexes nothing, and the style function has to ask whether a window
// column names a window before it looks one up.
func TestAFleetWithNothingReadableStillDrawsOneQuotaColumn(t *testing.T) {
	snap := fixtureSnapshot(fixtureReport(113, 26))
	snap.Rows = []view.Row{{Account: store.Account{Email: "unread@example.com", Idx: 1}}}
	m := newModel(snap, 113, 26, theme.Of(theme.None), UnicodeGlyphs)
	m.Cursor = noCursor

	lines := strings.Split(m.Body(), "\n")
	head, account := -1, -1
	for i, l := range lines {
		switch {
		case head < 0 && strings.Contains(l, view.PlaceholderHeader) && strings.Contains(l, "ACCOUNT"):
			head = i
		case strings.Contains(l, "unread@example.com"):
			account = i
		}
	}
	if head < 0 || account < 0 {
		t.Fatalf("no quota column over a row:\n%s", m.Body())
	}
	// The row is found by its ADDRESS rather than by stepping one line down
	// from the heading, because the line under a column heading is a SECTION
	// heading now. A test that walked to head+1 would be reading a row with no
	// account on it, and would pass or fail on a cell that is empty by design.
	//
	// The offset is the HEADER's, so this asks where the cell SITS rather than
	// counting fields: the ACCOUNT cell holds an address and splitting the row
	// on whitespace would answer about spaces instead of about columns.
	at := strings.Index(lines[head], view.PlaceholderHeader)
	if row := lines[account]; at >= len(row) || row[at] != '?' {
		t.Errorf("nothing readable stands under %s at column %d of\n%s\n%s",
			view.PlaceholderHeader, at, lines[head], row)
	}
}

// A load that fails mid-session -- accounts.toml deleted, a contended lock --
// keeps the numbers that were already on the page and says so. Emptying the
// table would read as "you have no accounts", and keeping the numbers with no
// notice would present a reading from before the failure as the current one.
func TestAFailedLoadKeepsThePreviousSnapshotAndLabelsItRatherThanEmptyingTheTable(t *testing.T) {
	m := fixtureModel(80, 24).AfterLoad(view.Snapshot{}, errors.New("accounts.toml: no such file or directory"))
	if len(m.Snap.Rows) != len(fixtureRows()) {
		t.Fatalf("a failed load left %d rows, want the %d that were already there", len(m.Snap.Rows), len(fixtureRows()))
	}
	body := m.Body()
	if !strings.Contains(body, "could not refresh") || !strings.Contains(body, "accounts.toml") {
		t.Fatalf("a failed load rendered no notice naming it:\n%s", body)
	}
}

// A failure that repeats every ten seconds must not stack a notice per tick:
// the line is one line, and the page under it is what the user is reading.
func TestRepeatedFailuresLeaveExactlyOneNotice(t *testing.T) {
	m := fixtureModel(80, 24)
	for range 5 {
		m = m.AfterLoad(view.Snapshot{}, errors.New("a contended lock"))
	}
	if got := len(m.Snap.Notices); got != 1 {
		t.Fatalf("five failed loads left %d notices, want 1", got)
	}
	if m = m.AfterLoad(fixtureSnapshot(fixtureReport(80, 24)), nil); len(m.Snap.Notices) != 0 {
		t.Fatalf("a load that succeeded left the failure notice behind: %v", m.Snap.Notices)
	}
}

// The one-shot render is NOT the 80-column fixture. A pipe has no columns, so
// the width is the design target and the height is unbounded -- which means
// the height ladder does not run and the figure block appears.
func TestTheOneShotRenderIsEightyColumnsWideWithNoHeightLadder(t *testing.T) {
	var out strings.Builder
	o := fixtureOptions()
	o.Out = &out
	got, err := Render(o)
	if err != nil {
		t.Fatal(err)
	}
	// The figure block's own first row, rendered at the grid's width so the
	// expectation does not depend on what `inner` works out to. The one-shot
	// page is colourless, so this is the runes and nothing else.
	want := fixtureModel(80, 24).artRow(figureArt, 0, figureArt.W, theme.RoleAccent, "")
	if !strings.Contains(got, want) {
		t.Error("the one-shot render dropped the figure block, which only the height ladder does")
	}
	for _, line := range strings.Split(got, "\n") {
		if ansi.StringWidth(line) > 80 {
			t.Fatalf("the one-shot render is wider than the design target: %q", line)
		}
	}
	if out.String() != got+"\n" {
		t.Fatal("Render returned a page it did not write, so a caller holding only the writer sees something else")
	}
}

// The wordmark and figures arms both switch on Glyphs.Art, and ASCIIGlyphs is
// the one set that answers false. A page drawn under it must fall back to the
// plain-text blocks in chrome.go and never reach artRow -- the whole reason
// Glyphs.Art exists is a console or a width mode where the art is wrong, and
// a fallback that quietly kept drawing the art anyway would defeat it.
//
// It is rendered at the tall 113x34 shape and not at the 113x26 one, because
// the figure block is only on a page the height ladder has room for it on: at
// 26 rows a five-account fleet under two section headings has already spent the
// family art, and a test asking whether the ASCII fallback was drawn would be
// asking about a block neither vocabulary puts on the page.
func TestASCIIGlyphsDrawTheFallbackBlocksAndNoArtRune(t *testing.T) {
	got := fixtureModelGlyphs(113, 34, ASCIIGlyphs).Body()
	if !strings.Contains(got, figures[0]) {
		t.Error("ASCIIGlyphs did not draw the figure block's own first row")
	}
	if !strings.Contains(got, wordmark[0]) {
		t.Error("ASCIIGlyphs did not draw the wordmark's own first row")
	}
	if strings.ContainsRune(got, artUpper) || strings.ContainsRune(got, artLower) {
		t.Error("ASCIIGlyphs drew an art rune, which only Glyphs.Art may")
	}
}

// The scenario Glyphs.Art exists to answer, rendered rather than merely
// inspected: an explicit `glyphs = "unicode"` configuration in a process whose
// width engine is in its east-asian mode. PickGlyphs answers that with a set
// whose Name is "unicode" and whose Art is false -- a shape neither
// TestASCIIGlyphsDrawTheFallbackBlocksAndNoArtRune above (which goes through
// ASCIIGlyphs, Name=="ascii") nor a render.go gated on `m.Glyphs.Name ==
// "unicode"` instead of `m.Glyphs.Art` could tell apart from ordinary Unicode.
//
// This is built through PickGlyphs itself and not the ASCIIGlyphs constant,
// because the point is the real decision path that produces that shape, not a
// stand-in for it. t.Setenv is legal here for the same reason
// TestGlyphsAutoFallsBackToAsciiWhenTheWidthEngineIsInEastAsianMode's is:
// PickGlyphs reads RUNEWIDTH_EASTASIAN itself, at call time, through
// eastAsianWidth -- unlike the x/ansi width engine, which reads it once at
// package init and needs alsoInEastAsianMode's subprocess to observe honestly.
// This test is about the render pipeline's decision, not about a measured
// column, so no re-exec is needed.
func TestAnExplicitUnicodeGlyphSetStillFallsBackToTheTypedBlocksInEastAsianMode(t *testing.T) {
	t.Setenv("RUNEWIDTH_EASTASIAN", "1")
	g := PickGlyphs("unicode", true)
	if g.Name != "unicode" || g.Art {
		t.Fatalf("PickGlyphs(\"unicode\", true) under RUNEWIDTH_EASTASIAN=1 is Name=%q Art=%v, want Name=\"unicode\" Art=false -- fix the setup before trusting the rest of this test",
			g.Name, g.Art)
	}

	// 34 rows and not 26, for the reason the test above gives: at 26 the
	// figure block is not on the page in either vocabulary.
	got := fixtureModelGlyphs(113, 34, g).Body()
	if !strings.Contains(got, figures[0]) {
		t.Error("Name==\"unicode\", Art==false did not draw the figure block's own typed fallback row")
	}
	if !strings.Contains(got, wordmark[0]) {
		t.Error("Name==\"unicode\", Art==false did not draw the wordmark's own typed fallback row")
	}
	if strings.ContainsRune(got, artUpper) || strings.ContainsRune(got, artLower) {
		t.Error("Name==\"unicode\", Art==false drew an art rune, which only Glyphs.Art may")
	}

	// The escape hatch is narrow, not total: the frame, the cursor and the
	// markers stay Unicode even though the art does not, because their widths
	// are still predictable in this mode. See PickGlyphs and Glyphs.Art.
	if g.Cursor != UnicodeGlyphs.Cursor {
		t.Errorf("the fallback took the cursor along with the art: %q, want %q", g.Cursor, UnicodeGlyphs.Cursor)
	}
}

// A load that fails with no previous page to keep is fatal rather than a
// notice: there is nothing to label.
func TestAOneShotRenderReportsALoadFailureRatherThanDrawingAnEmptyPage(t *testing.T) {
	o := fixtureOptions()
	o.Load = func(time.Time) (view.Snapshot, error) { return view.Snapshot{}, errors.New("no store") }
	if _, err := Render(o); err == nil {
		t.Fatal("Render swallowed a load failure and drew a page from a zero snapshot")
	}
}

// fixtureNow is the clock every fixture was drawn against. The spans below are
// exact offsets from it, so each one renders as one unambiguous string.
var fixtureNow = time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)

// fixtureRows is the placeholder pool: one subscription part-spent, one credit
// account fully spent and held out of rotation, one idle alternate, one
// api-key seat that could not be read at all, and one CODEX seat the proxy is
// serving.
//
// The codex row was ADDED and nothing was converted to it. Rows two and four
// carry coverage TestTheFixtureDataStillCoversTheUnreadableRowAndTheEmptyState
// exists to protect -- the 100% window, the unreadable seat, the state no
// daemon has published -- and re-flagging one of them as codex would have moved
// that coverage into the codex section and taken it out of the Claude one,
// which is the half every page drew before there were two.
//
// It carries a FiveHour reading and nothing else, which is deliberate: the
// quota block is a function of what the fleet carries, so a codex row with a
// window of its own would add a column and move every page for a reason that
// has nothing to do with sections.
//
// MinPct and MinWindow are filled on every row that has a reading, and the
// second row's floor carries its own slack pair. Neither is decoration.
// strategy.OutOfQuota reads MinPct alone, so a pool that left it at zero would
// report every account as EMPTY -- the one at 52% included -- and a test of the
// gauge's colour would be unable to tell green from red because the whole
// fixture is red. A floor with no FloorSlack beside it is the same hazard one
// level down: the bar's length would come from the floor while its colour came
// from a zero nobody set.
//
// None of it prints. LEFT reads Headroom.Pct, USED reads the window, and
// nothing on the page reads MinPct or either floor figure, so no golden under
// testdata moves.
func fixtureRows() []view.Row {
	pct := func(v float64) *float64 { return &v }
	in := func(d time.Duration) *time.Time { t := fixtureNow.Add(d); return &t }

	return []view.Row{
		{
			Account: store.Account{
				UUID:  "d3b07384-d9a0-4f9b-9a2c-5f0c3a1e7b21",
				Email: "work@example.com", Alias: "work", Idx: 1,
				Kind: identity.KindSubscription, Tier: "claude_max",
			},
			Active:   true,
			HasEntry: true,
			Entry: usage.Entry{
				Snapshot:  &usage.Snapshot{FiveHour: usage.NewWindow(pct(87), in(82*time.Minute))},
				FetchedAt: fixtureNow.Add(-4 * time.Minute),
			},
			Headroom: strategy.Headroom{
				Known: true, Binding: usage.WindowFiveHour, Pct: 13, Slack: -7, Threshold: 80,
				MinPct: 13, MinWindow: usage.WindowFiveHour,
			},
			Engine: daemon.AccountStatus{UUID: "d3b07384-d9a0-4f9b-9a2c-5f0c3a1e7b21", State: daemon.StateActive},
		},
		{
			Account: store.Account{
				UUID:  "1c8f9a52-4e6b-4d18-8f77-2a9d5c0b6e34",
				Email: "enterprise@co.example", Idx: 2,
				Kind: identity.KindCredit, Disabled: true,
			},
			HasEntry: true,
			Entry: usage.Entry{
				Snapshot:  &usage.Snapshot{SevenDayOpus: usage.NewWindow(pct(100), in(5*time.Minute))},
				FetchedAt: fixtureNow.Add(-2 * time.Minute),
			},
			Headroom: strategy.Headroom{
				Known: true, Binding: usage.WindowSevenDayOpus, Pct: 0, Slack: -20, Threshold: 80,
				MinPct: 0, MinWindow: usage.WindowSevenDayOpus,
				HasFloor: true, Floor: usage.WindowSevenDayOpus,
				FloorSlack: -20, FloorThreshold: 80,
			},
			Engine: daemon.AccountStatus{UUID: "1c8f9a52-4e6b-4d18-8f77-2a9d5c0b6e34", State: daemon.StateExhausted},
		},
		{
			Account: store.Account{
				UUID:  "7f2e1b90-3c45-4a6d-9e08-b1d7c4a5f6e2",
				Email: "spare@example.com", Idx: 3,
				Kind: identity.KindSubscription, Tier: "claude_pro",
			},
			HasEntry: true,
			Entry: usage.Entry{
				Snapshot:  &usage.Snapshot{FiveHour: usage.NewWindow(pct(52), in(4*time.Hour+11*time.Minute))},
				FetchedAt: fixtureNow.Add(-6 * time.Minute),
			},
			Headroom: strategy.Headroom{
				Known: true, Binding: usage.WindowFiveHour, Pct: 48, Slack: 28, Threshold: 80,
				MinPct: 48, MinWindow: usage.WindowFiveHour,
			},
			Engine: daemon.AccountStatus{UUID: "7f2e1b90-3c45-4a6d-9e08-b1d7c4a5f6e2", State: daemon.StateCandidate},
		},
		{
			Account: store.Account{
				UUID:  "b6a4c2d0-8e15-4f37-a9c6-3d5b7e1f0a48",
				Email: "keyonly@example.com", Alias: "key", Idx: 4,
				Kind: identity.KindAPIKey,
			},
			Engine: daemon.AccountStatus{UUID: "b6a4c2d0-8e15-4f37-a9c6-3d5b7e1f0a48", State: daemon.StateUnknown},
		},
		{
			Account: store.Account{
				UUID:  "4a9e0c17-6b32-4d58-8107-c2f4e6a9d3b5",
				Email: "cx@example.com", Idx: 5,
				Kind: identity.KindSubscription, Tier: "chatgpt_plus",
				Provider: provider.Codex,
			},
			HasEntry: true,
			Entry: usage.Entry{
				Snapshot:  &usage.Snapshot{FiveHour: usage.NewWindow(pct(31), in(3*time.Hour+9*time.Minute))},
				FetchedAt: fixtureNow.Add(-3 * time.Minute),
			},
			Headroom: strategy.Headroom{
				Known: true, Binding: usage.WindowFiveHour, Pct: 69, Slack: 49, Threshold: 80,
				MinPct: 69, MinWindow: usage.WindowFiveHour,
			},
			// Serving is the codex lane's own live state, and it is what puts
			// the second Active fact on the page: the proxy has a pointer, so
			// the summary block names both providers.
			Engine: daemon.AccountStatus{UUID: "4a9e0c17-6b32-4d58-8107-c2f4e6a9d3b5", State: daemon.StateServing},
		},
	}
}

// fixtureReport is the daemon state each fixture was drawn with. The rungs
// deliberately do not all show the same one: running, stopped and unknown are
// three separate wordings, and a fixture set that only ever rendered one of
// them would leave the other two drawn by nothing.
func fixtureReport(width, height int) daemon.Report {
	switch {
	case width == 113 && height == 26:
		return daemon.Report{
			State:     daemon.DaemonRunning,
			HasStatus: true,
			Status: daemon.Status{
				PID:       8123,
				StartedAt: fixtureNow.Add(-2*time.Hour - 5*time.Minute),
			},
		}
	case width == 80 && height == 24:
		return daemon.Report{State: daemon.DaemonStopped}
	default:
		return daemon.Report{State: daemon.DaemonUnknown}
	}
}

// fixtureSnapshot names BOTH providers' live seats, which is what puts the
// two-line Active block on every page below. The codex label is the pool's
// fifth row, and the uuid beside it is that row's own: a label with no account
// behind it would render a summary the table below it does not corroborate.
func fixtureSnapshot(report daemon.Report) view.Snapshot {
	return view.Snapshot{
		Now:               fixtureNow,
		Rows:              fixtureRows(),
		Report:            report,
		ActiveLabel:       "work@example.com (work)",
		CodexServingLabel: "cx@example.com",
		CodexServingUUID:  "4a9e0c17-6b32-4d58-8107-c2f4e6a9d3b5",
		Strategy:          "headroom",
		Mode:              strategy.ModeHeadroom,
		HasMode:           true,
		Version:           fixtureVersion,
	}
}

// fixtureModel is the page every golden below was drawn from. It names the
// theme and the glyph set EXPLICITLY, and neither may become a default.
//
// theme.None because a fixture compares bytes and a palette that resolved
// itself would paint them. UnicodeGlyphs because detection must not reach a
// test at all: under `go test` on Windows this process's stdout is a captured
// pipe rather than a console, the code-page read answers "cannot carry UTF-8",
// and every page below would come out in the ASCII vocabulary -- turning the
// whole fixture set red on the one leg of CI that gates a release.
func fixtureModel(width, height int) Model {
	return fixtureModelGlyphs(width, height, UnicodeGlyphs)
}

// fixtureModelGlyphs is the same page with the vocabulary named, for the tests
// that are about the vocabulary itself.
func fixtureModelGlyphs(width, height int, g Glyphs) Model {
	return newModel(fixtureSnapshot(fixtureReport(width, height)), width, height, theme.Of(theme.None), g)
}

// fixtureOptions is the world every key test starts from, and StderrTTY is
// TRUE in it deliberately. The add key's first act is to ask that question, so
// a fixture that answered "not a terminal" would quietly turn the key into a
// refusal in every test that never thought about it — and a test asserting what
// the choice offers would be asserting what the refusal says instead. The two
// tests that are about the refusal set it false for themselves.
func fixtureOptions() Options {
	snap := fixtureSnapshot(fixtureReport(113, 26))
	return Options{
		Load:      func(time.Time) (view.Snapshot, error) { return snap, nil },
		Now:       func() time.Time { return fixtureNow },
		Out:       io.Discard,
		Theme:     theme.None,
		GlyphSet:  "unicode",
		StderrTTY: true,
	}
}

// The keys that move the cursor have to move something a reader can see. On a
// page where every row fits there is no scrolling to reveal it, so the marker
// is the only evidence the keystroke did anything at all -- and it is the only
// way to know which account the switch key is about to offer first.
func TestTheCursorIsDrawnOnTheRowItIsOn(t *testing.T) {
	m := fixtureModel(113, 26)
	m.Cursor = 1

	lines := strings.Split(m.Body(), "\n")
	var marked []string
	for _, line := range lines {
		if strings.Contains(line, m.Glyphs.Cursor+" 2 ") {
			marked = append(marked, line)
		}
	}
	if len(marked) != 1 {
		t.Fatalf("the cursor is drawn on %d rows, want exactly the one it is on:\n%s", len(marked), m.Body())
	}
	if strings.Count(m.Body(), m.Glyphs.Cursor+" ") != 1 {
		t.Errorf("more than one row carries a cursor:\n%s", m.Body())
	}

	// And it costs no width: the marker column was already there.
	for i, line := range lines {
		if ansi.StringWidth(line) > 113 {
			t.Fatalf("row %d is %d columns wide with a cursor drawn", i, ansi.StringWidth(line))
		}
	}
}

// The live account wins the column where the two meet. That mark answers which
// login a session would get, which is a fact about the credential; the cursor
// answers where the reader left the highlight, which the reader already knows.
// The cost -- an invisible cursor on the live row -- is real and is the reason
// this is pinned rather than left to whoever reads the switch next.
func TestTheLiveAccountKeepsTheMarkerWhenTheCursorIsOnIt(t *testing.T) {
	m := fixtureModel(113, 26)
	m.Cursor = 0 // the fixture pool's first row is the live account

	body := m.Body()
	if !strings.Contains(body, "* 1 ") {
		t.Fatalf("the live account lost its marker to the cursor:\n%s", body)
	}
	if strings.Contains(body, m.Glyphs.Cursor+" 1 ") {
		t.Fatalf("the cursor took the live account's marker:\n%s", body)
	}
}

// Nobody is pointing at anything in a pipe. The one-shot render answers
// bare `ccdad` off a terminal, and a selection marker
// there was put on the row by a reader who is not present.
func TestTheOneShotRenderDrawsNoCursor(t *testing.T) {
	// The live account is deliberately NOT the first row here: with the marker
	// suppressed the first row would otherwise be indistinguishable from a row
	// that simply has no marks on it, and the test would pass for a bug.
	snap := fixtureSnapshot(fixtureReport(113, 26))
	snap.Rows[0].Active = false
	snap.Rows[2].Active = true

	page, err := Render(Options{
		Load:     func(time.Time) (view.Snapshot, error) { return snap, nil },
		Now:      func() time.Time { return fixtureNow },
		Out:      io.Discard,
		Theme:    theme.None,
		GlyphSet: "unicode",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, UnicodeGlyphs.Cursor+" ") {
		t.Fatalf("the one-shot render drew a cursor:\n%s", page)
	}
	// The live account's own marker is untouched by the same change.
	if !strings.Contains(page, "* 3 ") {
		t.Fatalf("the one-shot render lost the live account's marker:\n%s", page)
	}
}

// Once the table scrolls, the cursor's index into the rows and the row's
// position on the screen differ by Top. A marker drawn against the screen
// position marks whichever account happens to be that far down instead -- and
// the switch key would then offer one account while the page pointed at
// another.
func TestTheCursorFollowsTheRowAndNotTheScreenPositionOnceItScrolls(t *testing.T) {
	m := fixtureModel(80, 5)
	m.Top, m.Cursor = 3, 3
	body := m.Body()
	if !strings.Contains(body, m.Glyphs.Cursor+" 4 ") {
		t.Fatalf("the cursor is not on the row it indexes:\n%s", body)
	}
	if strings.Contains(body, m.Glyphs.Cursor+" 3 ") {
		t.Fatalf("the cursor is drawn against the screen position rather than the row:\n%s", body)
	}
}

// Each summary fact owns a row. Active has one fact per provider, so neither a
// long account label nor the second provider can push Strategy or Current off
// the right edge of the terminal.
//
// The four lines are named here as VALUES, which is what the golden pages
// cannot do: they draw the same block, but a page compares as one string and
// says nothing about where one fact ends and the next begins.
func TestEachSummaryFactAndActiveProviderOwnsItsOwnLine(t *testing.T) {
	snap := fixtureSnapshot(fixtureReport(113, 26))
	m := newModel(snap, 113, 26, theme.Of(theme.None), UnicodeGlyphs)

	got := m.summaryLines(200)
	want := []string{
		"Active (Claude): work@example.com (work)",
		"Active (Codex): cx@example.com",
		"Strategy: headroom",
		"Current: headroom",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("summary lines =\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// No codex pointer means there is no codex fact to print. The remaining facts
// still keep their own rows rather than collapsing back into the old pipe-
// separated sentence.
//
// The pointer is taken OFF the fixture rather than a codex-less snapshot being
// built here, because that is the machine this asserts about: one that has a
// codex account in the store and nothing serving from it. The label is the
// gate and the account list is not, so a renderer that drew the line off "is
// there a codex row" is caught here and by nothing else.
func TestSummaryOmitsOnlyTheAbsentCodexProvider(t *testing.T) {
	m := fixtureModel(80, 24)
	m.Snap.CodexServingLabel = ""
	got := m.summaryLines(200)
	want := []string{
		"Active (Claude): work@example.com (work)",
		"Strategy: headroom",
		"Current: headroom",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("summary lines =\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The fourth line is part of the layout budget, not text appended after the
// page was planned. Fourteen rows is deliberately tight enough to drive the
// height ladder while still retaining the complete summary block: it is the
// shortest page the summary survives on at all, so a fourth line the budget did
// not know about would push the page past the terminal here first.
func TestTheCodexActiveLineIsIncludedInTheHeightBudget(t *testing.T) {
	m := fixtureModel(80, 14)
	body := m.Body()
	if got := len(strings.Split(body, "\n")); got > m.Height {
		t.Fatalf("page with a codex active line is %d rows in a %d-row terminal:\n%s", got, m.Height, body)
	}
	if !strings.Contains(body, "Active (Codex): cx@example.com") {
		t.Fatalf("the height ladder dropped only the codex active fact:\n%s", body)
	}
}

// The summary names the strategy in FORCE, and under hover that is not the one in
// the file: strategy.Options' withHover pass overrides the key. Printing the
// file's value there is the defect this test exists for -- a reader who had set
// consume-first saw consume-first and concluded hover was off.
//
// It renders under the palette that PAINTS and strips afterwards, because this
// is a test about the summary and the summary is the one block on the page that
// styles its own labels. "Strategy: " is a label and "hover" is the
// answer, so an SGR reset closes the label's span between them and a colourless
// render would be testing a line this program never draws.
func TestTheHeaderNamesHoverRatherThanTheStrategyHoverOverrode(t *testing.T) {
	snap := fixtureSnapshot(fixtureReport(113, 26))
	snap.Strategy, snap.Hover = "consume-first", true

	page := ansi.Strip(newModel(snap, 113, 26, theme.Of(theme.Dark), UnicodeGlyphs).Body())
	if !strings.Contains(page, "Strategy: hover") {
		t.Errorf("the summary does not name hover:\n%s", page)
	}
	if strings.Contains(page, "consume-first") {
		t.Errorf("the summary prints a strategy hover overrode:\n%s", page)
	}
}

// Hover is now one of the four exclusive strategies, so the picker marks it
// rather than a dormant automatic strategy from the compatibility fields.
func TestTheStrategyPickerMarksHoverWhenHoverIsSelected(t *testing.T) {
	snap := fixtureSnapshot(fixtureReport(113, 26))
	snap.Strategy, snap.Hover = "consume-first", true
	o := fixtureOptions()
	o.Load = func(time.Time) (view.Snapshot, error) { return snap, nil }

	a := appAt(t, o, 113, 26)
	a, _, _ = a.key(keyPress("c"))
	if a.pick.current < 0 {
		t.Fatal("the strategy picker marks nothing as current under hover")
	}
	if got := a.pick.items[a.pick.current].label; got != "hover" {
		t.Errorf("the picker marks %q as current, want hover", got)
	}
}

// fixtureFleet is a measured fleet that has something to say: the weekly axis
// empties at a fixed moment and the five-hour one holds at the measured rate.
//
// Every moment in it is derived from fixtureNow, whose zone is UTC, and the
// dashboard renders in the zone its Snapshot's clock was read in — so the line
// below is the same string on a machine in Seoul and on one in CI with TZ
// unset. A fleet built from time.Now() would render a different hour in each.
func fixtureFleet() forecast.Fleet {
	// Fifty hours out, so the rendered span is a whole "2d2h" that can be
	// checked by hand against the absolute moment printed beside it.
	dry := fixtureNow.Add(50 * time.Hour)
	return forecast.Fleet{
		Basis: forecast.Basis{
			Window: 4 * time.Hour, Observed: 3*time.Hour + 51*time.Minute, Known: true,
		},
		FiveHour: forecast.Axis{Verdict: forecast.VerdictHolds},
		Weekly: forecast.Axis{
			Verdict: forecast.VerdictRunsDry, DryAt: dry, HasDryAt: true,
		},
		Both: forecast.Axis{Verdict: forecast.VerdictRunsDry, DryAt: dry, HasDryAt: true},

		// The seat counts a forecast of a fleet in this state carries, rather
		// than zeroes. A known basis is exactly the condition the seat search
		// runs under, so a Fleet with a basis and no counts is one the forecast
		// cannot produce, and a fixture in a state nothing reaches would pin the
		// renderer against a page nobody sees.
		//
		// Nine against five is short by four, and short is the only thing this
		// fleet can be: the search counts upward only from a size whose own run
		// went dry, which is what the verdicts above say happened.
		AccountsUsable: 5,
		AccountsNeeded: 9,
		HasNeeded:      true,
		NeededBy:       usage.WindowSevenDay,
		HasNeededBy:    true,
	}
}

// fixtureHoldingFleet is the same measurement over a fleet that survives the
// horizon: five accounts where three of them would do.
//
// It carries a seat count on purpose. The full form of that count is printed by
// `ccdad runway` and names the spare seats; the one-line summary this page draws
// is asserted below to leave them out, and a fixture with no count at all would
// pass that assertion for the wrong reason -- by having nothing to leave out.
func fixtureHoldingFleet() forecast.Fleet {
	return forecast.Fleet{
		Basis: forecast.Basis{
			Window: 4 * time.Hour, Observed: 3*time.Hour + 51*time.Minute, Known: true,
		},
		FiveHour: forecast.Axis{Verdict: forecast.VerdictHolds},
		Weekly:   forecast.Axis{Verdict: forecast.VerdictHolds},
		Both:     forecast.Axis{Verdict: forecast.VerdictHolds},

		AccountsUsable: 5,
		AccountsNeeded: 3,
		HasNeeded:      true,
	}
}

// The dashboard names how many accounts a short fleet needs, and it spells none
// of that here: the clause arrives inside view.RunwayLine, which `ccdad status`
// renders as well. Two surfaces and one wording -- a page that assembled its
// own would be a third, free to say something different about the
// same fleet.
//
// A fleet that HOLDS carries no seat count at all, and the second half asserts
// the page rather than the rule. Its answer is already the word "holds", and
// this line is read at a glance beside Active: and Current:, where a clause spent
// on good news is a clause the short case needed.
//
// What refuses it there is view.RunwayLine's own holding wording, which answers
// before any seat clause is reached; view.RunwayNeedSegment's rule -- a count
// that is not larger than the fleet says nothing -- decides the other way in,
// where the fleet run holds and an axis is still undecided, and it is pinned in
// internal/view beside the function. Measured: taking that rule out leaves this
// test green and reddens the one there. What this asserts is the whole page, and
// "need" catches both wordings a seat count could arrive in -- the summary's own
// "need 3 (2 more)" and the block form's "3 needed to hold at this rate", which
// would be the shape of a dashboard that drew the runway command's line instead
// of the summary.
//
// Rendered at 113 columns rather than at the 80x24 design target because the
// whole line is what is being read: a short fleet's line is 91 columns and comes
// back cut at 80, with the cue that says so. That cut is a separate question and
// TestTheRunwayLineIsCutToTheFrameRatherThanWrappingIt asks it, at 80 and at
// four other widths.
func TestTheDashboardRunwayRowsNameTheSeatsOnlyAShortFleetNeeds(t *testing.T) {
	short := fixtureModel(113, 24)
	short.Snap.Forecast, short.Snap.HasForecast = fixtureFleet(), true
	if body := short.Body(); !strings.Contains(body, "need 9 (4 more)") {
		t.Errorf("a fleet of five that needs nine drew no seat count:\n%s", body)
	}

	holding := fixtureModel(113, 24)
	holding.Snap.Forecast, holding.Snap.HasForecast = fixtureHoldingFleet(), true
	body := holding.Body()
	for _, want := range []string{"Runway:  5h holds", "7d holds", "basis 3h51m"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the holding fixture omitted %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "need") {
		t.Errorf("a fleet that holds spent part of the page on seats it does not need:\n%s", body)
	}
}

// fixtureRunwayLine is what the fleet above renders to, spelled out here rather
// than computed, so this file pins the bytes on the page and not the agreement
// of one function with itself.
//
// Ninety-one columns, which is wider than the 80-column design target: a short
// fleet's line names the seats it needs, and at 80 the page hands back the first
// 78 columns of this with a cue on the end. The whole string is the pin, so the
// test that looks for it renders wide enough to hold it, and the cut form is
// asserted separately by TestTheRunwayRowsKeepTheirBasisVisible.
//
// The seat count is last for that reason. The frame eats the tail, and the
// clause the page can spare is the one `ccdad runway` prints in full -- not the
// basis, which is on this line and nowhere else on this page.
const fixtureRunwayLine = "Runway:  5h holds"

// A populated forecast that the Snapshot does not claim moves no golden. All
// eight whole-page fixtures are compared byte for byte, and every one of them
// was drawn on a machine with no history behind it — which is the state of
// every machine for the first hours after a release that starts recording.
//
// HasForecast is what decides, and this is the test that makes the conditional
// a decision rather than an accident: the Fleet handed to each page below is
// fully populated and says the fleet runs dry, so an implementation that drew
// the line off the Fleet's own contents instead of off the flag is caught here.
//
// TWO of the eight catch it, measured rather than assumed: 113x34 and 113x26.
// It was three before the table grew its section headings and the fleet its
// fifth account — 80x24 and the notice page have both dropped below the rung
// that keeps a four-row runway block since — and the number is re-measured here
// rather than carried, because a count nobody re-measures is how a table of
// fixtures comes to claim coverage it stopped having. The other six would look
// identical under that mutation and rule nothing out. They are walked anyway:
// the six that cannot see this failure are exactly the six that would see a
// runway line drawn at a rung that is supposed to have taken it away.
func TestAForecastTheSnapshotDoesNotClaimMovesNoGolden(t *testing.T) {
	for _, tc := range []struct {
		name          string
		width, height int
		prep          func(*Model)
		file          string
	}{
		{"every block, trailer included", 113, 34, nil, goldenTrailer},
		{"every column the ladder fits, legend and art spent", 113, 26, nil, goldenFullPage},
		{"the design target, legend and art spent", 80, 24, nil, goldenDesignTarget},
		{"the frame and the headings gone, the facts kept", 80, 13, nil, goldenShort},
		{"the frame dropped", 56, 10, nil, goldenNarrow},
		{"the block collapsed and the headings spent", 43, 9, nil, goldenCollapsed},
		{"one notice", 80, 23, func(m *Model) {
			m.Snap.Notices = []string{"hover thresholds could not be read"}
		}, goldenNotice},
		{"zero accounts", 80, 13, func(m *Model) { m.Snap.Rows = nil }, goldenZeroAccounts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fixtureModel(tc.width, tc.height)
			if tc.prep != nil {
				tc.prep(&m)
			}
			m.Snap.Forecast = fixtureFleet()
			m.Snap.HasForecast = false
			got := m.Body()
			checkGolden(t, tc.file, got)
			if strings.Contains(got, "Runway") {
				t.Fatalf("a page with HasForecast false drew a runway line anyway:\n%s", got)
			}
		})
	}
}

// With a forecast behind it the line appears, directly under the
// Active/Strategy/Current block and above everything about the rows — the position
// `ccdad status` gives it, among the labels rather than among the accounts.
//
// The note line's own position is asserted elsewhere to be directly above the
// column header, so the two orderings together fix all three lines: header,
// runway, note, table.
//
// 113 columns rather than the 80x24 design target, because the needle is the
// whole line and a short fleet's line is 91 columns wide. At 80 it is cut, and a
// test that searched for the cut form would be pinning the truncator's arithmetic
// in a test about row order. Nothing here depends on the width: the four lines
// are drawn in the same order at every rung that draws them.
//
// 25 rows because the page has to carry the runway block AND the note at once,
// and both are conditional: at 24 the ladder gives the note up and there is no
// ordering left to assert.
func TestTheRunwayLineSitsUnderTheHeaderLineAndAboveTheNote(t *testing.T) {
	m := fixtureModel(113, 25)
	m.Snap.Forecast, m.Snap.HasForecast = fixtureFleet(), true
	m.Snap.Notices = []string{"hover thresholds could not be read"}

	lines := strings.Split(m.Body(), "\n")
	at := func(needle string) int {
		for i, line := range lines {
			if strings.Contains(line, needle) {
				return i
			}
		}
		return -1
	}
	current, runway, note, table := at("Current: "), at(fixtureRunwayLine), at("note: "), at("IDX ACCOUNT")
	if runway < 0 {
		t.Fatalf("a claimed forecast drew no runway line, or drew a different one:\n%s", m.Body())
	}
	if current < 0 || note < 0 || table < 0 {
		t.Fatalf("current=%d note=%d table=%d; the page is not the one this asserts about:\n%s",
			current, note, table, m.Body())
	}
	if runway != current+1 {
		t.Errorf("the runway line is at %d and the summary ends at %d; it belongs directly under the labels", runway, current)
	}
	if note != runway+4 || table != note+1 {
		t.Errorf("runway=%d note=%d table=%d; want the note after four runway rows and before the column header",
			runway, note, table)
	}
}

// The runway line is cut to the frame like every other line, with a cue that
// says so. This is the longest line the page can carry — an absolute moment, a
// span, and two more clauses after them — so it is the first one to overrun a
// narrow terminal.
//
// The row count is the assertion that matters, and the width check beside it is
// NOT a substitute for it: measured, an overlong line does not render overlong.
// A bordered lipgloss box SOFT-WRAPS content too wide for it, so the line comes
// out as two rows that are each inside the frame, every width assertion passes,
// and the page is one row taller than the height ladder budgeted for. At 43
// columns the wrap was reproduced before this was written. Twenty rows is where
// it bit: with the four accounts this pool then had and a runway line the ladder
// dropped the figure block and landed on exactly twenty, so one wrapped row was
// one row over.
//
// The heights walk the rungs the line is taken away at, not just the one it
// survives -- and they are kept where they were rather than re-derived for the
// taller fleet, because what each of them tests is the AGREEMENT below and not a
// particular rung: the equality holds at every height, and a set of heights that
// straddles several rungs is what makes it worth asserting. A page rendered at
// one height pins the renderer where the ladder's verdict happens to be true,
// and a renderer that consulted the WORDING rather than the plan — drawing
// whenever the line is non-empty — passes that single assertion while putting
// four rows of runway into a page that budgeted none. That is the same one-row
// overrun as the wrap above, arriving from the other side, and the equality
// below is what tells the two apart: the page draws the line exactly when the
// plan budgeted rows for it.
//
// This is also the one line on the page whose bytes above 0x7F came from a
// VALUE rather than from this package's own vocabulary: the shared wording
// separates its clauses with U+00B7, one display column wide, which is what the
// width functions here measure and what the count below compares against. The
// frame, the gauge and the state column are drawn from the glyph set and are
// this package's own choice; this line's characters are whoever computed the
// forecast's.
func TestTheRunwayLineIsCutToTheFrameRatherThanWrappingIt(t *testing.T) {
	for _, height := range []int{20, 19, 16, 12} {
		for _, w := range []int{113, 80, 56, 43, 35} {
			m := fixtureModel(w, height)
			m.Snap.Forecast, m.Snap.HasForecast = fixtureFleet(), true
			body := m.Body()
			if strings.ContainsRune(body, 0x1b) {
				t.Fatalf("at %dx%d the page with a runway line carries an escape byte", w, height)
			}
			lines := strings.Split(body, "\n")
			if len(lines) > height {
				t.Errorf("at %dx%d the page with a runway line is %d rows, %d more than the terminal has:\n%s",
					w, height, len(lines), len(lines)-height, body)
			}
			for i, line := range lines {
				if got := ansi.StringWidth(line); got > w {
					t.Errorf("at %dx%d line %d is %d columns wide: %q", w, height, i, got, line)
				}
			}
			// The plan is asked of the MODEL rather than restated here from
			// its parts. This case used to spell out every argument Body
			// passes, which made it a second copy of that call -- and a copy
			// that had already drifted: it handed the ladder a trailer length
			// of zero, so the day the page drew one it would have compared the
			// render against a layout nobody rendered.
			want := m.plan().Runway
			if got := strings.Contains(body, "Runway: "); got != want {
				t.Errorf("at %dx%d the page draws a runway line: %v; the height ladder budgeted a row for one: %v:\n%s",
					w, height, got, want, body)
			}
		}
	}
}

// Each runway fact owns a row, so the basis remains visible without competing
// with either quota axis for horizontal space.
//
// 25 rows and not the 24 of the design target: the runway block is four rows,
// and at 24 the ladder has already given it up on a five-account fleet under
// two section headings.
func TestTheRunwayRowsKeepTheirBasisVisible(t *testing.T) {
	m := fixtureModel(80, 25)
	m.Snap.Forecast, m.Snap.HasForecast = fixtureFleet(), true

	body := m.Body()
	if !strings.Contains(body, "         basis 3h51m") {
		t.Errorf("the runway rows omitted their evidence:\n%s", body)
	}
}

// The zone the page prints a moment in is the Snapshot's, not this process's.
//
// Nothing in this package reads the environment, so there is no time.Local to
// reach for here: the caller read the clock and its location came with it. The
// assertion is the whole point of that rule — a page that resolved the zone
// itself would render the author's hour in the author's terminal and a
// different hour in CI, where nothing sets TZ, and no fixture could pin either.
func TestTheRunwayLineRendersInTheSnapshotsOwnZone(t *testing.T) {
	kst := time.FixedZone("KST", 9*3600)
	m := fixtureModel(113, 24)
	m.Snap.Now = fixtureNow.In(kst)
	m.Snap.Forecast, m.Snap.HasForecast = fixtureFleet(), true

	body := m.Body()
	// fixtureFleet's dry moment is 2026-03-06 14:00 UTC, which is 23:00 the
	// same day nine hours east.
	if !strings.Contains(body, "260306 23:00") {
		t.Fatalf("the page did not render the dry moment in the Snapshot's zone:\n%s", body)
	}
	if strings.Contains(body, "KST") || strings.Contains(body, "UTC") {
		t.Fatalf("the compact dashboard timestamp retained a zone suffix:\n%s", body)
	}
}

// The count of what is off the page says WHICH WAY, and that is a change this
// commit makes on purpose rather than a glyph substitution. window() slices,
// so the rows it is not showing can be above the window as easily as below it
// -- press j to the bottom of a long list and every hidden row is above -- and
// a bare "+2 more" reads as "below" to everyone who has ever seen one.
func TestTheCountOfHiddenRowsSaysWhichWayTheyLie(t *testing.T) {
	for _, tc := range []struct {
		name string
		top  int
		want string
	}{
		{"at the top, everything hidden is below", 0, UnicodeGlyphs.MoreBelow + " +4 more  (j/k)"},
		{"scrolled to the bottom, everything hidden is above", 4, UnicodeGlyphs.MoreAbove + " +4 more  (j/k)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fixtureModel(80, 5)
			m.Top = tc.top
			if body := m.Body(); !strings.Contains(body, tc.want) {
				t.Fatalf("the scrolled page does not carry %q:\n%s", tc.want, body)
			}
		})
	}
}

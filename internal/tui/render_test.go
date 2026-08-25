package tui

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// The version in a fixture is a fixed constant and never the build stamp, or
// every release reddens the golden.
const fixtureVersion = "v0.2.0"

var fixtureAFullPage = fixture(`
+---------------------------------------------------------------------------------------------------------------+
|  ___ ___ ___          _    _                                                                                  |
| / __/ __|   \ __ _ __| |__| |_  _                                                                             |
|| (_| (__| |) / _' / _' / _' | || |                                                                            |
| \___\___|___/\__,_\__,_\__,_|\_, |                                                                            |
|                              |__/   ccdad v0.2.0                                                              |
|                                                                                                               |
|Quota down again? You were 'Yap'-ping.                                                                         |
|                      -- 'Daddy' Daemon                                                                        |
|                                                                                                               |
|  ____     ____     ____             ________                                                                  |
| / oo \   / oo \   / oo \           | o    o |                                                                 |
||  __  | |  __  | |  __  |         _|__ >< __|_                                                                |
|| |  | | | |  | | | |  | |        |   ~~~~~~~  |                                                               |
||_|  |_| |_|  |_| |_|  |_|        |___________|                                                                |
|  ||||     ||||     ||||             ||     ||                                                                 |
|                                                                                                               |
|Active: work@example.com (work)  |  Strategy: headroom  |  Mode: headroom                                      |
|IDX ACCOUNT               TYPE          USED               WINDOW          RESETS IN  STATE        AUTO        |
|* 1 work@example.com (..  subscription  [#########.]  87%  five_hour       1h22m      * active     yes         |
|  2 enterprise@co.exam..  credit        [##########] 100%  seven_day_opus  5m         ! exhausted  no          |
|  3 spare@example.com     subscription  [#####.....]  52%  five_hour       4h11m      + candidate  yes         |
|  4 keyonly@example.co..  api-key       ?                  -               -          ? unknown    yes         |
|                                                                                                               |
|a add  s switch  d daemon  c strategy  q quit  l list                      Daemon: running (pid 8123, up 2h05m)|
+---------------------------------------------------------------------------------------------------------------+`)

var fixtureBDesignTarget = fixture(`
+------------------------------------------------------------------------------+
|  ___ ___ ___          _    _                                                 |
| / __/ __|   \ __ _ __| |__| |_  _                                            |
|| (_| (__| |) / _' / _' / _' | || |                                           |
| \___\___|___/\__,_\__,_\__,_|\_, |                                           |
|                              |__/   ccdad v0.2.0                             |
|                                                                              |
|Quota down again? You were 'Yap'-ping.                                        |
|                      -- 'Daddy' Daemon                                       |
|                                                                              |
|Active: work@example.com (work)  |  Strategy: headroom  |  Mode: headroom     |
|IDX ACCOUNT               USED               RESETS IN  STATE        AUTO     |
|* 1 work@example.com (..  [#########.]  87%  1h22m      * active     yes      |
|  2 enterprise@co.exam..  [##########] 100%  5m         ! exhausted  no       |
|  3 spare@example.com     [#####.....]  52%  4h11m      + candidate  yes      |
|  4 keyonly@example.co..  ?                  -          ? unknown    yes      |
|                                                                              |
|a add  s switch  d daemon  c strategy  q quit  l list      Daemon: not running|
+------------------------------------------------------------------------------+`)

var fixtureCShort = fixture(`
+------------------------------------------------------------------------------+
|ccdad v0.2.0                                                                  |
|                                                                              |
|Active: work@example.com (work)  |  Strategy: headroom  |  Mode: headroom     |
|IDX ACCOUNT               USED               RESETS IN  STATE        AUTO     |
|* 1 work@example.com (..  [#########.]  87%  1h22m      * active     yes      |
|  2 enterprise@co.exam..  [##########] 100%  5m         ! exhausted  no       |
|  3 spare@example.com     [#####.....]  52%  4h11m      + candidate  yes      |
|  4 keyonly@example.co..  ?                  -          ? unknown    yes      |
|                                                                              |
|a add  s switch  d daemon  c strategy  q quit  l list          Daemon: unknown|
+------------------------------------------------------------------------------+`)

var fixtureDNarrow = fixture(`
ccdad v0.2.0

Active: work@example.com (work)  |  Strategy: headroom..
IDX ACCOUNT               USED               RESETS IN
* 1 work@example.com (..  [#########.]  87%  1h22m    
  2 enterprise@co.exam..  [##########] 100%  5m       
  3 spare@example.com     [#####.....]  52%  4h11m    
  4 keyonly@example.co..  ?                  -        

a add  s switch  d daemon  c strategy .. Daemon: unknown`)

var fixtureECollapsed = fixture(`
ccdad v0.2.0
Active: work@example.com (work)  |  Strat..
IDX ACCOUNT               USED  RESETS IN
* 1 work@example.com (..  87%   1h22m    
  2 enterprise@co.exam..  100%  5m       
  3 spare@example.com     52%   4h11m    
  4 keyonly@example.co..  ?     -        
a add  s switch  d daemon ..Daemon: unknown`)

var fixtureFWithNotice = fixture(`
+------------------------------------------------------------------------------+
|  ___ ___ ___          _    _                                                 |
| / __/ __|   \ __ _ __| |__| |_  _                                            |
|| (_| (__| |) / _' / _' / _' | || |                                           |
| \___\___|___/\__,_\__,_\__,_|\_, |                                           |
|                              |__/   ccdad v0.2.0                             |
|                                                                              |
|Quota down again? You were 'Yap'-ping.                                        |
|                      -- 'Daddy' Daemon                                       |
|                                                                              |
|Active: work@example.com (work)  |  Strategy: headroom  |  Mode: headroom     |
|note: hover thresholds could not be read                                      |
|IDX ACCOUNT               USED               RESETS IN  STATE        AUTO     |
|* 1 work@example.com (..  [#########.]  87%  1h22m      * active     yes      |
|  2 enterprise@co.exam..  [##########] 100%  5m         ! exhausted  no       |
|  3 spare@example.com     [#####.....]  52%  4h11m      + candidate  yes      |
|  4 keyonly@example.co..  ?                  -          ? unknown    yes      |
|                                                                              |
|a add  s switch  d daemon  c strategy  q quit  l list          Daemon: unknown|
+------------------------------------------------------------------------------+`)

var fixtureGZeroAccounts = fixture(`
+------------------------------------------------------------------------------+
|  ___ ___ ___          _    _                                                 |
| / __/ __|   \ __ _ __| |__| |_  _                                            |
|| (_| (__| |) / _' / _' / _' | || |                                           |
| \___\___|___/\__,_\__,_\__,_|\_, |                                           |
|                              |__/   ccdad v0.2.0                             |
|                                                                              |
|Active: work@example.com (work)  |  Strategy: headroom  |  Mode: headroom     |
|IDX ACCOUNT               USED  RESETS IN  STATE  AUTO                        |
|    no accounts                                                               |
|                                                                              |
|a add  s switch  d daemon  c strategy  q quit  l list          Daemon: unknown|
+------------------------------------------------------------------------------+`)

// fixture is the page a raw literal below holds. The literal starts with a
// newline because the alternative is a first row jammed against the backtick,
// where nothing lines up and a missing leading space is invisible; this takes
// that newline back off.
func fixture(page string) string { return strings.TrimPrefix(page, "\n") }

// The seven pages below were pasted from the compiled renderer, not written
// by hand and not edited toward what they were expected to say: a golden a
// human wrote is a golden that agrees with whoever wrote it last. Regenerating
// one means printing Body() at that size and pasting the bytes back.
//
// THE FIXTURE-DIFF LEDGER. The reference blocks these pages replace were
// drawn by a probe that predates the width ladder, the height ladder and the
// hand-transcribed chrome. Every differing byte was measured against those
// three and falls into one of five classes, none of which is a renderer bug:
//
//  1. AMENDED ON PURPOSE, and stated as such. One row is inserted directly
//     above the column header -- Active, Strategy, and Mode when the pass
//     Decided -- so every page is one row taller than its reference. The
//     daemon footer takes the wording `ccdad daemon status` already prints,
//     so "running  pid 8123  up 2h05m" is "running (pid 8123, up 2h05m)".
//  2. THE REFERENCE IS STALE ON ACCOUNT WIDTH. Its 80-column blocks render
//     ACCOUNT at 23 columns. The width ladder gives 20 and the ladder is
//     right: the reference was drawn by the probe that still had the defect
//     where WIDENING a terminal narrowed the address column, and holding
//     ACCOUNT at its comfort width is the fix for it.
//  3. THE REFERENCE IS STALE ON THE HEIGHT-DROP ORDER. Its 56- and
//     43-column blocks keep the frame and drop the title and the blanks
//     instead. The height ladder drops the border at rung 5, ahead of the
//     blanks at 6 and the title at 7, so both pages are frameless here --
//     and, having no frame, two columns wider inside, which is what moves
//     the footer's right edge with them.
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

// The five fixtures are the acceptance criterion. They are compared whole
// rather than by keyword: a near-miss passes any "contains a table" check and
// then drifts on the first change to either side.
func TestThePageRendersByteForByteAtEveryLadderRung(t *testing.T) {
	for _, tc := range []struct {
		name          string
		width, height int
		want          string
	}{
		{"the full page with every column", 113, 26, fixtureAFullPage},
		{"the design target, figures dropped", 80, 24, fixtureBDesignTarget},
		{"wordmark and tagline dropped", 80, 13, fixtureCShort},
		{"the frame dropped", 56, 10, fixtureDNarrow},
		{"the gauge collapsed", 43, 9, fixtureECollapsed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := fixtureModel(tc.width, tc.height).Body()
			if got != tc.want {
				t.Fatalf("Body() at %dx%d:\ngot:\n%s\nwant:\n%s", tc.width, tc.height, got, tc.want)
			}
		})
	}
}

// Every page fits in the terminal it was planned for. The height ladder
// computes a budget and the renderer spends it, and nothing else compares the
// two -- a block emitted outside the budget, or a rung that saves the wrong
// number of rows, shows up here and nowhere else.
func TestEveryFixtureFitsTheTerminalItWasPlannedFor(t *testing.T) {
	for _, tc := range []struct{ w, h int }{{113, 26}, {80, 24}, {80, 13}, {56, 10}, {43, 9}, {80, 20}, {80, 5}, {35, 3}} {
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
func TestThePageNeverScrollsHorizontally(t *testing.T) {
	for _, tc := range []struct{ w, h int }{{113, 26}, {80, 24}, {80, 13}, {56, 10}, {43, 9}, {35, 3}} {
		for i, line := range strings.Split(fixtureModel(tc.w, tc.h).Body(), "\n") {
			if got := ansi.StringWidth(line); got > tc.w {
				t.Errorf("at %dx%d line %d is %d columns wide: %q", tc.w, tc.h, i, got, line)
			}
		}
	}
}

// The plain path emits no escape byte at all, which is what makes an ANSI-free
// fixture possible. The width functions still report the intended widths with
// the colours unset, so the layout is the same one a coloured render produces.
func TestThePlainPathEmitsNoEscapeBytes(t *testing.T) {
	if strings.ContainsRune(fixtureModel(113, 26).Body(), 0x1b) {
		t.Fatal("the plain render carries an escape byte, so no fixture here can be trusted")
	}
}

// The list toggle changes the columns AND the heading, because the two
// percentages run opposite ways: status prints how much is spent, list prints
// how much is left. One heading carrying two polarities is the drift the two
// tables have avoided since they were written.
func TestTheListToggleSwapsTheHeadingWithThePolarity(t *testing.T) {
	status := fixtureModel(113, 26).Body()
	m := fixtureModel(113, 26)
	m.Set = SetList
	list := m.Body()
	if !strings.Contains(status, "USED") || strings.Contains(status, "LEFT") {
		t.Error("the status set does not head its column USED")
	}
	if !strings.Contains(list, "LEFT") || strings.Contains(list, "USED") {
		t.Error("the list set does not head its column LEFT")
	}
	if !strings.Contains(list, "TIER") {
		t.Error("the list set does not carry TIER, which is the column it exists for")
	}
}

// The floors. Below them the page says what it needs, rather than rendering
// something unreadable or panicking on a negative width.
func TestBelowTheFloorsThePageRendersWhatItNeeds(t *testing.T) {
	if got := fixtureModel(30, 24).Body(); !strings.Contains(got, "ccdad needs 35 columns") {
		t.Errorf("at 30 columns: %q", got)
	}
	if got := fixtureModel(80, 2).Body(); !strings.Contains(got, "ccdad needs 3 rows") {
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
	if !strings.Contains(body, "+2 more") {
		t.Fatalf("two accounts fell off the page and nothing said so:\n%s", body)
	}
	if !strings.Contains(body, "(j/k)") {
		t.Fatalf("the count does not say how to reach the rest:\n%s", body)
	}
	if !strings.Contains(body, "work@example.com") {
		t.Fatalf("the rung spent the page on a count and drew no accounts:\n%s", body)
	}
}

// With room for exactly one row the trade inverts: the account wins and the
// count is what goes. A header, a count of four and no accounts at all has
// stopped being a dashboard -- and j/k, which the count advertises, would have
// had nothing to move through.
func TestWithRoomForOneRowTheAccountWinsAndTheCountGoes(t *testing.T) {
	body := fixtureModel(35, 3).Body()
	lines := strings.Split(body, "\n")
	if len(lines) != 3 {
		t.Fatalf("at 35x3 the page is %d rows:\n%s", len(lines), body)
	}
	if strings.Contains(body, "more") {
		t.Fatalf("the one row left was spent on a count instead of an account:\n%s", body)
	}
	// The address is cut to the ACCOUNT column's hard floor at this width, so
	// the head is what there is to look for -- which is the half the
	// head-preserving truncation exists to keep.
	if !strings.Contains(body, "work@examp") {
		t.Fatalf("the never-dropped account row was dropped:\n%s", body)
	}
}

// An account no daemon has ever published, an account with no reading, and an
// account at 100% all appear in the fixtures. This names the ones the fixtures
// would silently stop covering if the placeholder data were edited.
func TestTheFixtureDataStillCoversTheUnreadableRowAndTheEmptyState(t *testing.T) {
	var unreadable, full, unknownToTheEngine bool
	for _, r := range fixtureRows() {
		switch r.UsedLabel() {
		case view.Unreadable:
			unreadable = true
		case "100%":
			full = true
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
// notice rung is dropped SECOND, before the tagline is even considered, so at
// 13 rows a notice never survives to be rendered at all. Twenty is the
// shortest terminal that keeps one. A fixture at 13 would have pinned the
// absence and called it the presence.
func TestANonEmptyNoticeRendersDirectlyAboveTheColumnHeader(t *testing.T) {
	m := fixtureModel(80, 20)
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
	if got != fixtureFWithNotice {
		t.Fatalf("Body() with one notice:\ngot:\n%s\nwant:\n%s", got, fixtureFWithNotice)
	}
}

// More notices than the one line can carry are counted rather than dropped: a
// page that shows the first of four and says nothing about the other three
// tells a user the first is all there was.
func TestMoreNoticesThanFitAreCountedRatherThanDropped(t *testing.T) {
	m := fixtureModel(80, 20)
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
	quiet := fixtureModel(80, 20)
	quiet.Snap.Notices = nil
	got := quiet.Body()
	if strings.Contains(got, "note:") {
		t.Fatalf("an empty Notices rendered a note line anyway:\n%s", got)
	}
	var want []string
	for _, line := range strings.Split(fixtureFWithNotice, "\n") {
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
	if got != fixtureGZeroAccounts {
		t.Fatalf("Body() with zero accounts:\ngot:\n%s\nwant:\n%s", got, fixtureGZeroAccounts)
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
	if !strings.Contains(got, figures[0]) {
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
// account fully spent and held out of rotation, one idle alternate, and one
// api-key seat that could not be read at all.
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
			Headroom: strategy.Headroom{Known: true, Binding: usage.WindowFiveHour, Pct: 13, Slack: -7, Threshold: 80},
			Engine:   daemon.AccountStatus{UUID: "d3b07384-d9a0-4f9b-9a2c-5f0c3a1e7b21", State: daemon.StateActive},
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
				HasFloor: true, Floor: usage.WindowSevenDayOpus,
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
			Headroom: strategy.Headroom{Known: true, Binding: usage.WindowFiveHour, Pct: 48, Slack: 28, Threshold: 80},
			Engine:   daemon.AccountStatus{UUID: "7f2e1b90-3c45-4a6d-9e08-b1d7c4a5f6e2", State: daemon.StateCandidate},
		},
		{
			Account: store.Account{
				UUID:  "b6a4c2d0-8e15-4f37-a9c6-3d5b7e1f0a48",
				Email: "keyonly@example.com", Alias: "key", Idx: 4,
				Kind: identity.KindAPIKey,
			},
			Engine: daemon.AccountStatus{UUID: "b6a4c2d0-8e15-4f37-a9c6-3d5b7e1f0a48", State: daemon.StateUnknown},
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

func fixtureSnapshot(report daemon.Report) view.Snapshot {
	return view.Snapshot{
		Now:         fixtureNow,
		Rows:        fixtureRows(),
		Report:      report,
		ActiveLabel: "work@example.com (work)",
		Strategy:    "headroom",
		Mode:        strategy.ModeHeadroom,
		HasMode:     true,
		Version:     fixtureVersion,
	}
}

func fixtureModel(width, height int) Model {
	return newModel(fixtureSnapshot(fixtureReport(width, height)), width, height)
}

func fixtureOptions() Options {
	snap := fixtureSnapshot(fixtureReport(113, 26))
	return Options{
		Load: func(time.Time) (view.Snapshot, error) { return snap, nil },
		Now:  func() time.Time { return fixtureNow },
		Out:  io.Discard,
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
		if strings.Contains(line, cursorMark+" 2 ") {
			marked = append(marked, line)
		}
	}
	if len(marked) != 1 {
		t.Fatalf("the cursor is drawn on %d rows, want exactly the one it is on:\n%s", len(marked), m.Body())
	}
	if strings.Count(m.Body(), cursorMark+" ") != 1 {
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
	if strings.Contains(body, cursorMark+" 1 ") {
		t.Fatalf("the cursor took the live account's marker:\n%s", body)
	}
}

// Nobody is pointing at anything in a pipe. The one-shot render answers
// `ccdad tui > file` and bare `ccdad` off a terminal, and a selection marker
// there was put on the row by a reader who is not present.
func TestTheOneShotRenderDrawsNoCursor(t *testing.T) {
	// The live account is deliberately NOT the first row here: with the marker
	// suppressed the first row would otherwise be indistinguishable from a row
	// that simply has no marks on it, and the test would pass for a bug.
	snap := fixtureSnapshot(fixtureReport(113, 26))
	snap.Rows[0].Active = false
	snap.Rows[2].Active = true

	page, err := Render(Options{
		Load: func(time.Time) (view.Snapshot, error) { return snap, nil },
		Now:  func() time.Time { return fixtureNow },
		Out:  io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, cursorMark+" ") {
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
	m.Top, m.Cursor = 2, 3
	body := m.Body()
	if !strings.Contains(body, cursorMark+" 4 ") {
		t.Fatalf("the cursor is not on the row it indexes:\n%s", body)
	}
	if strings.Contains(body, cursorMark+" 3 ") {
		t.Fatalf("the cursor is drawn against the screen position rather than the row:\n%s", body)
	}
}

// The header names the strategy in FORCE, and under hover that is not the one in
// the file: strategy.Options' withHover pass overrides the key. Printing the
// file's value there is the defect this test exists for -- a reader who had set
// consume-first saw consume-first and concluded hover was off.
func TestTheHeaderNamesHoverRatherThanTheStrategyHoverOverrode(t *testing.T) {
	snap := fixtureSnapshot(fixtureReport(113, 26))
	snap.Strategy, snap.Hover = "consume-first", true

	page := newModel(snap, 113, 26).Body()
	if !strings.Contains(page, "Strategy: hover") {
		t.Errorf("the header does not name hover:\n%s", page)
	}
	if strings.Contains(page, "consume-first") {
		t.Errorf("the header prints a strategy hover overrode:\n%s", page)
	}
}

// The picker still marks the configured strategy while hover is on, and that is
// the reason Snapshot keeps that value beside the label rather than being
// overwritten with "hover". Setting the key under hover is a legitimate "set it
// for later" -- `ccdad config list` marks it overriding -- and a picker with
// nothing marked would have been this fix's own regression.
func TestTheStrategyPickerStillMarksTheConfiguredEntryUnderHover(t *testing.T) {
	snap := fixtureSnapshot(fixtureReport(113, 26))
	snap.Strategy, snap.Hover = "consume-first", true
	o := fixtureOptions()
	o.Load = func(time.Time) (view.Snapshot, error) { return snap, nil }

	a := appAt(t, o, 113, 26)
	a, _, _ = a.key(keyPress("c"))
	if a.pick.current < 0 {
		t.Fatal("the strategy picker marks nothing as current under hover")
	}
	if got := a.pick.items[a.pick.current].label; got != "consume-first" {
		t.Errorf("the picker marks %q as current, want the configured consume-first", got)
	}
}

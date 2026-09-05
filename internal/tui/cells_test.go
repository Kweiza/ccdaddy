package tui

import (
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/theme"
	"github.com/Kweiza/ccdaddy/internal/usage"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// AccountState is a STRING type, so a switch with no default falls out of every
// case and leaves the caller holding its zero value -- which reads as active.
// The document contract is additive and guarantees a newer daemon publishing a
// state this binary has never heard of, on upgrade day.
//
// stateCell's role IS asserted, in the test below, and that is new: it used to
// be a lipgloss.Style, which carries a []color.Color field and a func field and
// therefore has no ==, so a test could only ever check that two styles were
// both the zero value -- which is to say it could check nothing at all while
// every style var in cells.go was unset. It is a theme.Role now, which is an
// int.
//
// The WORD is view.StateLabel's and is asserted through the cell the page
// draws, because that is where the two halves meet: this page owns the glyph in
// front and internal/view owns everything after it, and a test of either alone
// would pass on a page that had stopped putting them together.
func TestAStateThisBinaryHasNeverHeardOfIsCarriedThroughAndNeverReadsAsActive(t *testing.T) {
	got := stateCellOf(t, daemon.AccountState("draining"))
	if want := UnicodeGlyphs.Unknown + " draining"; got != want {
		t.Errorf("the state cell is %q, want %q", got, want)
	}
}

// pageCell is what the dashboard draws in one column of one row.
//
// It goes through Model.cell rather than through a view.Row method directly,
// and that is the whole point of it: the cells are internal/view's now, so a
// test that called the method would assert what internal/view already asserts
// and would stay green on a page that had stopped drawing that column, or drew
// it from the wrong kind.
//
// Nobody is pointing at the row -- the cursor decoration has its own tests up
// in render_test.go -- and ACCOUNT is given the comfort width, so a label short
// enough to fit comes back with only its padding added.
func pageCell(t *testing.T, k view.ColumnKind, r view.Row) string {
	t.Helper()
	m := fixtureModel(113, 26)
	m.Cursor = noCursor
	return m.cell(view.ListColumn{Kind: k, Index: -1},
		view.ListRow{Row: r, At: 0}, Layout{AccountWide: accountComfort}, m.Cols)
}

// stateCellOf is the dashboard's STATE cell for one engine state: the shared
// cell function's word, with this page's glyph in front of it.
func stateCellOf(t *testing.T, s daemon.AccountState) string {
	t.Helper()
	return pageCell(t, view.ColumnState, view.Row{Engine: daemon.AccountStatus{State: s}})
}

// Which colour each state will be painted in, asserted by VALUE, one commit
// before anything paints it. This is the whole reason stateCell hands back a
// role rather than a style: the mapping from a state to its emphasis is a
// decision, and a decision that no test can compare is a decision that drifts.
//
// StateEmpty and StateExhausted share a role deliberately -- both mean "there
// is nothing left here", and the WORD beside the glyph is what tells them
// apart. Disabled, Unknown, the never-published empty state and the
// unrecognised fallthrough all share the muted role for the same kind of
// reason: none of them is a quota fact, and painting four different colours for
// four flavours of "no engine opinion" spends the reader's attention on the
// column that has the least to say.
func TestEveryStateNamesTheRoleItWillBePaintedIn(t *testing.T) {
	for _, tc := range []struct {
		state daemon.AccountState
		role  theme.Role
	}{
		{daemon.StateActive, theme.RoleActive},
		{daemon.StateCandidate, theme.RoleCandidate},
		{daemon.StateExhausted, theme.RoleExhausted},
		{daemon.StateEmpty, theme.RoleExhausted},
		{daemon.StateQuarantined, theme.RoleQuarantined},
		{daemon.StateServing, theme.RoleActive},
		{daemon.StateNeedsRelogin, theme.RoleQuarantined},
		{daemon.StateDisabled, theme.RoleMuted},
		{daemon.StateUnknown, theme.RoleMuted},
		{"", theme.RoleMuted},
		{daemon.AccountState("draining"), theme.RoleMuted},
	} {
		if _, got := stateCell(UnicodeGlyphs, tc.state); got != tc.role {
			t.Errorf("stateCell(%q) names role %d, want role %d", tc.state, got, tc.role)
		}
	}
}

// The empty state is real: AccountStatus.State is json:"state,omitempty" and is
// filled from a map lookup that returns the zero value on a miss, so an account
// no daemon has ever published carries "".
func TestAnAccountNoDaemonHasEverPublishedRendersADashAndNoGlyph(t *testing.T) {
	if glyph, _ := stateCell(UnicodeGlyphs, ""); glyph != "" {
		t.Errorf("stateCell(\"\") draws the glyph %q over an absence", glyph)
	}
	if got := stateCellOf(t, ""); got != "-" {
		t.Fatalf("the state cell of an unpublished account is %q, want \"-\"", got)
	}
}

// The eight states enumerated here each render a cell nothing else renders, and
// the count is asserted so that deleting an arm is not a silent narrowing. The
// two codex states are in the list for the reason the other six are: serving
// takes the ACTIVE glyph and needs-relogin the QUARANTINED one, so the word is
// all that separates each from its neighbour and a shared cell would be
// invisible.
func TestEveryAccountStateHasItsOwnCell(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range []daemon.AccountState{
		daemon.StateActive, daemon.StateCandidate, daemon.StateExhausted,
		daemon.StateQuarantined, daemon.StateDisabled, daemon.StateUnknown,
		daemon.StateServing, daemon.StateNeedsRelogin,
	} {
		cell := stateCellOf(t, s)
		if !strings.HasSuffix(cell, " "+string(s)) {
			t.Errorf("the state cell of %q is %q, want the state's own name after the glyph", s, cell)
		}
		if seen[cell] {
			t.Errorf("two states render the same cell %q", cell)
		}
		seen[cell] = true
	}
	if len(seen) != 8 {
		t.Fatalf("eight named states rendered %d distinct cells", len(seen))
	}
}

// The mockup's per-row "Best"/"Nearest" name strategies that exist nowhere in
// this tree. The column is a rotation policy and carries two strings.
func TestAutoIsARotationPolicyAndNotAStrategyName(t *testing.T) {
	if got := pageCell(t, view.ColumnAuto, enabledRow()); got != "yes" {
		t.Errorf("the AUTO cell of an enabled account = %q, want \"yes\"", got)
	}
	if got := pageCell(t, view.ColumnAuto, disabledRow()); got != "no" {
		t.Errorf("the AUTO cell of a disabled account = %q, want \"no\"", got)
	}
}

// Head-preserving, because the head is where the local part of an address is
// and that is what tells two accounts apart. The cue is the page's own, not a
// literal spelled a second time here: it is two ASCII characters in both glyph
// sets, and it stays that way because a cue is emitted at a measured column
// boundary, where the Unicode ellipsis costs two columns on a machine in
// east-asian width mode and one everywhere else.
func TestALongAddressLosesItsTailAndKeepsItsHead(t *testing.T) {
	got := accountCell("enterprise@co.example.com", 22, UnicodeGlyphs.Cue)
	if len([]rune(got)) != 22 {
		t.Fatalf("accountCell(..., 22) is %d runes wide: %q", len([]rune(got)), got)
	}
	if !strings.HasSuffix(got, UnicodeGlyphs.Cue) || !strings.HasPrefix(got, "enterprise@") {
		t.Fatalf("accountCell = %q, want a kept head and a %q tail", got, UnicodeGlyphs.Cue)
	}
}

// The cells this package builds by hand carry no escape byte under the None
// theme, and the state cell's ROLE is now checked through a rendered style
// rather than through the text alone.
//
// The third return used to be dropped on the floor here, justified by a claim
// that lipgloss v2 has no auto-adaptive fallback. What v2 removed is the global
// renderer -- a Style consults nothing about the terminal on its own, so the
// background-darkness boolean is threaded in from the program -- and that claim
// was measured wrong: with a colour on the active state, this test went on
// passing. That is exactly the shape of gate that keeps passing after somebody
// starts painting the cell it was watching, so it renders the style now.
func TestThePlainPathEmitsNoEscapeByte(t *testing.T) {
	pal := theme.Of(theme.None)
	rows := []view.Row{rowAtPercent(0), rowAtPercent(87), rowAtPercent(100), rowWithNoEntry(), enabledRow(), disabledRow()}
	for _, r := range rows {
		for _, cell := range []string{
			r.WindowCell(usage.WindowFiveHour),
			pageCell(t, view.ColumnWorst, r),
			pageCell(t, view.ColumnAuto, r),
			pageCell(t, view.ColumnAccount, r),
		} {
			if strings.ContainsRune(cell, 0x1b) {
				t.Fatalf("cell %q carries an escape byte under the None theme", cell)
			}
		}
	}
	for _, s := range []daemon.AccountState{
		daemon.StateActive, daemon.StateCandidate, daemon.StateExhausted,
		daemon.StateQuarantined, daemon.StateDisabled, daemon.StateUnknown, "",
	} {
		_, role := stateCell(UnicodeGlyphs, s)
		if styled := pal.Style(role).Render(stateCellOf(t, s)); strings.ContainsRune(styled, 0x1b) {
			t.Fatalf("the state cell of %q carries an escape byte under the None theme", s)
		}
	}
}

// rowWithNoEntry is the first absence: no cache entry at all, so HasEntry is
// false and Reported never gets as far as looking at Headroom.
func rowWithNoEntry() view.Row {
	return view.Row{}
}

// rowWithUnknownHeadroom is the second absence: there is an entry, but no
// window reported a utilization, so Headroom.Known is false. Reported() checks
// this before ReportedName ever runs.
func rowWithUnknownHeadroom() view.Row {
	return view.Row{
		HasEntry: true,
		Entry:    usage.Entry{Snapshot: &usage.Snapshot{}},
		Headroom: strategy.Headroom{Known: false},
	}
}

// rowWithSilentWindow is a row whose binding window IS present -- Reported()
// finds it in AllWindows() -- but that window reported no utilization, so
// Percent() is the one that says no. usage.Window's tri-state fields are
// unexported, so NewWindow(nil, nil) is the only way to build a present window
// that read nothing, rather than an absent one.
func rowWithSilentWindow(name usage.WindowName) view.Row {
	snap := &usage.Snapshot{}
	setNamedWindow(snap, name, usage.NewWindow(nil, nil))
	return view.Row{
		HasEntry: true,
		Entry:    usage.Entry{Snapshot: snap},
		Headroom: strategy.Headroom{Known: true, Binding: name},
	}
}

// rowAtPercent is a row whose binding window read a real percentage -- the
// case usedCell must draw a bar for, including zero.
func rowAtPercent(pct float64) view.Row {
	return view.Row{
		HasEntry: true,
		Entry:    usage.Entry{Snapshot: &usage.Snapshot{FiveHour: usage.NewWindow(&pct, nil)}},
		Headroom: strategy.Headroom{Known: true, Binding: usage.WindowFiveHour},
	}
}

func enabledRow() view.Row  { return view.Row{Account: store.Account{Disabled: false}} }
func disabledRow() view.Row { return view.Row{Account: store.Account{Disabled: true}} }

// setNamedWindow assigns w to the one of Snapshot's five fixed fields name
// identifies. Snapshot has no map here -- each window is its own field, the
// schema's own shape -- so a generic-by-name test builder needs the switch.
func setNamedWindow(s *usage.Snapshot, name usage.WindowName, w usage.Window) {
	switch name {
	case usage.WindowFiveHour:
		s.FiveHour = w
	case usage.WindowSevenDay:
		s.SevenDay = w
	case usage.WindowSevenDayOAuthApps:
		s.SevenDayOAuthApps = w
	case usage.WindowSevenDayOpus:
		s.SevenDayOpus = w
	case usage.WindowSevenDaySonnet:
		s.SevenDaySonnet = w
	}
}

// The dashboard is the THIRD table that has to call a codex account codex, and
// it was the one nothing asserted. list and status are pinned end to end by
// their own tests, but those call view.Row.TypeLabel directly and never reached
// this page's TYPE cell -- so replacing the wrapper's body with
// r.Account.Kind.String() left this package green, every byte-compared golden
// page included.
//
// There is no wrapper left to break: the cell is view.Row.ListCell now. What
// can still go wrong is the wiring, and it is what this asks about -- a TYPE
// column drawn from the wrong kind, or a page that stopped drawing TYPE at all,
// answers something other than "codex" here.
//
// A UNIT test, kept even though fixtureRows() now carries a codex seat and the
// golden pages draw its TYPE cell. The two ask different questions: a page
// compares as one string and goes red for any reason at all, so it can tell you
// that something moved and never that this cell is the thing that moved.
func TestTheTypeCellCallsACodexRowCodex(t *testing.T) {
	codex := view.Row{Account: store.Account{
		Provider: provider.Codex,
		Kind:     identity.KindSubscription,
	}}
	if got := pageCell(t, view.ColumnType, codex); got != "codex" {
		t.Errorf("the TYPE cell of a codex row = %q, want codex", got)
	}

	claude := view.Row{Account: store.Account{
		Provider: provider.Claude,
		Kind:     identity.KindSubscription,
	}}
	if got := pageCell(t, view.ColumnType, claude); got == "codex" {
		t.Errorf("the TYPE cell called a claude row codex")
	}
}

// The collapsed block keeps the absence rule the gauge used to keep. It is the
// one cell a terminal too narrow for the window block gets, and it must not
// turn "nobody could read this account" into a percentage.
func TestTheCollapsedBlockKeepsTheAbsenceRule(t *testing.T) {
	if got := pageCell(t, view.ColumnWorst, rowWithNoEntry()); got != view.Unreadable {
		t.Errorf("the collapsed cell of an unread row = %q, want %q", got, view.Unreadable)
	}
}

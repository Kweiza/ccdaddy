package view

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// listRow is one fully-populated account: a live Claude seat with two readable
// windows, an alias, a tier, an engine state and a reading four minutes old.
// Every column below has something to say about it, which is what makes the
// one-cell-per-kind table a complete test rather than a sample.
func listRow() Row {
	r := fleetRow("u-1", 87, 42, nil, 0)
	r.Account = store.Account{
		UUID: "u-1", Email: "work@example.com", Alias: "work", Idx: 3,
		Kind: identity.KindSubscription, Tier: "claude_max",
	}
	r.Active = true
	r.Entry.FetchedAt = colNow.Add(-4 * time.Minute)
	r.Engine = daemon.AccountStatus{UUID: "u-1", State: daemon.StateActive}
	return r
}

func kinds(cols []ListColumn) []string {
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		out = append(out, c.Kind.String())
	}
	return out
}

func listHeaders(cols []ListColumn) []string {
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		out = append(out, c.Header)
	}
	return out
}

// subsequence reports whether want appears in got in order, gaps allowed.
func subsequence(got, want []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

// ---- the column set ---------------------------------------------------------

func TestListColumnsIsOneFixedOrderForEverySurface(t *testing.T) {
	c := ColumnsOf([]Row{listRow()})
	got := kinds(ListColumns(c))
	want := []string{
		"IDX", "ACCOUNT", "TYPE", "TIER",
		"WINDOW", "WINDOW", "RESET", "RESET",
		"STATE", "AUTO", "AGE",
	}
	if !eq(got, want) {
		t.Fatalf("ListColumns = %v, want %v", got, want)
	}
	if got, want := listHeaders(ListColumns(c)), []string{
		"IDX", "ACCOUNT", "TYPE", "TIER", "5H", "7D", "5H IN", "7D IN", "STATE", "AUTO", "AGE",
	}; !eq(got, want) {
		t.Errorf("headers = %v, want %v", got, want)
	}
}

// The point of one order is that neither surface has to reorder anything a
// reader has already learned. Both of today's orders have to survive inside it
// as subsequences, or adopting it moves a column sideways.
func TestListColumnsHoldsEachSurfacesOwnOrderAsASubsequence(t *testing.T) {
	got := kinds(ListColumns(ColumnsOf([]Row{listRow()})))
	for _, tc := range []struct {
		surface string
		want    []string
	}{
		{"ccdad status", []string{"IDX", "ACCOUNT", "TYPE", "TIER", "WINDOW", "RESET", "AGE"}},
		{"the dashboard", []string{"IDX", "ACCOUNT", "TYPE", "WINDOW", "RESET", "STATE", "AUTO", "AGE"}},
	} {
		if !subsequence(got, tc.want) {
			t.Errorf("%s draws %v, which is not a subsequence of %v", tc.surface, tc.want, got)
		}
	}
}

// The quota block is never empty. A table that simply stopped having quota
// columns reads as a build with no quota feature rather than as a fleet nobody
// could read, which is the answer Columns.Headers has given since it was
// written.
func TestAFleetWithNoReadableWindowStillGetsOneQuotaColumn(t *testing.T) {
	cols := ListColumns(ColumnsOf([]Row{{Account: store.Account{UUID: "u-1"}}}))
	if got, want := kinds(cols), []string{
		"IDX", "ACCOUNT", "TYPE", "TIER", "WINDOW", "STATE", "AUTO", "AGE",
	}; !eq(got, want) {
		t.Fatalf("ListColumns = %v, want %v", got, want)
	}
	var quota ListColumn
	for _, c := range cols {
		if c.Kind == ColumnWindow {
			quota = c
		}
	}
	if quota.Header != PlaceholderHeader {
		t.Errorf("the placeholder header = %q, want %q", quota.Header, PlaceholderHeader)
	}
	if quota.Index != -1 {
		t.Errorf("the placeholder indexes window %d; it stands for no window at all", quota.Index)
	}
}

// Index is a promise: it indexes Columns.Windows or Columns.Resets, and it is
// -1 wherever there is nothing to index. A kind that carried a stale 0 would
// read as "window 0" to anything that looked.
func TestOnlyAWindowOrAResetColumnIndexesAnything(t *testing.T) {
	c := ColumnsOf([]Row{listRow()})
	for _, col := range ListColumns(c) {
		switch col.Kind {
		case ColumnWindow:
			if col.Index < 0 || col.Index >= len(c.Windows) {
				t.Errorf("%s indexes window %d, out of %d", col.Kind, col.Index, len(c.Windows))
			}
		case ColumnReset:
			if col.Index < 0 || col.Index >= len(c.Resets) {
				t.Errorf("%s indexes reset %d, out of %d", col.Kind, col.Index, len(c.Resets))
			}
		default:
			if col.Index != -1 {
				t.Errorf("%s carries index %d, want -1", col.Kind, col.Index)
			}
		}
	}
}

// THE FOOTPRINTS ARE FROZEN. Every Content below is what internal/tui reserves
// today minus the two-column gap that follows a column, so a ladder computing
// max(header, Content) + 2 reserves exactly what it reserves now and no page
// moves when it starts reading these instead of its own constants.
//
// The numbers are restated here rather than imported because internal/tui
// imports this package and the reverse would be a cycle. What keeps the two
// copies honest is the same assertion made from the other side, in
// TestTheLadderReservesWhatTheSharedColumnsAskFor.
func TestEveryListColumnReservesWhatTheDashboardReservesToday(t *testing.T) {
	c := ColumnsOf([]Row{listRow()})
	want := map[string]int{
		"IDX": 6, "ACCOUNT": 22, "TYPE": 14, "TIER": 8,
		"5H": 6, "7D": 6, "5H IN": 7, "7D IN": 7,
		"STATE": 15, "AUTO": 6, "AGE": 8,
	}
	for _, col := range ListColumns(c) {
		got := maxOf(ansi.StringWidth(col.Header), col.Content) + 2
		if got != want[col.Header] {
			t.Errorf("%s (%s) reserves %d, want %d", col.Header, col.Kind, got, want[col.Header])
		}
	}
	// WORST is not in the set above -- CollapseWindows is what puts it on a
	// table -- and it carries the widest cell of any column here: the whole
	// block in one cell is a percentage plus a window header.
	worst := CollapseWindows(ListColumns(c))[4]
	if got := maxOf(ansi.StringWidth(worst.Header), worst.Content) + 2; got != HeaderBudget+7 {
		t.Errorf("WORST reserves %d, want %d", got, HeaderBudget+7)
	}
}

func maxOf(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---- the drop order ---------------------------------------------------------

func TestListDropsIsLowestPriorityFirst(t *testing.T) {
	got := kinds(ListDrops(ColumnsOf([]Row{listRow()})))
	want := []string{"TIER", "AUTO", "TYPE", "AGE", "STATE", "RESET", "RESET"}
	if !eq(got, want) {
		t.Fatalf("ListDrops = %v, want %v", got, want)
	}
}

// The soonest rollover sorts first and is the one a reader is most likely to be
// waiting on, so the reset columns go from the LAST plan-order one back.
func TestTheSoonestRolloverIsTheLastResetColumnToGo(t *testing.T) {
	c := ColumnsOf([]Row{listRow()})
	drops := ListDrops(c)
	last := drops[len(drops)-1]
	if last.Kind != ColumnReset || last.Index != 0 {
		t.Fatalf("the last column dropped is %s/%d, want the first reset column", last.Kind, last.Index)
	}
	if got, want := last.Header, c.Resets[0].Header; got != want {
		t.Errorf("its header is %q, want %q", got, want)
	}
}

// IDX and ACCOUNT say WHICH account and every window column says how much of
// one limit is gone. None of them may be offered to a caller looking for
// something to remove.
func TestListDropsNeverOffersAColumnTheTableMayNotLose(t *testing.T) {
	for _, c := range ListDrops(ColumnsOf([]Row{listRow()})) {
		switch c.Kind {
		case ColumnIdx, ColumnAccount, ColumnWindow, ColumnWorst:
			t.Errorf("%s is in the drop order; it is never dropped", c.Kind)
		}
	}
}

// Every entry in the drop order has to be findable in the column set by `==`,
// or a caller removing them removes nothing and the ladder silently stops
// working.
func TestEveryDropIsOneOfTheColumns(t *testing.T) {
	c := ColumnsOf([]Row{listRow()})
	cols := ListColumns(c)
	for _, d := range ListDrops(c) {
		found := false
		for _, col := range cols {
			if col == d {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s (%q) is dropped but is not one of the columns", d.Kind, d.Header)
		}
	}
}

// ---- collapsing the block ---------------------------------------------------

func TestCollapseWindowsSwapsTheBlockForOneWorstCell(t *testing.T) {
	c := ColumnsOf([]Row{listRow()})
	got := kinds(CollapseWindows(ListColumns(c)))
	want := []string{"IDX", "ACCOUNT", "TYPE", "TIER", "WORST", "RESET", "RESET", "STATE", "AUTO", "AGE"}
	if !eq(got, want) {
		t.Fatalf("CollapseWindows = %v, want %v — one cell, in the place the block held", got, want)
	}
}

// A fleet nobody could read has one placeholder quota column, and collapsing
// there would WIDEN the table a caller is collapsing because it is too narrow:
// the placeholder reserves seven columns and WORST reserves seventeen. Both
// cells say the same thing either way.
func TestCollapseWindowsDeclinesOnThePlaceholderQuotaColumn(t *testing.T) {
	cols := ListColumns(ColumnsOf([]Row{{Account: store.Account{UUID: "u-1"}}}))
	got := CollapseWindows(cols)
	if !eq(kinds(got), kinds(cols)) {
		t.Fatalf("CollapseWindows = %v, want the columns unchanged", kinds(got))
	}
	r := Row{Account: store.Account{UUID: "u-1"}}
	if cell := r.ListCell(got[4], Columns{}, colNow, false); cell != Unreadable {
		t.Errorf("the placeholder cell = %q, want %q either way", cell, Unreadable)
	}
}

// ---- one cell per kind ------------------------------------------------------

// The whole switch, answered once, for a row every column has something to say
// about. This is the assertion the two half-owned switches in internal/tui and
// internal/cli were each making for their own half.
func TestListCellAnswersEveryColumnKind(t *testing.T) {
	r := listRow()
	c := ColumnsOf([]Row{r})
	cols := CollapseWindows(ListColumns(c))
	want := map[string]string{
		"IDX": "* 3", "ACCOUNT": "work@example.com (work)", "TYPE": "subscription",
		"TIER": "claude_max", "WORST": "87% 5H", "5H IN": "2h00m", "7D IN": "1d16h",
		"STATE": "active", "AUTO": "yes", "AGE": "4m",
	}
	for _, col := range cols {
		if got := r.ListCell(col, c, colNow, false); got != want[col.Header] {
			t.Errorf("%s = %q, want %q", col.Header, got, want[col.Header])
		}
	}
	// And the two window cells the collapse above folded away.
	for _, col := range ListColumns(c) {
		if col.Kind != ColumnWindow {
			continue
		}
		want := map[string]string{"5H": "87%", "7D": "42%"}[col.Header]
		if got := r.ListCell(col, c, colNow, false); got != want {
			t.Errorf("%s = %q, want %q", col.Header, got, want)
		}
	}
}

// hover selects the quota cells' form and nothing else: the countdown beside
// them is the same span under either report.
func TestListCellUnderHoverShowsUsedAgainstTheThreshold(t *testing.T) {
	r := listRow()
	c := ColumnsOf([]Row{r})
	for _, col := range ListColumns(c) {
		switch col.Header {
		case "5H":
			if got, want := r.ListCell(col, c, colNow, true), "87%/80%"; got != want {
				t.Errorf("5H under hover = %q, want %q", got, want)
			}
		case "5H IN":
			if got, want := r.ListCell(col, c, colNow, true), "2h00m"; got != want {
				t.Errorf("5H IN under hover = %q, want %q — a countdown has no threshold", got, want)
			}
		}
	}
}

// The one cell a surface may not print as it arrives, because the switch cannot
// know the cursor: a page pointing at this row draws its own glyph over the
// marker, and where the two meet the live account wins.
func TestTheIdxCellCarriesTheLiveMarkerAndTheStoreIndex(t *testing.T) {
	r := listRow()
	col := ListColumns(ColumnsOf([]Row{r}))[0]
	if got := r.ListCell(col, Columns{}, colNow, false); got != "* 3" {
		t.Errorf("IDX on the live account = %q, want %q", got, "* 3")
	}
	r.Active = false
	if got := r.ListCell(col, Columns{}, colNow, false); got != "  3" {
		t.Errorf("IDX on an idle account = %q, want %q", got, "  3")
	}
}

// The ACCOUNT cell is the whole address-and-handle label, uncut: what it is cut
// to is a column width, and a column width comes off a terminal.
func TestTheAccountCellIsTheWholeLabelAndIsNotCut(t *testing.T) {
	r := listRow()
	r.Account.Email = "a-very-long-address-indeed@example.com"
	col := ListColumns(ColumnsOf([]Row{r}))[1]
	if got, want := r.ListCell(col, Columns{}, colNow, false), r.ListLabel(); got != want {
		t.Errorf("ACCOUNT = %q, want %q", got, want)
	}
}

// AGE is the age and only the age. `ccdad status` rides the flags on this cell
// as a suffix of its own, because they belong to the account rather than to the
// column.
func TestTheAgeCellCarriesNoStatusFlags(t *testing.T) {
	r := listRow()
	r.Account.Primary, r.Account.Disabled = true, true
	col := ListColumns(ColumnsOf([]Row{r}))[10]
	if col.Kind != ColumnAge {
		t.Fatalf("column 10 is %s, want AGE", col.Kind)
	}
	if got := r.ListCell(col, Columns{}, colNow, false); got != "4m" {
		t.Errorf("AGE = %q, want %q with the flags left to their own suffix", got, "4m")
	}
	if r.StatusFlags() == "" {
		t.Error("StatusFlags is empty, so the assertion above proves nothing")
	}
}

func TestAnUnknownColumnKindRendersNothing(t *testing.T) {
	if got := listRow().ListCell(ListColumn{Kind: ColumnKind(99)}, Columns{}, colNow, false); got != "" {
		t.Errorf("an unknown kind rendered %q, want the empty string", got)
	}
}

// ---- the collapsed cell -----------------------------------------------------

func TestWorstCellIsTheMaxAndNamesItsWindow(t *testing.T) {
	r := listRow()
	if got, want := r.WorstCell(ColumnsOf([]Row{r})), "87% 5H"; got != want {
		t.Errorf("WorstCell = %q, want %q", got, want)
	}
}

// One window could not be read, so the max is a lower bound. The "+" is what
// stops the cell claiming more than it knows.
func TestWorstCellFlagsAWindowItCouldNotRead(t *testing.T) {
	read := fleetRow("u-1", 30, 40, nil, 0)
	blind := Row{
		Account: store.Account{UUID: "u-2"}, HasEntry: true,
		Entry: usage.Entry{Snapshot: &usage.Snapshot{
			FiveHour: usage.NewWindow(nil, nil),
			SevenDay: window(55, colNow.Add(time.Hour)),
		}},
	}
	c := ColumnsOf([]Row{read, blind})
	if got, want := blind.WorstCell(c), "55% 7D+"; got != want {
		t.Errorf("WorstCell = %q, want %q", got, want)
	}
}

func TestWorstCellOnARowNobodyCouldReadIsUnreadable(t *testing.T) {
	r := listRow()
	blind := Row{Account: store.Account{UUID: "u-2"}}
	if got := blind.WorstCell(ColumnsOf([]Row{r})); got != Unreadable {
		t.Errorf("WorstCell = %q, want %q", got, Unreadable)
	}
}

// ---- the word columns -------------------------------------------------------

func TestStateLabelNamesEveryStateThisBinaryDeclares(t *testing.T) {
	for _, tc := range []struct {
		state daemon.AccountState
		want  string
	}{
		{daemon.StateActive, "active"},
		{daemon.StateCandidate, "candidate"},
		{daemon.StateExhausted, "exhausted"},
		{daemon.StateEmpty, "empty"},
		{daemon.StateQuarantined, "quarantined"},
		{daemon.StateServing, "serving"},
		{daemon.StateNeedsRelogin, "needs-relogin"},
		{daemon.StateDisabled, "disabled"},
		{daemon.StateUnknown, "unknown"},
	} {
		if got := StateLabel(tc.state); got != tc.want {
			t.Errorf("StateLabel(%q) = %q, want %q", tc.state, got, tc.want)
		}
	}
}

// The status document is additive by contract, so a newer daemon publishes a
// state this binary has never heard of on the day somebody upgrades one half of
// a machine. Carry it through rather than blanking the column.
func TestStateLabelCarriesAStateThisBuildHasNeverHeardOf(t *testing.T) {
	if got := StateLabel(daemon.AccountState("hibernating")); got != "hibernating" {
		t.Errorf("StateLabel = %q, want the value carried through", got)
	}
}

// An account no daemon has ever published carries "", which is no state at all
// rather than a state somebody tried to read and could not.
func TestAnAccountNoDaemonHasPublishedHasNoState(t *testing.T) {
	if got := StateLabel(""); got != NoQuantity {
		t.Errorf("StateLabel(\"\") = %q, want %q", got, NoQuantity)
	}
}

func TestAutoLabelIsARotationPolicyAndNotALock(t *testing.T) {
	r := listRow()
	if got := r.AutoLabel(); got != "yes" {
		t.Errorf("AutoLabel = %q, want yes", got)
	}
	r.Account.Disabled = true
	if got := r.AutoLabel(); got != "no" {
		t.Errorf("AutoLabel on a disabled account = %q, want no", got)
	}
}

// ---- the sections -----------------------------------------------------------

func claudeRow(uuid string) Row {
	return Row{Account: store.Account{UUID: uuid, Email: uuid + "@example.com", Provider: provider.Claude}}
}

func codexRow(uuid string) Row {
	return Row{Account: store.Account{UUID: uuid, Email: uuid + "@example.com", Provider: provider.Codex}}
}

func uuids(rows []ListRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Row.Account.UUID)
	}
	return out
}

// lines names each drawable line by what it carries, because a failure that
// printed the ListRow structs would print four screens of zero-valued account
// and no reader could find the heading in it.
func lines(rows []ListRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Header != "" {
			out = append(out, r.Header)
			continue
		}
		out = append(out, r.Row.Account.UUID)
	}
	return out
}

func TestSectionsGroupByProviderWithClaudeFirst(t *testing.T) {
	secs := Sections([]Row{codexRow("cx-1"), claudeRow("cl-1"), codexRow("cx-2"), claudeRow("cl-2")})
	if len(secs) != 2 {
		t.Fatalf("Sections returned %d sections, want 2", len(secs))
	}
	if secs[0].Provider != provider.Claude || secs[0].Header != ClaudeSection {
		t.Errorf("the first section is %q/%q, want Claude", secs[0].Provider, secs[0].Header)
	}
	if secs[1].Provider != provider.Codex || secs[1].Header != CodexSection {
		t.Errorf("the second section is %q/%q, want Codex", secs[1].Provider, secs[1].Header)
	}
	if got, want := uuids(secs[0].Rows), []string{"cl-1", "cl-2"}; !eq(got, want) {
		t.Errorf("Claude section = %v, want %v", got, want)
	}
	if got, want := uuids(secs[1].Rows), []string{"cx-1", "cx-2"}; !eq(got, want) {
		t.Errorf("Codex section = %v, want %v", got, want)
	}
}

// BOTH sections, always. A caller can then ask each for its rows without asking
// first whether it exists, and the total is the input by construction.
func TestSectionsReturnsAnEmptySectionRatherThanDroppingIt(t *testing.T) {
	secs := Sections([]Row{claudeRow("cl-1")})
	if len(secs) != 2 {
		t.Fatalf("Sections returned %d sections for a Claude-only fleet, want 2", len(secs))
	}
	if len(secs[1].Rows) != 0 {
		t.Errorf("the Codex section holds %d rows, want none", len(secs[1].Rows))
	}
	if secs[1].Header != CodexSection {
		t.Errorf("the empty section's header is %q, want %q", secs[1].Header, CodexSection)
	}
	if secs = Sections(nil); len(secs) != 2 {
		t.Errorf("Sections(nil) returned %d sections, want 2", len(secs))
	}
}

// A row read out of a version-1 document carries a zero Provider, which the
// store fills in as Claude when it loads. A grouping that read the zero value
// as Codex would file every one of those accounts under the wrong heading, and
// one that gave them a bucket of their own would leave them off the page. This
// is the sectioning half of TestTypeLabelOnAZeroProviderIsNotCodex.
func TestAZeroProviderIsGroupedUnderClaudeAndNeverUnderCodex(t *testing.T) {
	secs := Sections([]Row{{Account: store.Account{UUID: "cl-1"}}})
	if len(secs) != 2 {
		t.Fatalf("Sections returned %d sections, want both", len(secs))
	}
	if got, want := uuids(secs[0].Rows), []string{"cl-1"}; !eq(got, want) {
		t.Errorf("the Claude section = %v, want %v", got, want)
	}
	if len(secs[1].Rows) != 0 {
		t.Errorf("a row with no provider was filed under Codex: %v", uuids(secs[1].Rows))
	}
}

// Grouping changes which rows are adjacent. It may never change how many there
// are, and a third bucket is how one would go missing.
func TestSectionsLoseNoRow(t *testing.T) {
	rows := []Row{
		codexRow("cx-1"), claudeRow("cl-1"),
		{Account: store.Account{UUID: "zero"}},
		{Account: store.Account{UUID: "odd", Provider: provider.ID("gemini")}},
	}
	n := 0
	for _, s := range Sections(rows) {
		n += len(s.Rows)
	}
	if n != len(rows) {
		t.Fatalf("%d rows in, %d out — a section dropped one", len(rows), n)
	}
}

// ---- the drawable slice -----------------------------------------------------

func TestListRowsInterleaveEachHeadingWithTheAccountsBelowIt(t *testing.T) {
	rows := []Row{codexRow("cx-1"), claudeRow("cl-1"), claudeRow("cl-2")}
	got := ListRows(Sections(rows))
	want := []struct {
		header string
		uuid   string
		at     int
	}{
		{ClaudeSection, "", -1},
		{"", "cl-1", 1},
		{"", "cl-2", 2},
		{CodexSection, "", -1},
		{"", "cx-1", 0},
	}
	if len(got) != len(want) {
		t.Fatalf("ListRows = %d lines, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Header != w.header {
			t.Errorf("line %d header = %q, want %q", i, got[i].Header, w.header)
		}
		if got[i].Row.Account.UUID != w.uuid {
			t.Errorf("line %d account = %q, want %q", i, got[i].Row.Account.UUID, w.uuid)
		}
		if got[i].At != w.at {
			t.Errorf("line %d At = %d, want %d", i, got[i].At, w.at)
		}
	}
}

// At has to be carried rather than derived, and this is the case that proves
// it: cx-1 is the FIRST row in the store and the LAST line on the page, so a
// caller using the display position would point at one account and act on
// another.
func TestAnAccountRowRemembersWhereTheGroupingMovedItFrom(t *testing.T) {
	rows := []Row{codexRow("cx-1"), claudeRow("cl-1")}
	got := ListRows(Sections(rows))
	last := got[len(got)-1]
	if last.Row.Account.UUID != "cx-1" {
		t.Fatalf("the last line is %q, want cx-1", last.Row.Account.UUID)
	}
	if last.At != 0 {
		t.Fatalf("cx-1 was drawn last and reports At = %d, want 0 — its index in the store", last.At)
	}
	if rows[last.At].Account.UUID != "cx-1" {
		t.Error("At does not index the slice Sections was given")
	}
}

// A heading over NO rows still draws. A machine with four Claude accounts and
// no Codex one would otherwise render exactly as a build that has never heard
// of Codex, which is the one case the sections were added for.
func TestListRowsDrawTheHeadingOverASectionWithNoRows(t *testing.T) {
	got := ListRows(Sections([]Row{claudeRow("cl-1")}))
	want := []string{ClaudeSection, "", CodexSection}
	if len(got) != len(want) {
		t.Fatalf("ListRows = %v, want a heading, one account, and the empty section's heading", lines(got))
	}
	for i, w := range want {
		if got[i].Header != w {
			t.Errorf("line %d header = %q, want %q", i, got[i].Header, w)
		}
	}
}

// An empty STORE is still two providers. The sentence about having no accounts
// belongs to the store rather than to either section, so the surfaces print it
// once under both headings rather than twice, once under each.
func TestListRowsOnAnEmptyStoreAreTheTwoHeadingsAndNothingElse(t *testing.T) {
	got := ListRows(Sections(nil))
	if len(got) != 2 {
		t.Fatalf("ListRows on an empty fleet = %v, want the two headings", lines(got))
	}
	if got[0].Header != ClaudeSection || got[1].Header != CodexSection {
		t.Errorf("ListRows on an empty fleet = %v, want %q then %q", lines(got), ClaudeSection, CodexSection)
	}
}

// The heading is drawn as a table row carrying its text in the ACCOUNT cell,
// and the narrowest ACCOUNT is ever squeezed to is twelve columns. Six columns
// and five fit inside twelve at every width, so a heading can never be cut --
// and a cut heading would read as a provider name that is not one.
func TestASectionHeadingFitsInsideTheNarrowestAccountColumn(t *testing.T) {
	const accountFloor = 12
	for _, h := range []string{ClaudeSection, CodexSection} {
		if w := ansi.StringWidth(h); w > accountFloor {
			t.Errorf("%q is %d columns, more than the %d ACCOUNT is squeezed to", h, w, accountFloor)
		}
		for _, r := range h {
			if r > 127 {
				t.Errorf("%q is not 7-bit; a console that cannot draw it would cut the heading", h)
			}
		}
		if h != strings.ToUpper(h) {
			t.Errorf("%q is not all-caps", h)
		}
	}
	// Three tests grep case-sensitively for the `Codex:` that `ccdad status`
	// prints in front of the codex half of the Active line, to assert its
	// ABSENCE. Neither heading may make one of those greps answer for a reason
	// that has nothing to do with the line it is about.
	for _, h := range []string{ClaudeSection, CodexSection} {
		if strings.Contains(h, "Codex:") || strings.Contains("Codex:", h) {
			t.Errorf("%q shares a substring with %q", h, "Codex:")
		}
	}
}

// ---- under the table --------------------------------------------------------

// The order is a fact about the TABLE and not about the surface drawing it:
// the two lines that describe the quota block come before the ones that
// describe a row.
func TestTrailerLinesAreOneOrderedSlice(t *testing.T) {
	credit := creditOnlyRow(t, 60.2255)
	credit.Account.Alias = "money"
	rows := []Row{credit}
	// Built by hand rather than read off a fleet, because an unranked column is
	// a scoped cap under a scope this build does not name and no reading this
	// build can parse produces one. The note exists for the release that adds a
	// scope to the wire before this binary learns it.
	c := Columns{Windows: []WindowColumn{
		{Name: usage.WindowFiveHour, Header: "5H", Reset: -1, Ranked: true},
		{Name: usage.WindowName("weekly_scoped:project:Atlas"), Header: "ATLAS", Reset: -1},
	}}

	got := TrailerLines(rows, c, true, "", "")
	if len(got) != 4 {
		t.Fatalf("TrailerLines = %d lines, want 4:\n%s", len(got), strings.Join(got, "\n"))
	}
	if got[0] != c.Legend() {
		t.Errorf("line 0 = %q, want the legend", got[0])
	}
	if got[1] != HoverNote {
		t.Errorf("line 1 = %q, want the hover sentence", got[1])
	}
	if got[2] != c.UnrankedNote() {
		t.Errorf("line 2 = %q, want the unranked note", got[2])
	}
	if !strings.HasPrefix(got[3], "credit:   ") || !strings.Contains(got[3], credit.StatusLabel()) {
		t.Errorf("line 3 = %q, want the credit line naming its account", got[3])
	}
}

func TestTrailerLinesOmitEveryLineWithNothingToSay(t *testing.T) {
	rows := []Row{listRow()}
	got := TrailerLines(rows, ColumnsOf(rows), false, "", "")
	if len(got) != 1 {
		t.Fatalf("TrailerLines = %d lines, want the legend alone:\n%s", len(got), strings.Join(got, "\n"))
	}
	if !strings.HasPrefix(got[0], "windows:") {
		t.Errorf("the one line is %q, want the legend", got[0])
	}
	// A fleet nobody could read has no legend either, and then there is nothing
	// under the table at all.
	blind := []Row{{Account: store.Account{UUID: "u-1"}}}
	if n := len(TrailerLines(blind, ColumnsOf(blind), false, "", "")); n != 0 {
		t.Errorf("TrailerLines on an unreadable fleet = %d lines, want none", n)
	}
}

func TestOneCreditLinePerCreditMeteredSeat(t *testing.T) {
	credit := creditOnlyRow(t, 60.2255)
	rows := []Row{listRow(), credit, credit}
	n := 0
	for _, line := range TrailerLines(rows, ColumnsOf(rows), false, "", "") {
		if strings.HasPrefix(line, "credit:") {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d credit lines for two credit-metered seats, want 2", n)
	}
}

// ---- the summary block ------------------------------------------------------

// The PAIR rather than a joined string is what lets a surface paint the label
// without painting the value.
func TestSummaryLinesSplitEachLabelFromItsValue(t *testing.T) {
	s := Snapshot{
		ActiveLabel: "work@example.com (work)",
		Strategy:    "headroom",
		Mode:        strategy.ModeRecovery,
		HasMode:     true,
	}
	got := s.SummaryLines()
	want := []SummaryLine{
		{Label: "Active (Claude): ", Value: "work@example.com (work)"},
		{Label: "Strategy: ", Value: "headroom"},
		{Label: "Current:  ", Value: strings.TrimPrefix(CurrentLine(strategy.ModeRecovery), "Current:  ")},
	}
	if len(got) != len(want) {
		t.Fatalf("SummaryLines = %d lines, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// The two sentences have ONE author. Joining the pair back has to give exactly
// what the line functions produce, or the label text has two.
func TestSummaryLinesTakeTheirWordingFromTheLineFunctions(t *testing.T) {
	for _, name := range []string{"hover", "manual", "headroom"} {
		s := Snapshot{Strategy: name, Mode: strategy.ModeConsumeFirst, HasMode: true}
		lines := s.SummaryLines()
		strat := lines[len(lines)-2]
		if got, want := strat.Label+strat.Value, StrategyLine(name); got != want {
			t.Errorf("the Strategy line = %q, want %q", got, want)
		}
		cur := lines[len(lines)-1]
		if got, want := cur.Label+cur.Value, CurrentLine(strategy.ModeConsumeFirst); got != want {
			t.Errorf("the Current line = %q, want %q", got, want)
		}
	}
}

// Every fact owns one line, and the Codex fact exists only on a machine that is
// serving one -- so a machine with one provider renders what it rendered before
// there were two.
func TestSummaryLinesNameTheCodexSeatOnlyWhenOneIsServed(t *testing.T) {
	s := Snapshot{ActiveLabel: "work@example.com", Strategy: "headroom"}
	if got := s.SummaryLines(); len(got) != 2 {
		t.Fatalf("SummaryLines = %d lines with no codex seat, want 2: %+v", len(got), got)
	}
	s.CodexServingLabel = "cx@example.com"
	got := s.SummaryLines()
	if len(got) != 3 {
		t.Fatalf("SummaryLines = %d lines with a codex seat, want 3: %+v", len(got), got)
	}
	if got[1].Label != "Active (Codex): " || got[1].Value != "cx@example.com" {
		t.Errorf("the codex line = %+v", got[1])
	}
}

// The Current line is present only when the pass Decided. A zero Plan
// stringifies to plausible values, so a line built from a pass that never ran
// would print a real answer nobody computed.
func TestSummaryLinesDrawNoCurrentLineWhenNothingRanked(t *testing.T) {
	s := Snapshot{ActiveLabel: "work@example.com", Strategy: "headroom"}
	for _, line := range s.SummaryLines() {
		if strings.HasPrefix(line.Label, "Current") {
			t.Errorf("a Current line was drawn from a pass that never ran: %+v", line)
		}
	}
}

// Under hover the configured strategy has stopped being read, and naming it
// here made a page under a fully automatic mode look exactly like one that was
// not.
func TestTheStrategyLineNamesThePolicyInForce(t *testing.T) {
	s := Snapshot{Strategy: "headroom", Hover: true}
	lines := s.SummaryLines()
	strat := lines[len(lines)-1]
	if got, want := strat.Label+strat.Value, StrategyLine("hover"); got != want {
		t.Errorf("the Strategy line = %q, want %q", got, want)
	}
}

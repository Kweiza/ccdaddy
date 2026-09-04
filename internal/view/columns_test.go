package view

import (
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

var colNow = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

// fleetRow is one account's reading. skewUs is how far the scoped cap's reset
// lands from seven_day's, in microseconds — the live fleet's shape.
func fleetRow(uuid string, five, seven float64, scoped map[string]float64, skewUs int) Row {
	s := &usage.Snapshot{
		FiveHour: window(five, colNow.Add(2*time.Hour)),
		SevenDay: window(seven, colNow.Add(40*time.Hour)),
	}
	for name, p := range scoped {
		at := colNow.Add(40*time.Hour + time.Duration(skewUs)*time.Microsecond)
		pv := p
		s.Limits = append(s.Limits, usage.LimitFor(usage.LimitInput{
			Kind: "weekly_scoped", Model: name, Percent: &pv, ResetsAt: &at,
		}))
	}
	return Row{
		Account:  store.Account{UUID: uuid},
		HasEntry: true,
		Entry:    usage.Entry{Snapshot: s},
	}
}

func headers(c Columns) []string {
	out := make([]string, 0, len(c.Windows))
	for _, w := range c.Windows {
		out = append(out, w.Header)
	}
	return out
}

// ---- membership -------------------------------------------------------------

// The union over visible rows of what each row CARRIES. The Present filter is
// load-bearing: the snapshot lists the fixed Claude keys unconditionally, so an
// unfiltered walk gives every Claude fleet five columns with three of them
// null on every row.
func TestColumnsAreTheWindowsTheRowsActuallyCarry(t *testing.T) {
	c := ColumnsOf([]Row{fleetRow("a", 22, 84, map[string]float64{"Fable": 100}, 158)})

	if got, want := headers(c), []string{"5H", "7D", "FABLE"}; !eq(got, want) {
		t.Errorf("headers = %v, want %v", got, want)
	}
}

func TestAWindowOnlyOneRowCarriesStillGetsAColumn(t *testing.T) {
	rows := []Row{
		fleetRow("a", 10, 20, nil, 0),
		fleetRow("b", 10, 20, map[string]float64{"Fable": 50}, 0),
	}
	if got, want := headers(ColumnsOf(rows)), []string{"5H", "7D", "FABLE"}; !eq(got, want) {
		t.Errorf("headers = %v, want %v — the union, not the intersection", got, want)
	}
}

func TestAnUnreadRowContributesNoColumns(t *testing.T) {
	c := ColumnsOf([]Row{{Account: store.Account{UUID: "a"}}})
	if len(c.Windows) != 0 {
		t.Errorf("headers = %v, want none — nothing was read", headers(c))
	}
}

// ---- order ------------------------------------------------------------------

// Rows arrive in STORE order, which moves when an account is added or hidden.
// A first-seen rule would slide a column sideways between two runs of the same
// command; sorted, the header row is a function of which windows exist.
func TestTheHeaderRowDoesNotDependOnRowOrder(t *testing.T) {
	a := fleetRow("a", 10, 20, map[string]float64{"Zeta": 10}, 0)
	b := fleetRow("b", 10, 20, map[string]float64{"Alpha": 10}, 0)

	one := headers(ColumnsOf([]Row{a, b}))
	two := headers(ColumnsOf([]Row{b, a}))
	if !eq(one, two) {
		t.Errorf("order-dependent headers: %v vs %v", one, two)
	}
	if want := []string{"5H", "7D", "ALPHA", "ZETA"}; !eq(one, want) {
		t.Errorf("headers = %v, want %v", one, want)
	}
}

// ---- headers ----------------------------------------------------------------

func TestAHeaderIsCutToTheBudgetWithACueThatIsOneColumnEverywhere(t *testing.T) {
	long := strings.Repeat("A", HeaderBudget+5)
	got := WindowHeader(usage.ScopedWindowName(usage.ScopeModel, long))
	if len([]rune(got)) > HeaderBudget {
		t.Errorf("header %q is %d runes, past the budget of %d", got, len([]rune(got)), HeaderBudget)
	}
	if !strings.HasSuffix(got, "+") {
		t.Errorf("header %q does not carry the cut cue", got)
	}
}

// Two windows must never share a header, whatever their names.
func TestAModelCapAndASurfaceCapSharingADisplayNameStayTwoColumns(t *testing.T) {
	at := colNow.Add(40 * time.Hour)
	p := 10.0
	s := &usage.Snapshot{
		FiveHour: window(10, colNow.Add(time.Hour)),
		Limits: []usage.Limit{
			usage.LimitFor(usage.LimitInput{Kind: "weekly_scoped", Model: "Cowork", Percent: &p, ResetsAt: &at}),
			usage.LimitFor(usage.LimitInput{Kind: "weekly_scoped", Surface: "Cowork", Percent: &p, ResetsAt: &at}),
		},
	}
	c := ColumnsOf([]Row{{Account: store.Account{UUID: "a"}, HasEntry: true, Entry: usage.Entry{Snapshot: s}}})

	seen := map[string]bool{}
	for _, w := range c.Windows {
		if seen[w.Header] {
			t.Fatalf("two columns share the header %q: %v", w.Header, headers(c))
		}
		seen[w.Header] = true
	}
	if len(c.Windows) != 3 {
		t.Errorf("headers = %v, want three columns", headers(c))
	}
}

// ---- reset grouping ---------------------------------------------------------

// The measured shape: the scoped cap and seven_day are one server-side instant
// arriving microseconds apart. Exact equality would draw three countdowns where
// the fleet has two.
func TestTwoWindowsMicrosecondsApartAreOneCountdown(t *testing.T) {
	c := ColumnsOf([]Row{fleetRow("a", 22, 84, map[string]float64{"Fable": 100}, 158)})

	if len(c.Resets) != 2 {
		t.Fatalf("reset columns = %d (%v), want 2", len(c.Resets), resetHeaders(c))
	}
	if got, want := resetHeaders(c), []string{"5H IN", "7D IN"}; !eq(got, want) {
		t.Errorf("reset headers = %v, want %v", got, want)
	}
	for _, w := range c.Windows {
		if w.Header == "FABLE" && c.Resets[w.Reset].Header != "7D IN" {
			t.Errorf("FABLE points at %q, want 7D IN", c.Resets[w.Reset].Header)
		}
	}
}

// A tolerance and not a truncation: past the tolerance the two are two
// rollovers and get two columns, with no cliff at any particular boundary.
func TestRollingOverMoreThanAToleranceApartStaysTwoCountdowns(t *testing.T) {
	c := ColumnsOf([]Row{fleetRow("a", 22, 84, map[string]float64{"Fable": 100}, 2_000_000)})
	if len(c.Resets) != 3 {
		t.Errorf("reset columns = %v, want 3 — two seconds apart is two rollovers", resetHeaders(c))
	}
}

// The witness. On a fleet nobody could read, vacuous agreement would collapse
// every window into one countdown belonging to none of them.
func TestWindowsNoRowEverComparedDoNotMerge(t *testing.T) {
	at := colNow.Add(40 * time.Hour)
	p := 10.0
	only5h := &usage.Snapshot{FiveHour: window(10, at)}
	onlyFable := &usage.Snapshot{Limits: []usage.Limit{
		usage.LimitFor(usage.LimitInput{Kind: "weekly_scoped", Model: "Fable", Percent: &p, ResetsAt: &at}),
	}}
	c := ColumnsOf([]Row{
		{Account: store.Account{UUID: "a"}, HasEntry: true, Entry: usage.Entry{Snapshot: only5h}},
		{Account: store.Account{UUID: "b"}, HasEntry: true, Entry: usage.Entry{Snapshot: onlyFable}},
	})
	if len(c.Resets) != 2 {
		t.Errorf("reset columns = %v, want 2 — no row carried both, so nothing witnessed the agreement", resetHeaders(c))
	}
}

// ---- cells ------------------------------------------------------------------

// Three absences, three renderings, and the one that must never be guessed.
func TestACellTellsUnreadFromNotCarriedFromRead(t *testing.T) {
	r := fleetRow("a", 22, 84, map[string]float64{"Fable": 100}, 0)

	if got := r.WindowCell(usage.WindowFiveHour); got != "22%" {
		t.Errorf("five_hour cell = %q, want 22%%", got)
	}
	if got := r.WindowCell(usage.WindowSevenDayOpus); got != "-" {
		t.Errorf("a window the account does not carry = %q, want -", got)
	}
	unread := Row{Account: store.Account{UUID: "b"}}
	if got := unread.WindowCell(usage.WindowFiveHour); got != Unreadable {
		t.Errorf("a row nobody could read = %q, want %q — never - and never 0%%", got, Unreadable)
	}
}

func TestAWindowCarriedWithNoUtilizationReadsUnknownAndNotZero(t *testing.T) {
	s := &usage.Snapshot{FiveHour: usage.NewWindow(nil, nil)}
	r := Row{Account: store.Account{UUID: "a"}, HasEntry: true, Entry: usage.Entry{Snapshot: s}}
	if got := r.WindowCell(usage.WindowFiveHour); got != Unreadable {
		t.Errorf("present-with-null = %q, want %q", got, Unreadable)
	}
}

// Empty is checked before the band. Under hover a threshold is an unclamped
// pace target, so a spent window reports POSITIVE slack and a band consulted
// first paints a gone week the colour of a healthy one.
func TestASpentWindowIsOverEvenWhenItsSlackIsPositive(t *testing.T) {
	r := fleetRow("a", 0, 80, map[string]float64{"Fable": 100}, 0)
	r.Thresholds = strategy.Thresholds{Default: 80, PerWindow: map[usage.WindowName]float64{
		usage.ScopedWindowName(usage.ScopeModel, "Fable"): 117,
	}}
	n := usage.ScopedWindowName(usage.ScopeModel, "Fable")
	if slack := r.Thresholds.For(n) - 100; slack <= WarnBand {
		t.Fatalf("fixture slack = %v; it must be past the band for this test to mean anything", slack)
	}
	if got := r.CellState(n); got != CellOver {
		t.Errorf("CellState = %v, want CellOver", got)
	}
}

func resetHeaders(c Columns) []string {
	out := make([]string, 0, len(c.Resets))
	for _, r := range c.Resets {
		out = append(out, r.Header)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The other half of the absence rule, and the half a fix for the first one can
// break: ZERO IS A READING. An account at 0% used must render as a figure, or
// "never say 0%" satisfies both halves and the table stops distinguishing them.
//
// This pair is cswap's parked engine in its newest home. There, one expired
// token made every account look empty and the engine settled on whichever reset
// last; here, a cell that read "-" or "0%" for an account nobody could reach
// would say the same thing to a reader.
func TestZeroPercentIsAReadingAndRendersAsOne(t *testing.T) {
	r := fleetRow("a", 0, 40, nil, 0)
	if got := r.WindowCell(usage.WindowFiveHour); got != "0%" {
		t.Errorf("a window at 0%% used = %q, want 0%%", got)
	}
	unread := Row{}
	if got := unread.WindowCell(usage.WindowFiveHour); got == "0%" {
		t.Errorf("an unread row = %q; zero is a reading and this is not one", got)
	}
}

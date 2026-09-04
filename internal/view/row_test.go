package view

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

func ptr(f float64) *float64 { return &f }

func window(pct float64, reset time.Time) usage.Window {
	return usage.NewWindow(&pct, &reset)
}

// rowWithWindow builds a Row whose Reported() resolves to name, carrying
// whatever pct and reset usage.NewWindow was given -- either may be nil, for
// "not reported".
func rowWithWindow(t *testing.T, name usage.WindowName, pct *float64, reset *time.Time) Row {
	t.Helper()
	if name != usage.WindowFiveHour {
		t.Fatalf("rowWithWindow only knows five_hour; got %q", name)
	}
	return Row{
		HasEntry: true,
		Entry:    usage.Entry{Snapshot: &usage.Snapshot{FiveHour: usage.NewWindow(pct, reset)}},
		Headroom: strategy.Headroom{Known: true, Binding: name},
	}
}

// The old loop set `used` and `windowName` from one Reported() call, in two
// branches: windowName was assigned whenever Reported() succeeded, and `used`
// only when Percent() also did. A present window that reported no utilization
// is therefore a real row with a name and no number, and splitting the two
// cells into two methods is only correct if each one re-derives that answer.
func TestAPresentWindowWithNoNumberKeepsItsNameAndLosesOnlyItsPercentage(t *testing.T) {
	r := rowWithWindow(t, usage.WindowFiveHour, nil, nil)
	if got := r.WindowCell(usage.WindowFiveHour); got != Unreadable {
		t.Errorf("UsedLabel() = %q, want %q: a window that reported no utilization is not one at 0%%", got, Unreadable)
	}
	if got := string(r.ReportedName()); got != string(usage.WindowFiveHour) {
		t.Errorf("WindowLabel() = %q, want %q: the window is present and has a name", got, usage.WindowFiveHour)
	}
}

// The other side of the same split, and the one that fails for a renderer that
// never draws a number.
func TestAWindowAtZeroPercentIsARealReadingAndNotAnAbsence(t *testing.T) {
	r := rowWithWindow(t, usage.WindowFiveHour, ptr(0.0), nil)
	if got := r.WindowCell(usage.WindowFiveHour); got != "0%" {
		t.Errorf("UsedLabel() = %q, want \"0%%\": 0 is a reading", got)
	}
}

// ThresholdsFor's notices are the strings package cli used to print. A dropped
// trailing newline is invisible in a unit test that trims and is a run-together
// dashboard in the shell, so it is asserted here rather than trimmed away.
func TestAThresholdNoticeCarriesItsOwnTrailingNewline(t *testing.T) {
	cfg := config.Config{Hover: true}
	_, notices := ThresholdsFor(cfg, time.Now(), strategy.Plan{}, errors.New("the account store could not be read"))
	if len(notices) != 1 {
		t.Fatalf("notices = %v, want exactly one", notices)
	}
	if !strings.HasSuffix(notices[0], "\n") {
		t.Errorf("notice = %q, want it to carry its own trailing newline", notices[0])
	}
}

// A regression check that KindCredit's fallback in LeftLabel is actually
// exercised here too, not only compiled: a row whose headroom is never Known
// still renders a real remaining-credit figure rather than Unreadable.
func TestACreditAccountsLeftLabelReadsTheCreditAxisRatherThanUnreadable(t *testing.T) {
	limit, used := 10000.0, 2550.0 // cents: a $100 cap, $25.50 spent
	r := Row{
		HasEntry: true,
		Entry: usage.Entry{Snapshot: &usage.Snapshot{
			ExtraUsage: usage.ExtraUsageFor(usage.ExtraUsageInput{
				State: usage.ExtraUsageEnabled, Currency: "USD",
				MonthlyLimit: &limit, UsedCredits: &used,
			}),
		}},
		// Headroom is never Known for a KindCredit seat: it is computed from
		// the five subscription windows alone, and a credit account carries
		// none of them.
		Headroom: strategy.Headroom{Known: false},
	}
	if got := func() string { l, _ := r.CreditLine(); return l }(); got == Unreadable {
		t.Errorf("LeftLabel() = %q, want the credit axis rather than Unreadable", got)
	}
	if got := func() string { l, _ := r.CreditLine(); return l }(); !strings.Contains(got, "74.50") {
		t.Errorf("LeftLabel() = %q, want the remaining amount", got)
	}
}

// creditOnlyRow is an account metered in money and nothing else, built through
// the same constructor status and the TUI use so the wiring is under test and
// not only the formatting. The numbers are a live claude_enterprise seat read
// on 2026-08-26, in the minor units the wire carries.
func creditOnlyRow(t *testing.T, utilization float64) Row {
	t.Helper()
	u, limit, used := utilization, 200000.0, 120451.0
	snap := &usage.Snapshot{ExtraUsage: usage.ExtraUsageFor(usage.ExtraUsageInput{
		State:        usage.ExtraUsageEnabled,
		Currency:     "USD",
		MonthlyLimit: &limit,
		UsedCredits:  &used,
		Utilization:  &u,
	})}
	cache := &usage.Cache{}
	cache.Put("u-1", usage.Entry{Snapshot: snap, FetchedAt: time.Now()})
	acct := store.Account{UUID: "u-1", Kind: identity.KindCredit, Primary: true}
	rows := Rows([]store.Account{acct}, cache, acct, true, time.Now(),
		func(string) strategy.Thresholds { return strategy.Thresholds{} })
	if len(rows) != 1 {
		t.Fatalf("Rows() returned %d rows, want 1", len(rows))
	}
	return rows[0]
}

// TestASeatMeteredOnlyInMoneyStillReportsHowMuchItHasUsed is the display half
// of the enterprise seat.
//
// Rows() derived every row's headroom from the window-only axis, so a seat with
// no plan window rendered "?" in status and the TUI — while the engine, which
// reassigns exactly this seat onto the credit
// axis, was ranking it on a real percentage. The dashboard has to describe the
// meter the account actually runs on.
func TestASeatMeteredOnlyInMoneyStillReportsHowMuchItHasUsed(t *testing.T) {
	r := creditOnlyRow(t, 60.2255)
	if !r.Headroom.Known {
		t.Fatal("Headroom.Known = false — Rows() is still reading the window-only axis")
	}
	if got := r.WindowCell(strategy.CreditWindow); got != "60%" {
		t.Errorf("the credit cell = %q, want %q — the seat has spent 60%% of its balance", got, "60%")
	}
	// And it has no five-hour window at all, which is a different absence from
	// one it holds and could not read.
	if got := r.WindowCell(usage.WindowFiveHour); got != "-" {
		t.Errorf("five_hour = %q, want - — this seat carries no plan window", got)
	}
}

// TestASeatMeteredOnlyInMoneyNamesNoWindow is the guard on the test above. The
// credit axis is not a window: it has no reset and no rollover, and naming one
// would send the WINDOW and RESETS IN columns looking for a recovery that never
// comes.
func TestASeatMeteredOnlyInMoneyNamesNoWindow(t *testing.T) {
	r := creditOnlyRow(t, 60.2255)
	// The credit axis is not a window: it has no reset and no rollover, so it
	// resolves to nothing in the window set and gets no countdown column.
	if _, ok := r.Reported(); ok {
		t.Errorf("Reported() resolved %q — extra_usage is a balance, not a window", r.ReportedName())
	}
	if _, ok := r.WindowReset(strategy.CreditWindow); ok {
		t.Error("the credit meter reported a rollover; a balance does not roll over")
	}
}

// TestASeatMeteredOnlyInMoneyKeepsTheMoneyInItsLeftColumn is the half of the
// display that a percentage cannot carry.
//
// LEFT reads the headroom percentage for an account metered on a plan window,
// because that is the quantity the ranking used and a window has no other. A
// BALANCE does: "40%" and "795.23 left of 2000.00 (USD)" are the same fact, and
// only one of them tells a user whether to top up. The credit fallback here was
// written for exactly this class of account and became unreachable the moment
// the headroom axis learned to read them, which is the kind of regression that
// leaves both columns technically correct and the page less useful.
func TestASeatMeteredOnlyInMoneyKeepsTheMoneyInItsLeftColumn(t *testing.T) {
	r := creditOnlyRow(t, 60.2255)
	got := func() string { l, _ := r.CreditLine(); return l }()
	if !strings.Contains(got, "USD") {
		t.Errorf("LeftLabel() = %q, want the balance and its currency — a percentage of a balance hides the balance", got)
	}
	if !strings.Contains(got, "2000.00") {
		t.Errorf("LeftLabel() = %q, want the account's own cap in it", got)
	}
	// The cell keeps the percentage, which is what makes it comparable with
	// every other account's cells across a mixed fleet.
	if used := r.WindowCell(strategy.CreditWindow); used != "60%" {
		t.Errorf("UsedLabel() = %q, want %q", used, "60%")
	}
}

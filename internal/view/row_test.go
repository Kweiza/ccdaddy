package view

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/config"
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
	if got := r.UsedLabel(); got != Unreadable {
		t.Errorf("UsedLabel() = %q, want %q: a window that reported no utilization is not one at 0%%", got, Unreadable)
	}
	if got := r.WindowLabel(); got != string(usage.WindowFiveHour) {
		t.Errorf("WindowLabel() = %q, want %q: the window is present and has a name", got, usage.WindowFiveHour)
	}
}

// The other side of the same split, and the one that fails for a renderer that
// never draws a number.
func TestAWindowAtZeroPercentIsARealReadingAndNotAnAbsence(t *testing.T) {
	r := rowWithWindow(t, usage.WindowFiveHour, ptr(0.0), nil)
	if got := r.UsedLabel(); got != "0%" {
		t.Errorf("UsedLabel() = %q, want \"0%%\": 0 is a reading", got)
	}
}

// The three absences, each of which reaches Unreadable by a different route.
func TestEveryWayAReadingCanBeMissingRendersTheSameQuestionMark(t *testing.T) {
	cases := []struct {
		name string
		row  Row
	}{
		{
			name: "no cache entry at all",
			row:  Row{HasEntry: false},
		},
		{
			name: "headroom never resolved to Known",
			row: Row{
				HasEntry: true,
				Entry:    usage.Entry{Snapshot: &usage.Snapshot{FiveHour: window(20, time.Time{})}},
				Headroom: strategy.Headroom{Known: false},
			},
		},
		{
			name: "a binding name AllWindows does not carry",
			row: Row{
				HasEntry: true,
				Entry:    usage.Entry{Snapshot: &usage.Snapshot{}},
				Headroom: strategy.Headroom{Known: true, Binding: usage.WindowName("weekly_scoped:model:retired")},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.row.UsedLabel(); got != Unreadable {
				t.Errorf("UsedLabel() = %q, want %q", got, Unreadable)
			}
			if got := tc.row.WindowLabel(); got != "-" {
				t.Errorf("WindowLabel() = %q, want \"-\"", got)
			}
		})
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
	if got := r.LeftLabel(); got == Unreadable {
		t.Errorf("LeftLabel() = %q, want the credit axis rather than Unreadable", got)
	}
	if got := r.LeftLabel(); !strings.Contains(got, "74.50") {
		t.Errorf("LeftLabel() = %q, want the remaining amount", got)
	}
}

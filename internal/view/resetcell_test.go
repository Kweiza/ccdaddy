package view

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

func resetCellRow(s *usage.Snapshot) Row {
	return Row{Account: store.Account{UUID: "a"}, HasEntry: true, Entry: usage.Entry{Snapshot: s}}
}

// The reset cell draws the line the quota cell beside it draws. A week-only seat has
// no five-hour window, so its countdown does not exist, and "?" would send a reader
// looking for a reading that was never going to arrive.
func TestAResetCellTellsAnAbsentWindowFromAnUnreadOne(t *testing.T) {
	col := ResetColumn{Windows: []usage.WindowName{usage.WindowFiveHour}}
	pct := 0.0
	for _, tc := range []struct {
		name string
		row  Row
		want string
	}{
		{"no such window", resetCellRow(&usage.Snapshot{SevenDay: window(60, colNow.Add(time.Hour))}), NoQuantity},
		{"carried with no reset", resetCellRow(&usage.Snapshot{FiveHour: usage.NewWindow(&pct, nil)}), Unreadable},
		{"carried and unreadable", resetCellRow(&usage.Snapshot{FiveHour: usage.NewWindow(nil, nil)}), Unreadable},
		{"never read", Row{Account: store.Account{UUID: "a"}}, Unreadable},
		{"a reading with no snapshot", resetCellRow(nil), Unreadable},
		{"carried with a reset", resetCellRow(&usage.Snapshot{FiveHour: window(1, colNow.Add(time.Hour))}), "1h00m"},
	} {
		if got := tc.row.ResetCell(col, colNow); got != tc.want {
			t.Errorf("%s: ResetCell = %q, want %q", tc.name, got, tc.want)
		}
	}
}

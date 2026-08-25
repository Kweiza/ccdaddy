package view

import (
	"math"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// close3 compares to three decimals, because every figure in this file is
// 100/6 arithmetic: an exact literal would be a transcription of float64's
// rounding rather than of the worked example the comment states.
func close3(got, want float64) bool { return math.Abs(got-want) < 0.001 }

// hoverTable is one account's derived pace targets, written out as a per-window
// table. Hover's derivation lives in package strategy and is not exported, and
// reaching into it here would make these cases turn on a pool size rather than
// on the two numbers they are actually about.
func hoverTable(fiveHour, sevenDay float64) strategy.Thresholds {
	return strategy.Thresholds{PerWindow: map[usage.WindowName]float64{
		usage.WindowFiveHour: fiveHour,
		usage.WindowSevenDay: sevenDay,
	}}
}

// twoWindowRow is a Row whose Headroom is COMPUTED rather than written out.
// Which window binds, and whether there is a floor at all, are the things under
// test here, so a literal Headroom would let a case assert the shape it was
// handed.
func twoWindowRow(fivePct, sevenPct float64, thr strategy.Thresholds) Row {
	at := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	s := &usage.Snapshot{FiveHour: window(fivePct, at), SevenDay: window(sevenPct, at)}
	return Row{HasEntry: true, Entry: usage.Entry{Snapshot: s}, Headroom: strategy.HeadroomOf(s, thr)}
}

// The blown weekly. Six usable accounts make the share 100/6, so five_hour at
// 85% elapsed is measured against 101.667 and seven_day at 92% elapsed against
// 108.667. The five-hour window binds on 3.667 of slack; the weekly, with
// NOTHING left in it, reports 8.667 and is the window the row reports.
//
// Pairing the reported name with Headroom.Slack is therefore a bar drawn at
// 100% and described as three points from its threshold.
func TestReportedSlackDescribesTheWindowTheRowActuallyReports(t *testing.T) {
	r := twoWindowRow(98, 100, hoverTable(101.667, 108.667))

	if got := r.ReportedName(); got != usage.WindowSevenDay {
		t.Fatalf("ReportedName() = %q, want seven_day: a blown weekly is the floor", got)
	}
	slack, thr, ok := r.ReportedSlack()
	if !ok {
		t.Fatal("ReportedSlack() ok = false, want true: the window is right there in the snapshot")
	}
	if !close3(slack, 8.667) || !close3(thr, 108.667) {
		t.Errorf("ReportedSlack() = (%v, %v), want (8.667, 108.667)", slack, thr)
	}
	if close3(slack, r.Headroom.Slack) {
		t.Errorf("ReportedSlack() returned the BINDING slack %v; that is five_hour's number on a seven_day row",
			r.Headroom.Slack)
	}
}

// The blown five-hour window, which is the case no test on the reported window
// can see. five_hour is 100% used at 95% elapsed, so its pace target is 111.667
// and it reports POSITIVE slack; seven_day is 40% used at 30% elapsed against
// 46.667 and binds on the smaller 6.667. A blown five-hour window can never be
// a floor, so the row reports the weekly, the bar reads 40%, and the account
// cannot serve one prompt.
func TestEmptyAsksTheAccountAndNotTheReportedWindow(t *testing.T) {
	r := twoWindowRow(100, 40, hoverTable(111.667, 46.667))

	if r.Headroom.Binding != usage.WindowSevenDay || r.Headroom.HasFloor {
		t.Fatalf("Binding = %q, HasFloor = %v; want seven_day and no floor -- five_hour is not weekly and cannot be one",
			r.Headroom.Binding, r.Headroom.HasFloor)
	}
	slack, _, ok := r.ReportedSlack()
	if !ok || slack <= 0 {
		t.Fatalf("ReportedSlack() = (%v, ok %v); this case is pointless unless the reported window looks healthy",
			slack, ok)
	}
	empty, known := r.Empty()
	if !known || !empty {
		t.Errorf("Empty() = (%v, %v), want (true, true): five_hour has nothing left in it", empty, known)
	}
}

// Both answers refuse rather than guess, and they refuse on their own terms.
// ReportedSlack mirrors Reported(): no window to draw means no number to draw
// beside it. Empty mirrors Headroom.Known, which is a different question -- an
// account nobody could read is not an empty one, and that confusion is what
// parked cswap's engine on the account that reset last.
func TestNeitherAnswerIsInventedForARowWithNoReading(t *testing.T) {
	var r Row
	if _, _, ok := r.ReportedSlack(); ok {
		t.Error("ReportedSlack() ok = true on a row with no entry")
	}
	if empty, known := r.Empty(); known || empty {
		t.Errorf("Empty() = (%v, %v), want (false, false): unread is not empty", empty, known)
	}
}

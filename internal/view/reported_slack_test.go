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
	// 0 and not 8.667: the weekly has nothing left, and an empty window never
	// reports positive slack however far past 100 its pace target ran. The
	// THRESHOLD is still the pace target it actually was, so the pair does not
	// subtract -- which is the same thing `ccdad hover status` prints a footer
	// about.
	if !close3(slack, 0) || !close3(thr, 108.667) {
		t.Errorf("ReportedSlack() = (%v, %v), want (0, 108.667)", slack, thr)
	}
}

// The two pairs genuinely apart, on a shape that does not need an empty window
// to look roomy. five_hour is 95% used against a 75 target -- twenty points
// past, and the tightest thing here -- while the weekly is 85% used against 80,
// five points past, over its line and therefore the floor. Binding is five_hour
// and Reported is seven_day, so a reader resolving the window through one and
// the number through the other reads a five-hour figure on a weekly row.
func TestReportedSlackAndTheBindingSlackAreDifferentNumbers(t *testing.T) {
	r := twoWindowRow(95, 85, hoverTable(75, 80))

	if r.Headroom.Binding != usage.WindowFiveHour {
		t.Fatalf("Binding = %q, want five_hour", r.Headroom.Binding)
	}
	if r.ReportedName() != usage.WindowSevenDay {
		t.Fatalf("ReportedName() = %q, want seven_day", r.ReportedName())
	}
	slack, thr, ok := r.ReportedSlack()
	if !ok || !close3(slack, -5) || !close3(thr, 80) {
		t.Errorf("ReportedSlack() = (%v, %v, ok %v), want (-5, 80, true)", slack, thr, ok)
	}
	if close3(slack, r.Headroom.Slack) {
		t.Errorf("ReportedSlack() returned the BINDING slack %v; that is five_hour's number on a seven_day row",
			r.Headroom.Slack)
	}
}

// The blown five-hour window. It used to be the case no column could see: at
// 100% used and 95% elapsed its pace target was 111.667, so it reported
// POSITIVE slack, seven_day at 40% bound on the smaller 6.667, and the row
// reported a weekly with room while the account could not serve one prompt.
// That is the shape a live fleet hit -- a session cut off at the five-hour
// limit with three accounts holding five-hour room behind it.
//
// The clamp closes it at the source: an empty window never reports positive
// slack, so five_hour reports 0 and is the tightest window on the account. It
// BINDS, which means the ranking sees it, and Empty() answers on the account
// as it always did.
func TestABlownFiveHourWindowBindsInsteadOfHidingBehindAWeekly(t *testing.T) {
	r := twoWindowRow(100, 40, hoverTable(111.667, 46.667))

	if r.Headroom.Binding != usage.WindowFiveHour {
		t.Fatalf("Binding = %q, want five_hour: it has nothing left and nothing is tighter than that",
			r.Headroom.Binding)
	}
	if !close3(r.Headroom.Slack, 0) {
		t.Errorf("Slack = %v, want 0: a window with nothing left never reports positive slack", r.Headroom.Slack)
	}
	// A five-hour window can never be a floor -- the floor rule wants a weekly --
	// so the account is reported against nothing here, and the ranking is the
	// only thing that can act on it.
	if r.Headroom.HasFloor {
		t.Errorf("HasFloor = true; five_hour is not weekly and cannot be one")
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

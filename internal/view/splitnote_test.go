package view

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

var splitNow = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

// splitRow builds a Row from a COMPUTED Headroom, so a case cannot assert the
// shape it was handed -- the convention reported_slack_test.go already sets.
func splitRow(fiveHour, sevenDay float64, fable *float64, per map[usage.WindowName]float64) Row {
	s := &usage.Snapshot{
		FiveHour: window(fiveHour, splitNow.Add(3*time.Hour)),
		SevenDay: window(sevenDay, splitNow.Add(40*time.Hour)),
	}
	if fable != nil {
		at := splitNow.Add(40 * time.Hour)
		s.Limits = []usage.Limit{usage.LimitFor(usage.LimitInput{
			Kind: "weekly_scoped", Model: "Fable", Percent: fable, ResetsAt: &at,
		})}
	}
	thr := strategy.Thresholds{Default: 80, PerWindow: per}
	return Row{HasEntry: true, Entry: usage.Entry{Snapshot: s}, Headroom: strategy.HeadroomOf(s, thr)}
}

func pct(v float64) *float64 { return &v }

// The row this note exists for: LEFT comes off seven_day and RESETS IN off the
// blown Fable cap, and `ccdad list` has no window column at any width.
func TestTheRowNamesBothWindowsWhenItsTwoFiguresComeFromTwo(t *testing.T) {
	r := splitRow(0, 80, pct(100), map[usage.WindowName]float64{
		usage.WindowFiveHour:                              100,
		usage.WindowSevenDay:                              117,
		usage.ScopedWindowName(usage.ScopeModel, "Fable"): 117,
	})

	// The two figures are unchanged by this note; only their names are added.
	if got := r.LeftLabel(); got != "20%" {
		t.Fatalf("LeftLabel() = %q, want 20%% off seven_day", got)
	}
	if got := r.UsedLabel(); got != "100%" {
		t.Fatalf("UsedLabel() = %q, want 100%% off the blown cap", got)
	}
	want := "  (weekly_scoped:model:Fable spent; 20% left on seven_day)"
	if got := r.SplitNote(); got != want {
		t.Errorf("SplitNote() = %q, want %q", got, want)
	}
}

func TestNoNoteWhenTheFloorIsTheBindingWindow(t *testing.T) {
	r := splitRow(10, 99, nil, nil)
	if got := r.SplitNote(); got != "" {
		t.Errorf("SplitNote() = %q, want empty — one window answers both figures", got)
	}
}

func TestNoNoteWhenThereIsNoFloorAtAll(t *testing.T) {
	r := splitRow(10, 20, pct(71), nil)
	if r.Headroom.HasFloor {
		t.Fatalf("HasFloor = true, want false — nothing is over its threshold")
	}
	if got := r.SplitNote(); got != "" {
		t.Errorf("SplitNote() = %q, want empty", got)
	}
}

// "spent" is a claim about the reported window having NOTHING LEFT, not about
// it being past a line. A cap merely over its threshold gets the note without
// the word.
func TestTheNoteSaysSpentOnlyWhenTheReportedWindowIsEmpty(t *testing.T) {
	r := splitRow(95, 20, pct(95), map[usage.WindowName]float64{
		usage.WindowFiveHour:                              40,
		usage.ScopedWindowName(usage.ScopeModel, "Fable"): 60,
	})
	note := r.SplitNote()
	if note == "" {
		t.Fatalf("SplitNote() is empty; the fixture must split (Floor=%q Binding=%q)", r.Headroom.Floor, r.Headroom.Binding)
	}
	if r.ReportedEmpty() {
		t.Fatal("ReportedEmpty() = true at 95% — only a window with nothing left is empty")
	}
	if contains(note, "spent") {
		t.Errorf("SplitNote() = %q, want no \"spent\" — the cap is over its line, not gone", note)
	}
}

// The gate is Floor != Binding and nothing else, so it covers the divergence
// that predates model scoping: a five-hour window binding on slack beside a
// weekly with nothing left. Narrowing it to the model-scope rule loses this.
func TestTheNoteCoversTheDivergenceThatPredatesModelScoping(t *testing.T) {
	r := splitRow(98, 100, nil, map[usage.WindowName]float64{
		usage.WindowFiveHour: 101,
		usage.WindowSevenDay: 108,
	})
	note := r.SplitNote()
	if note == "" {
		t.Fatalf("SplitNote() is empty (Floor=%q Binding=%q)", r.Headroom.Floor, r.Headroom.Binding)
	}
	if !contains(note, "seven_day") || !contains(note, "five_hour") {
		t.Errorf("SplitNote() = %q, want both window names and no model-scoped window in sight", note)
	}
}

func TestAnUnreadRowSaysNothing(t *testing.T) {
	if got := (Row{}).SplitNote(); got != "" {
		t.Errorf("SplitNote() on a bare Row = %q, want empty", got)
	}
	if got := (Row{Headroom: strategy.Headroom{Known: true}}).SplitNote(); got != "" {
		t.Errorf("SplitNote() with no floor = %q, want empty", got)
	}
}

// The Known clause on its own. The two cases above cannot see it: a bare Row
// has no floor either, so dropping Known leaves them answering empty for the
// other reason. This is the shape that tells them apart -- a floor and a
// binding window recorded on a headroom nothing could read, which is what a
// zero value carrying names would look like. Without the clause the row prints
// a note built from an unknown reading.
func TestAnUnknownHeadroomSaysNothingEvenCarryingTwoWindowNames(t *testing.T) {
	r := Row{Headroom: strategy.Headroom{
		Known:    false,
		HasFloor: true,
		Floor:    usage.WindowSevenDay,
		Binding:  usage.WindowFiveHour,
	}}
	if got := r.SplitNote(); got != "" {
		t.Errorf("SplitNote() on an unread headroom = %q, want empty", got)
	}
}

// ---- the width cut ----------------------------------------------------------

func TestWindowLabelShortCutsOnlyTheConstantPrefix(t *testing.T) {
	r := splitRow(0, 80, pct(100), map[usage.WindowName]float64{
		usage.ScopedWindowName(usage.ScopeModel, "Fable"): 50,
	})
	if got, want := r.WindowLabel(), "weekly_scoped:model:Fable"; got != want {
		t.Fatalf("WindowLabel() = %q, want %q — the full name is what status prints", got, want)
	}
	if got, want := r.WindowLabelShort(), "model:Fable"; got != want {
		t.Errorf("WindowLabelShort() = %q, want %q", got, want)
	}
}

// A fixed name carries no prefix and must come back untouched.
func TestWindowLabelShortLeavesAFixedNameAlone(t *testing.T) {
	r := splitRow(90, 20, nil, nil)
	if got := r.WindowLabelShort(); got != "five_hour" {
		t.Errorf("WindowLabelShort() = %q, want five_hour", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

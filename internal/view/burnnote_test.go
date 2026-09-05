package view

import (
	"strings"
	"testing"
)

// The rate has to reach the page. hover.go's own note refuses to read a
// fleet-wide rate on the ground that a threshold a user cannot follow is a
// threshold they cannot argue with, and publishing the figure is what makes the
// reading legal: the licence floor is one cooldown of WORK, and how many points
// that is depends entirely on this number.
func TestTheMeasuredRateIsPublishedUnderTheTable(t *testing.T) {
	s := Snapshot{Hover: true, BurnPerMin: 5.4}
	got := s.BurnNote()
	if !strings.Contains(got, "5.4 pts/min") {
		t.Errorf("BurnNote = %q, want the measured rate in it", got)
	}
	// One HoverCooldown of work at 5.4 a minute is 10.8 points.
	if !strings.Contains(got, "10.8 points") {
		t.Errorf("BurnNote = %q, want the floor the rate implies", got)
	}
}

// Off hover there is no derived threshold for the rate to explain, so the line
// is not drawn -- the same gate the other two hover sentences ride behind.
func TestTheRateIsNotPublishedWhenHoverIsOff(t *testing.T) {
	if got := (Snapshot{BurnPerMin: 5.4}).BurnNote(); got != "" {
		t.Errorf("BurnNote = %q off hover, want nothing", got)
	}
}

// An unmeasured fleet and a measured idle one draw the same page here: neither
// has a rate that moved a threshold, and a line reading "0.0 pts/min" would
// invite a reader to divide by it.
func TestAZeroRateDrawsNoLine(t *testing.T) {
	if got := (Snapshot{Hover: true}).BurnNote(); got != "" {
		t.Errorf("BurnNote = %q with no measurement, want nothing", got)
	}
}

// Order is a fact about the table: the reader meets the thresholds first, then
// the rate those thresholds were priced in, then what stranded them.
func TestTheRateLineFollowsTheHoverNoteAndPrecedesTheStrandedOne(t *testing.T) {
	rows := []Row{}
	got := TrailerLines(rows, Columns{}, true, "somebody is running wide", "hover:    measured burn 5.4 pts/min")
	want := []string{HoverNote, "hover:    measured burn 5.4 pts/min", "hover:    somebody is running wide"}
	if len(got) != len(want) {
		t.Fatalf("TrailerLines = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// And off hover none of the three is drawn, rate included.
func TestNoHoverSentenceSurvivesWithHoverOff(t *testing.T) {
	got := TrailerLines(nil, Columns{}, false, "stranded", "hover:    measured burn 5.4 pts/min")
	if len(got) != 0 {
		t.Errorf("TrailerLines = %v off hover, want nothing", got)
	}
}

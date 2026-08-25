package strategy

import (
	"math"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// nearly compares a figure this file states as a worked example to three
// decimals. The thresholds here are 100/6 arithmetic, so an exact literal would
// be a transcription of float64's rounding rather than of the example, and a
// reader could not check it against the sentence above it.
func nearly(got, want float64) bool { return math.Abs(got-want) < 0.001 }

// A weekly window with nothing left in it is what holds an account back, and
// under hover it says so on NO threshold at all: the pace target of a window far
// enough through its own cycle runs past 100, so a blown weekly reports POSITIVE
// slack and the "past its threshold" arm cannot see it.
//
// Getting this wrong is not an ordering bug -- MinPct files such an account in
// the empty tier either way -- it is a REPORTING one, and a loud one: Floor
// selects the window `measure` takes RecoversAt from, so without this arm an
// account whose week is gone for thirteen hours is reported as coming back in
// forty-five minutes, on the strength of a five-hour window that happened to
// bind.
func TestABlownWeeklyIsAFloorEvenWithAPaceTargetAbove100(t *testing.T) {
	// Six usable accounts, as in the live fleet, so the share is 100/6 and a
	// weekly 92% elapsed is measured against 108.67.
	c := sub("z", &usage.Snapshot{
		FiveHour: elapsedWindow(5*time.Hour, 0.85, 98),
		SevenDay: elapsedWindow(7*24*time.Hour, 0.92, 100),
	})
	pool := []Candidate{c}
	for i := 0; i < 5; i++ {
		pool = append(pool, sub(string(rune('a'+i)),
			&usage.Snapshot{SevenDay: elapsedWindow(7*24*time.Hour, 0.30, 40)}))
	}
	o := hoverOpts().withHover(pool)
	r := measure(c, o)

	if r.Headroom.Slack <= 0 {
		t.Fatalf("Slack = %v; this case is pointless unless the blown weekly reports positive slack",
			r.Headroom.Slack)
	}
	if !r.Headroom.HasFloor || r.Headroom.Floor != usage.WindowSevenDay {
		t.Fatalf("Floor = %q (has %v), want seven_day: it is the window with nothing left in it",
			r.Headroom.Floor, r.Headroom.HasFloor)
	}
	// The two pairs, side by side, because that is the whole point of carrying
	// the second one: Binding and Floor are different windows here, and a
	// reader that resolves the window through Floor and the number through
	// Slack reads a weekly with nothing left in it as three points of room.
	if r.Headroom.Binding != usage.WindowFiveHour {
		t.Fatalf("Binding = %q, want five_hour: 3.667 of slack is tighter than the weekly's 8.667",
			r.Headroom.Binding)
	}
	if !nearly(r.Headroom.Slack, 3.667) || !nearly(r.Headroom.Threshold, 101.667) {
		t.Errorf("binding pair = (%v, %v), want (3.667, 101.667): five_hour 98%% used against 85 elapsed plus a 16.667 share",
			r.Headroom.Slack, r.Headroom.Threshold)
	}
	if !nearly(r.Headroom.FloorSlack, 8.667) || !nearly(r.Headroom.FloorThreshold, 108.667) {
		t.Errorf("floor pair = (%v, %v), want (8.667, 108.667): seven_day 100%% used against 92 elapsed plus the same share",
			r.Headroom.FloorSlack, r.Headroom.FloorThreshold)
	}
	// The five-hour window resets in 45 minutes and the weekly in over thirteen
	// hours. Recovery has to name the second.
	if got := r.RecoversAt.Sub(o.Now); got < time.Hour {
		t.Errorf("RecoversAt = +%s, want the weekly's rollover rather than the five-hour one", got)
	}
	if r.ReturnsInsideHorizon {
		t.Error("ReturnsInsideHorizon = true; the week is gone for thirteen hours")
	}
}

// The other arm is untouched, and this is the case that proves it: a configured
// threshold of 80 makes an 85%-used weekly a floor, exactly as it always did. A
// person's threshold IS their stop line, so "past it" is the right question
// there even though the window is nowhere near empty.
func TestAConfiguredThresholdStillMakesATrippedWeeklyAFloor(t *testing.T) {
	c := sub("z", &usage.Snapshot{
		FiveHour: elapsedWindow(5*time.Hour, 0.85, 10),
		SevenDay: elapsedWindow(7*24*time.Hour, 0.30, 85),
	})
	h := HeadroomOf(c.Usage, Thresholds{Default: 80})
	if !h.HasFloor || h.Floor != usage.WindowSevenDay {
		t.Errorf("Floor = %q (has %v), want seven_day at 85%% under a threshold of 80",
			h.Floor, h.HasFloor)
	}
}

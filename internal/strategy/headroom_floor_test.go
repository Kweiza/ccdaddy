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

// creditOnly is a reading from a seat metered in money and nothing else: no
// plan window carried any figure, and extra_usage holds the whole meter. The
// numbers are a live claude_enterprise seat read on 2026-08-26, in the minor
// units the wire uses.
func creditOnly(utilization float64) *usage.Snapshot {
	u := utilization
	limit, used := 200000.0, 120451.0
	return &usage.Snapshot{ExtraUsage: usage.ExtraUsageFor(usage.ExtraUsageInput{
		State:        usage.ExtraUsageEnabled,
		Currency:     "USD",
		MonthlyLimit: &limit,
		UsedCredits:  &used,
		Utilization:  &u,
	})}
}

// TestHeadroomOrCreditReadsTheCreditAxisWhenNoWindowBinds is the display half
// of what rank.go:measure already does for the engine.
//
// HeadroomOf reads plan windows and nothing else, so a seat with none answers
// Known=false — and every caller that renders a percent then prints "?" while
// the engine, which reassigns the same seat onto the credit axis, is ranking it
// on a real number. One reading must not produce two answers.
func TestHeadroomOrCreditReadsTheCreditAxisWhenNoWindowBinds(t *testing.T) {
	h := HeadroomOrCredit(creditOnly(60.2255), Thresholds{})
	if !h.Known {
		t.Fatal("Known = false — the seat has a readable meter, it is just not a plan window")
	}
	if got, want := h.Pct, 100-60.2255; math.Abs(got-want) > 1e-9 {
		t.Errorf("Pct = %v, want %v", got, want)
	}
	if h.Binding != "extra_usage" {
		t.Errorf("Binding = %q, want %q", h.Binding, "extra_usage")
	}
	if got, want := h.Threshold, (Thresholds{}).CreditThreshold(); got != want {
		t.Errorf("Threshold = %v, want the credit threshold %v", got, want)
	}
}

// TestHeadroomOrCreditPrefersAPlanWindowOverTheCreditAxis is the guard on the
// fallback above, and it is the one that keeps a subscription honest.
//
// An ordinary account with overage switched on carries BOTH a plan window and a
// populated extra_usage. Its credits are not its meter — they are what it
// spends after the window runs out — so the window has to win, exactly as
// Classify's first branch already decides for the same reading.
func TestHeadroomOrCreditPrefersAPlanWindowOverTheCreditAxis(t *testing.T) {
	s := creditOnly(90)
	s.FiveHour = win(10, time.Hour)
	h := HeadroomOrCredit(s, Thresholds{})
	if !h.Known {
		t.Fatal("Known = false for a reading with a live five-hour window")
	}
	if h.Binding != usage.WindowFiveHour {
		t.Fatalf("Binding = %q, want %q — overage credits are not the meter while a window has room", h.Binding, usage.WindowFiveHour)
	}
	if h.Pct != 90 {
		t.Errorf("Pct = %v, want 90 — the five-hour window's own headroom", h.Pct)
	}
}

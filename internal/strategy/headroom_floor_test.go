package strategy

import (
	"math"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// A weekly window with nothing left in it is what holds an account back, and
// under a threshold of exactly 100 -- one a user may set -- it says so on no
// threshold at all: a window at 100 reports a slack of zero, so the "past its
// threshold" arm cannot see it.
//
// Getting this wrong is not an ordering bug -- MinPct files such an account in
// the empty tier either way -- it is a REPORTING one, and a loud one: Floor
// selects the window `measure` takes RecoversAt from, so without this arm an
// account whose week is gone for thirteen hours is reported as coming back in
// forty-five minutes, on the strength of a five-hour window that happened to
// bind.
func TestABlownWeeklyIsAFloorEvenWhenItsThresholdIs100(t *testing.T) {
	c := sub("z", &usage.Snapshot{
		FiveHour: elapsedWindow(5*time.Hour, 0.85, 98),
		SevenDay: elapsedWindow(7*24*time.Hour, 0.92, 100),
	})
	o := opts()
	o.Threshold = 100
	r := measure(c, o)

	// Not negative is the whole premise: `slack < 0` is false, so the floor's
	// EMPTY arm is the only thing that can catch this window.
	if r.Headroom.Slack != 0 || r.Headroom.Threshold != 100 {
		t.Fatalf("binding pair = (%v, %v), want (0, 100): this case is pointless unless the blown weekly reports zero slack",
			r.Headroom.Slack, r.Headroom.Threshold)
	}
	if !r.Headroom.HasFloor || r.Headroom.Floor != usage.WindowSevenDay {
		t.Fatalf("Floor = %q (has %v), want seven_day: it is the window with nothing left in it",
			r.Headroom.Floor, r.Headroom.HasFloor)
	}
	if r.Headroom.FloorSlack != 0 || r.Headroom.FloorThreshold != 100 {
		t.Errorf("floor pair = (%v, %v), want (0, 100)", r.Headroom.FloorSlack, r.Headroom.FloorThreshold)
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

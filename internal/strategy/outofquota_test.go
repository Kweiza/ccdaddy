package strategy

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// hoverAt is one subscription account whose weekly window is `share` of the way
// through the week at `util` percent used, plus an untouched five-hour window so
// the weekly is what binds.
func hoverAt(uuid string, share, util float64) Candidate {
	return sub(uuid, snap(
		win(0, 4*time.Hour),
		elapsedWindow(7*24*time.Hour, share, util),
	))
}

// livePoolOf20260824 is the pool that produced the empty tier: six accounts, and
// the one with NOTHING left ranked first under the pace rotation hover ran then,
// because its threshold was capped at 99 and so its slack at -1 while every
// other account's pace target was far below its utilization.
func livePoolOf20260824() []Candidate {
	return []Candidate{
		hoverAt("1-official", 0.90, 100),   // 0 points left
		hoverAt("2-kweizaa", 0.30, 80),     // 20 points left
		hoverAt("3-tlfyvhsdlek", 0.15, 53), // 47 points left
		hoverAt("4-ejalrnrmf", 0.35, 70),   // 30 points left
		hoverAt("5-junseong", 0.51, 79),    // 21 points left
		hoverAt("6-chan", 0.34, 80),        // 20 points left
	}
}

func hoverOpts() Options {
	o := opts()
	o.Hover = true
	return o
}

// The account with nothing left goes last.
func TestHoverRanksTheEmptyAccountLast(t *testing.T) {
	r := Rank(livePoolOf20260824(), hoverOpts())
	got := order(r)
	if got[len(got)-1] != "1-official" {
		t.Errorf("order = %v, want the account at 100%% last", got)
	}
	if got[0] == "1-official" {
		t.Fatal("the empty account is still first")
	}
}

// And the engine moves to it from anywhere else -- and, crucially, OFF it.
func TestHoverSwitchesOffTheEmptyAccount(t *testing.T) {
	cands := livePoolOf20260824()
	o := hoverOpts()

	p := Decide(cands, o, Config{}, NewState(), "1-official")
	if p.Action != ActionSwitch {
		t.Fatalf("on the empty account: action = %s (%s), want switch", p.Action, p.Reason)
	}
	if p.Target.UUID == "1-official" {
		t.Fatal("switched to itself")
	}
	if left := p.Target.Headroom.MinPct; left <= 0 {
		t.Errorf("switched to an account with %.0f points left", left)
	}

	// And nothing anywhere in the pool switches TO it.
	for _, c := range cands {
		p := Decide(cands, o, Config{}, NewState(), c.UUID)
		if p.Action == ActionSwitch && p.Target.UUID == "1-official" {
			t.Errorf("live=%s switched onto the empty account", c.UUID)
		}
	}
}

// MinPct is not Pct, and the case where they disagree is the reason it exists:
// Pct is read off the least-SLACK window, which under a per-window table is not
// always the window that will run out.
func TestMinPctNamesTheWindowThatActuallyRanOut(t *testing.T) {
	// five_hour held to 20 at 25% used: least slack. seven_day held to 99 at
	// 95% used: more slack, and five points of room.
	c := sub("x", &usage.Snapshot{
		FiveHour: elapsedWindow(5*time.Hour, 0.03, 25),
		SevenDay: elapsedWindow(7*24*time.Hour, 0.90, 95),
	})
	o := perWindow(map[usage.WindowName]float64{usage.WindowFiveHour: 20, usage.WindowSevenDay: 99})

	h := measure(c, o).Headroom
	if h.Binding != usage.WindowFiveHour {
		t.Fatalf("Binding = %v, want five_hour: the fixture is not exercising the disagreement", h.Binding)
	}
	if h.Pct != 75 {
		t.Errorf("Pct = %v, want 75 (the five-hour window's raw room)", h.Pct)
	}
	if h.MinPct != 5 || h.MinWindow != usage.WindowSevenDay {
		t.Errorf("MinPct = %v on %v, want 5 on seven_day: the window that will actually stop the session",
			h.MinPct, h.MinWindow)
	}
}

// Spent and OutOfQuota are different questions, and under a threshold short of
// 100 they routinely give different answers for the same account.
func TestSpentAndOutOfQuotaAreDifferentQuestions(t *testing.T) {
	o := opts()
	o.Threshold = 60
	pool := livePoolOf20260824()

	spentCount, emptyCount := 0, 0
	for _, c := range pool {
		h := measure(c, o).Headroom
		if s, _ := Spent(h); s {
			spentCount++
		}
		if e, _ := OutOfQuota(h); e {
			emptyCount++
		}
	}
	if spentCount != 5 {
		t.Errorf("Spent said %d of 6; five accounts are past 60", spentCount)
	}
	if emptyCount != 1 {
		t.Errorf("OutOfQuota said %d of 6; exactly one account has nothing left", emptyCount)
	}
}

// "Empty implies spent" holds whatever the threshold. Under a threshold of
// exactly 100 -- one a user may set -- a window with nothing in it reports a
// slack of zero, not a negative one, so Spent's `Slack < 0` clause is false and
// the MinPct clause is the only one that can fire.
//
// Spent has to say so anyway. It gates allOver, which is the only thing that
// reaches ModeRecovery, so one account like this reading as roomy would take
// recovery mode away from the whole pool -- and headroomTier would file it in
// tier 0, ahead of every account that actually has quota.
func TestAnEmptyAccountIsSpentWhateverItsThreshold(t *testing.T) {
	o := opts()
	o.Threshold = 100
	r := measure(hoverAt("solo", 0.92, 100), o)

	if r.Headroom.Slack != 0 {
		t.Fatalf("Slack = %v; this test is pointless unless the slack is exactly zero", r.Headroom.Slack)
	}
	if spent, known := Spent(r.Headroom); !known || !spent {
		t.Errorf("Spent = %v (known %v) on an account at 100%% used", spent, known)
	}
	if empty, known := OutOfQuota(r.Headroom); !known || !empty {
		t.Errorf("OutOfQuota = %v (known %v); MinPct = %v", empty, known, r.Headroom.MinPct)
	}
	if got := headroomTier(r); got != tierEmpty {
		t.Errorf("headroomTier = %d, want tierEmpty: an account with nothing in it is not roomy", got)
	}
}

// A single threshold cannot produce the inversion at all: slack is raw room
// shifted by a constant, so the two axes order identically. This is what bounds
// the whole defect to per-window thresholds.
func TestASingleThresholdCannotInvert(t *testing.T) {
	o := opts()
	o.Threshold = 80
	r := Rank(livePoolOf20260824(), o)
	for i := 1; i < len(r.Order); i++ {
		if r.Order[i-1].Headroom.Pct < r.Order[i].Headroom.Pct {
			t.Fatalf("a single threshold inverted the order at #%d: %v", i, order(r))
		}
	}
}

// The arithmetic stays MONOTONE past the line: a window five points over is
// worse than one exactly empty. An ordering that flattened them -- a clamp of
// any kind on an empty window -- would shuffle two spent accounts by uuid.
func TestPastTheLineIsStillWorseThanExactlyEmpty(t *testing.T) {
	o := opts()
	o.Threshold = 100
	exact := measure(hoverAt("exact", 0.92, 100), o).Headroom
	over := measure(hoverAt("over", 0.92, 105), o).Headroom

	if over.Slack >= exact.Slack {
		t.Errorf("105%% used reports %v and 100%% reports %v; past the line has to be worse",
			over.Slack, exact.Slack)
	}
}

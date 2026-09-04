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

// livePoolOf20260824 is the pool that produced this whole change: six accounts,
// every one past the threshold hover derived for it, and the one with NOTHING
// left ranked first because HoverCap capped its threshold at 99 and so capped
// its slack at -1.
func livePoolOf20260824() []Candidate {
	return []Candidate{
		hoverAt("1-official", 0.90, 100),   // 0 points left, slack -1
		hoverAt("2-kweizaa", 0.30, 80),     // 20 points left, slack -33
		hoverAt("3-tlfyvhsdlek", 0.15, 53), // 47 points left, slack -22
		hoverAt("4-ejalrnrmf", 0.35, 70),   // 30 points left, slack -18
		hoverAt("5-junseong", 0.51, 79),    // 21 points left, slack -11
		hoverAt("6-chan", 0.34, 80),        // 20 points left, slack -29
	}
}

func hoverOpts() Options {
	o := opts()
	o.Hover = true
	return o
}

// The account with nothing left goes last, whatever its pace says.
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
// Pct is read off the least-SLACK window, which is not always the window that
// has run out.
func TestMinPctNamesTheWindowThatActuallyRanOut(t *testing.T) {
	// five_hour: 3% through its cycle at 25% used -> low threshold, least slack.
	// seven_day: 90% through the week at 95% used -> threshold 99, more slack.
	c := sub("x", &usage.Snapshot{
		FiveHour: elapsedWindow(5*time.Hour, 0.03, 25),
		SevenDay: elapsedWindow(7*24*time.Hour, 0.90, 95),
	})
	pool := append([]Candidate{c}, hoverAt("f1", 0.5, 10), hoverAt("f2", 0.5, 10),
		hoverAt("f3", 0.5, 10), hoverAt("f4", 0.5, 10), hoverAt("f5", 0.5, 10))

	h := measure(pool[0], hoverOpts().withHover(pool)).Headroom
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

// Spent and OutOfQuota are different questions, and under hover they routinely
// give different answers for the same account.
func TestSpentAndOutOfQuotaAreDifferentQuestions(t *testing.T) {
	o := hoverOpts()
	pool := livePoolOf20260824()
	o = o.withHover(pool)

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
	if spentCount != 6 {
		t.Errorf("Spent said %d of 6; the whole pool is ahead of its pace", spentCount)
	}
	if emptyCount != 1 {
		t.Errorf("OutOfQuota said %d of 6; exactly one account has nothing left", emptyCount)
	}
}

// "Empty implies spent" used to be arithmetic on a clamp -- 100 > HoverCap
// guaranteed a used-up window reported negative slack -- and the clamp is gone.
// So the pace target of an account far enough through its own window now runs
// PAST 100, and a window with nothing in it reports POSITIVE slack.
//
// Spent has to say so anyway. It gates allOver, which is the only thing that
// reaches ModeRecovery, so one account like this reading as roomy would take
// recovery mode away from the whole pool -- and headroomTier would file it in
// tier 0, ahead of every account that actually has quota.
func TestAnEmptyAccountIsSpentEvenWhenItsPaceTargetIsAbove100(t *testing.T) {
	// One usable account, so the share is the whole 100 points: a weekly window
	// 92% elapsed is measured against 192, and at 100% used that is +92 of slack.
	pool := []Candidate{hoverAt("solo", 0.92, 100)}
	o := hoverOpts().withHover(pool)

	h := measure(pool[0], o).Headroom
	// Not negative is the premise. The pace target is 192, so the subtraction
	// gives +92; the clamp on an empty window brings that to exactly 0. Either
	// way Spent's `Slack < 0` clause is false and the MinPct clause is the only
	// one that can fire, which is what this test holds.
	if h.Slack < 0 {
		t.Fatalf("Slack = %v; this test is pointless unless the slack is non-negative", h.Slack)
	}
	if spent, known := Spent(h); !known || !spent {
		t.Errorf("Spent = %v (known %v) on an account at 100%% used with slack %+.0f",
			spent, known, h.Slack)
	}
	if empty, known := OutOfQuota(h); !known || !empty {
		t.Errorf("OutOfQuota = %v (known %v); MinPct = %v", empty, known, h.MinPct)
	}
	if got := headroomTier(measure(pool[0], o)); got != 3 {
		t.Errorf("headroomTier = %d, want 3: an account with nothing in it is not roomy", got)
	}
}

// A single threshold cannot produce the inversion at all: slack is raw room
// shifted by a constant, so the two axes order identically. This is what bounds
// the whole defect to per-window thresholds, which is to say to hover.
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

// The clamp is 100-pct and not a flat zero, so the arithmetic stays MONOTONE
// past the line: a window five points over is worse than one exactly empty. An
// ordering that flattened them would shuffle two spent accounts by uuid.
func TestPastTheLineIsStillWorseThanExactlyEmpty(t *testing.T) {
	exact := measure(hoverAt("exact", 0.92, 100), hoverOpts().withHover([]Candidate{hoverAt("exact", 0.92, 100)})).Headroom
	over := measure(hoverAt("over", 0.92, 105), hoverOpts().withHover([]Candidate{hoverAt("over", 0.92, 105)})).Headroom

	if exact.Slack != 0 {
		t.Errorf("an exactly empty window reports %v of slack, want 0", exact.Slack)
	}
	if over.Slack >= exact.Slack {
		t.Errorf("105%% used reports %v and 100%% reports %v; past the line has to be worse",
			over.Slack, exact.Slack)
	}
}

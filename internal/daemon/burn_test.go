package daemon

import (
	"math"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// rolledWindows is windowsUsed with the five-hour window on the OTHER side of a
// rollover: a fresh reset five hours out, which is what a real cycle boundary
// looks like on the wire.
func rolledWindows(fiveHour, sevenDay float64) *usage.Snapshot {
	fiveHourResets := tickEpoch.Add(5 * time.Hour)
	weeklyResets := tickEpoch.Add(72 * time.Hour)
	return &usage.Snapshot{
		FiveHour: usage.NewWindow(&fiveHour, &fiveHourResets),
		SevenDay: usage.NewWindow(&sevenDay, &weeklyResets),
	}
}

func burnThresholds() func(string) strategy.Thresholds {
	return configuredThresholds(config.Config{Threshold: 80})
}

// commitTwice runs two polls ten minutes apart and hands back what the second
// one stored, so a case can state a pair of readings rather than a cache row.
func commitTwice(t *testing.T, first, second *usage.Snapshot) usage.PollState {
	t.Helper()
	isolateEngine(t)
	a := store.Account{UUID: "acct-1"}
	e := NewEngine()
	e.Rand = midJitter
	e.commit(a, first, tickEpoch, []string{a.UUID}, burnThresholds(), true, nil)
	e.commit(a, second, tickEpoch.Add(10*time.Minute), []string{a.UUID}, burnThresholds(), true, nil)
	entry, ok := cacheEntry(t, a.UUID)
	if !ok {
		t.Fatal("commit() wrote no cache entry")
	}
	return entry.Poll
}

// The rate the engine reads is measured across the two readings the poller
// actually took, and it is stored because the second reading overwrites the
// point the first one was.
func TestTwoPollsLeaveTheMeasuredBurnRateInTheCache(t *testing.T) {
	// five_hour is the binding window under a flat threshold of 80: 10 -> 64
	// points in ten minutes is 5.4 a minute, which is the rate this fleet was
	// measured at on 2026-09-05.
	got := commitTwice(t, windowsUsed(10, 5), windowsUsed(64, 5))
	if !got.HasBurn {
		t.Fatal("two readings of the same window left no burn rate")
	}
	if math.Abs(got.BurnPerMin-5.4) > 0.0001 {
		t.Errorf("BurnPerMin = %v, want 5.4", got.BurnPerMin)
	}
}

// The first reading measures nothing and still moves the anchor -- without that
// the second reading has nothing to subtract from and the rate never starts.
func TestTheFirstPollLeavesAnAnchorAndNoRate(t *testing.T) {
	isolateEngine(t)
	a := store.Account{UUID: "acct-1"}
	e := NewEngine()
	e.Rand = midJitter
	e.commit(a, windowsUsed(10, 5), tickEpoch, []string{a.UUID}, burnThresholds(), true, nil)

	entry, ok := cacheEntry(t, a.UUID)
	if !ok {
		t.Fatal("commit() wrote no cache entry")
	}
	if entry.Poll.HasBurn {
		t.Errorf("BurnPerMin = %v from a single reading; there is nothing to subtract",
			entry.Poll.BurnPerMin)
	}
	if !entry.Poll.LastBindingAt.Equal(tickEpoch) {
		t.Errorf("LastBindingAt = %v, want %v: the anchor must move or the rate never starts",
			entry.Poll.LastBindingAt, tickEpoch)
	}
	if want := tickEpoch.Add(time.Hour); !entry.Poll.LastBindingReset.Equal(want) {
		t.Errorf("LastBindingReset = %v, want %v", entry.Poll.LastBindingReset, want)
	}
	if entry.Poll.LastBindingWindow != usage.WindowFiveHour {
		t.Errorf("LastBindingWindow = %v, want five_hour", entry.Poll.LastBindingWindow)
	}
}

// A window that rolled between the two readings reports no NEW rate. The fall
// from 96 to 4 is a reset, and a rate taken across it would tell the projection
// the account is filling up.
func TestARolloverBetweenTwoPollsLeavesTheRateAlone(t *testing.T) {
	got := commitTwice(t, windowsUsed(96, 5), rolledWindows(4, 5))
	if got.HasBurn {
		t.Errorf("BurnPerMin = %v across a rollover", got.BurnPerMin)
	}
	// The anchor still moves, so the NEXT pair measures the current binding
	// window rather than reaching back across the boundary forever. After the
	// five-hour window rolls to 4% the weekly window at 5% is the tighter of the
	// two, so what the anchor follows is the change of BINDING WINDOW -- which is
	// the second reason a pair can fail to be a pair.
	if got.LastBindingWindow != usage.WindowSevenDay {
		t.Errorf("LastBindingWindow = %v, want seven_day: the anchor must follow the window that now binds",
			got.LastBindingWindow)
	}
	if want := tickEpoch.Add(72 * time.Hour); !got.LastBindingReset.Equal(want) {
		t.Errorf("LastBindingReset = %v, want %v", got.LastBindingReset, want)
	}
}

// An idle account measures zero, and zero is an ANSWER: it is what says no
// session is running here.
func TestAnIdleAccountRecordsAMeasuredZero(t *testing.T) {
	got := commitTwice(t, windowsUsed(40, 5), windowsUsed(40, 5))
	if !got.HasBurn {
		t.Fatal("an unchanged reading left no rate; a measured zero is not an absence")
	}
	if got.BurnPerMin != 0 {
		t.Errorf("BurnPerMin = %v, want 0", got.BurnPerMin)
	}
}

// A poll that failed moves neither anchor nor rate, so the next success measures
// across the whole gap instead of against a point nobody took.
func TestAFailedPollKeepsTheAnchorItNeverMoved(t *testing.T) {
	isolateEngine(t)
	a := store.Account{UUID: "acct-1"}
	e := NewEngine()
	e.Rand = midJitter
	e.commit(a, windowsUsed(10, 5), tickEpoch, []string{a.UUID}, burnThresholds(), true, nil)
	e.commit(a, nil, tickEpoch.Add(3*time.Minute), []string{a.UUID}, burnThresholds(), true, nil)
	e.commit(a, windowsUsed(64, 5), tickEpoch.Add(10*time.Minute), []string{a.UUID}, burnThresholds(), true, nil)

	entry, ok := cacheEntry(t, a.UUID)
	if !ok {
		t.Fatal("commit() wrote no cache entry")
	}
	if !entry.Poll.HasBurn {
		t.Fatal("a failed poll between two good ones destroyed the measurement")
	}
	if math.Abs(entry.Poll.BurnPerMin-5.4) > 0.0001 {
		t.Errorf("BurnPerMin = %v, want 5.4 measured across the whole ten minutes",
			entry.Poll.BurnPerMin)
	}
}

// A rate names the window it was measured on, and that name does not follow the
// anchor. When the binding window changes, no new rate is taken, so the old rate
// is still a statement about the OLD window while the anchor has moved on.
func TestTheBurnRateKeepsTheWindowItWasMeasuredOn(t *testing.T) {
	isolateEngine(t)
	a := store.Account{UUID: "acct-1"}
	e := NewEngine()
	e.Rand = midJitter
	commit := func(s *usage.Snapshot, at time.Duration) usage.PollState {
		e.commit(a, s, tickEpoch.Add(at), []string{a.UUID}, burnThresholds(), true, nil)
		entry, ok := cacheEntry(t, a.UUID)
		if !ok {
			t.Fatal("commit() wrote no cache entry")
		}
		return entry.Poll
	}
	commit(windowsUsed(10, 5), 0)
	if got := commit(windowsUsed(64, 5), 10*time.Minute); got.BurnWindow != usage.WindowFiveHour {
		t.Fatalf("BurnWindow = %q, want five_hour", got.BurnWindow)
	}
	got := commit(rolledWindows(0, 75), 20*time.Minute)
	if got.LastBindingWindow != usage.WindowSevenDay {
		t.Fatalf("LastBindingWindow = %q; the fixture needs the binding window to move", got.LastBindingWindow)
	}
	if got.BurnWindow != usage.WindowFiveHour {
		t.Errorf("BurnWindow = %q, want five_hour: a rate taken on five_hour was relabelled", got.BurnWindow)
	}
}

// Under hover a cap on one model family is not a limit, so it is not the window
// a poll measures the rate on either. Held to hover's one line, a Fable week past
// it used to be the account's tightest window, and the poller read a rate of zero
// off a cap nothing was spending against while the five-hour window beside it
// filled -- so the work's rate read zero, every candidate carried it, and the
// live account's own measured projection read a window its limits never look at.
//
// 99.5 rather than 100: a cap with nothing left already stops being the ordering
// axis once an all-model window is readable (strategy.HeadroomFor), and the hole
// was the cap past hover's line and not yet empty.
func TestABlownModelCapIsNotTheWindowAPollUnderHoverMeasures(t *testing.T) {
	isolateEngine(t)
	reading := func(five float64) *usage.Snapshot {
		s := windowsUsed(five, 5)
		pct, at := 99.5, tickEpoch.Add(72*time.Hour)
		s.Limits = []usage.Limit{usage.LimitFor(usage.LimitInput{
			Kind: "weekly_scoped", Model: "Fable", Percent: &pct, ResetsAt: &at,
		})}
		return s
	}
	a := store.Account{UUID: "acct-1"}
	plan := strategy.HoverThresholds([]strategy.Candidate{{UUID: a.UUID, Kind: identity.KindSubscription, Usage: reading(10)}},
		strategy.Options{Now: tickEpoch, Hover: true})
	e := NewEngine()
	e.Rand = midJitter
	e.commit(a, reading(10), tickEpoch, []string{a.UUID}, plan.For, true, nil)
	e.commit(a, reading(64), tickEpoch.Add(10*time.Minute), []string{a.UUID}, plan.For, true, nil)

	entry, ok := cacheEntry(t, a.UUID)
	if !ok {
		t.Fatal("commit() wrote no cache entry")
	}
	if entry.Poll.BurnWindow != usage.WindowFiveHour || !entry.Poll.HasBurn {
		t.Fatalf("rate measured on %q (has %v), want five_hour", entry.Poll.BurnWindow, entry.Poll.HasBurn)
	}
	if math.Abs(entry.Poll.BurnPerMin-5.4) > 0.0001 {
		t.Errorf("BurnPerMin = %v, want the five-hour window's 5.4", entry.Poll.BurnPerMin)
	}
}

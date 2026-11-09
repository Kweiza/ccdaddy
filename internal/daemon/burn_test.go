package daemon

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/pollpolicy"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/switcher"
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
	e.commit(a, first, tickEpoch, tickEpoch, []string{a.UUID}, burnThresholds(), true, nil)
	e.commit(a, second, tickEpoch.Add(10*time.Minute), tickEpoch.Add(10*time.Minute), []string{a.UUID}, burnThresholds(), true, nil)
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
	e.commit(a, windowsUsed(10, 5), tickEpoch, tickEpoch, []string{a.UUID}, burnThresholds(), true, nil)

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
	e.commit(a, windowsUsed(10, 5), tickEpoch, tickEpoch, []string{a.UUID}, burnThresholds(), true, nil)
	e.commit(a, nil, tickEpoch.Add(3*time.Minute), tickEpoch.Add(3*time.Minute), []string{a.UUID}, burnThresholds(), true, nil)
	e.commit(a, windowsUsed(64, 5), tickEpoch.Add(10*time.Minute), tickEpoch.Add(10*time.Minute), []string{a.UUID}, burnThresholds(), true, nil)

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
		e.commit(a, s, tickEpoch.Add(at), tickEpoch.Add(at), []string{a.UUID}, burnThresholds(), true, nil)
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
	e.commit(a, reading(10), tickEpoch, tickEpoch, []string{a.UUID}, plan.For, true, nil)
	e.commit(a, reading(64), tickEpoch.Add(10*time.Minute), tickEpoch.Add(10*time.Minute), []string{a.UUID}, plan.For, true, nil)

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

// The work's rate is taken only across two readings that were both live. A pair
// from an idle anchor to the first live reading is diluted by however long the
// account sat unused, so it sets nothing; the next live pair sets it; and a later
// idle reading keeps it, because it is the last thing the work was measured doing
// here and a switch away must not forget it.
func TestAPairAcrossASwitchIsNotALiveRate(t *testing.T) {
	isolateEngine(t)
	a := store.Account{UUID: "acct-1"}
	e := NewEngine()
	e.Rand = midJitter
	commit := func(five, week float64, at time.Duration, live bool) usage.PollState {
		e.commit(a, windowsUsed(five, week), tickEpoch.Add(at), tickEpoch.Add(at), []string{a.UUID}, burnThresholds(), live, nil)
		entry, ok := cacheEntry(t, a.UUID)
		if !ok {
			t.Fatal("commit() wrote no cache entry")
		}
		return entry.Poll
	}
	rateOn := func(p usage.PollState, w usage.WindowName) (usage.LiveBurn, bool) {
		for _, b := range p.LiveBurns {
			if b.Window == w {
				return b, true
			}
		}
		return usage.LiveBurn{}, false
	}

	// The week is the binding window throughout -- sixty points against the
	// five-hour window's fifty at most -- and the five-hour rate is measured
	// all the same: it is the window that runs out first.
	commit(10, 60, 0, false)
	if got := commit(20, 60.1, 10*time.Minute, true); len(got.LiveBurns) != 0 {
		t.Fatalf("LiveBurns = %+v from an idle anchor and a live reading", got.LiveBurns)
	}
	got := commit(50, 60.4, 20*time.Minute, true)
	five, ok := rateOn(got, usage.WindowFiveHour)
	if !ok || math.Abs(five.PerMin-3) > 1e-9 {
		t.Fatalf("LiveBurns = %+v after two live readings, want 3 a minute on five_hour", got.LiveBurns)
	}
	if week, ok := rateOn(got, usage.WindowSevenDay); !ok || math.Abs(week.PerMin-0.03) > 1e-9 {
		t.Errorf("LiveBurns = %+v, want 0.03 a minute on seven_day too", got.LiveBurns)
	}
	if !five.From.Equal(tickEpoch.Add(10*time.Minute)) || !five.At.Equal(tickEpoch.Add(20*time.Minute)) {
		t.Errorf("the five-hour rate spans %s..%s, want the two live readings", five.From, five.At)
	}
	got = commit(50, 60.4, 30*time.Minute, false)
	if five, ok := rateOn(got, usage.WindowFiveHour); !ok || five.PerMin != 3 || got.AnchorLive {
		t.Errorf("after an idle reading LiveBurns = %+v, AnchorLive = %v; want the live rate kept and an idle anchor",
			got.LiveBurns, got.AnchorLive)
	}
}

// A poll dispatched before a switch and committed after it is the new live
// account's first live reading, and it is labelled by who is live when it is
// COMMITTED -- or the account's first live pair waits a whole poll longer, and
// the idle cadence it would be given overwrites the prompt poll the switch asked
// for.
func TestAReadingIsLiveByWhoIsLiveWhenItIsCommitted(t *testing.T) {
	isolateEngine(t)
	a := store.Account{UUID: "acct-1"}
	now := tickEpoch
	e := movableEngine(t, &now, func(context.Context, string) (*usage.Snapshot, error) {
		return windowsUsed(10, 5), nil
	})
	cfg := config.Config{Threshold: 80, CreditThreshold: 80}
	e.setLive("someone-else", true)
	e.setLive(a.UUID, true) // the switch landed while the poll was in flight
	if err := e.poll(context.Background(), a, cfg, configuredThresholds(cfg), []string{a.UUID}, false); err != nil {
		t.Fatal(err)
	}
	entry, ok := cacheEntry(t, a.UUID)
	if !ok {
		t.Fatal("poll() wrote no cache entry")
	}
	if !entry.Poll.AnchorLive {
		t.Error("the reading committed after the switch was labelled idle")
	}
}

// The race the label cannot close: a poll sent while the account was idle, a
// switch onto it landing while that poll is in flight -- before the daemon
// relabels anything -- and then the poll committing. The switch's own record,
// written and read under the cache lock, makes the reading the live account's
// and keeps the prompt poll it scheduled; the prompt poll itself, sent after
// the switch, clears the record.
func TestAPollInFlightAcrossASwitchKeepsThePromptPoll(t *testing.T) {
	isolateEngine(t)
	a := store.Account{UUID: "acct-1"}
	now := tickEpoch
	cfg := config.Config{Threshold: 80, CreditThreshold: 80}
	e := movableEngine(t, &now, func(context.Context, string) (*usage.Snapshot, error) {
		return windowsUsed(10, 5), nil
	})
	e.setLive("someone-else", true)
	if err := e.poll(context.Background(), a, cfg, configuredThresholds(cfg), []string{a.UUID}, false); err != nil {
		t.Fatal(err)
	}

	now = tickEpoch.Add(10 * time.Minute)
	var prompt time.Time
	e.FetchUsage = func(context.Context, string) (*usage.Snapshot, error) {
		now = now.Add(time.Second)
		if err := switcher.SchedulePromptPoll(a.UUID, now, 1); err != nil {
			t.Fatal(err)
		}
		entry, _ := cacheEntry(t, a.UUID)
		prompt = entry.NextPollAt
		now = now.Add(2 * time.Second)
		return windowsUsed(11, 5), nil
	}
	if err := e.poll(context.Background(), a, cfg, configuredThresholds(cfg), []string{a.UUID}, false); err != nil {
		t.Fatal(err)
	}
	entry, _ := cacheEntry(t, a.UUID)
	if !entry.Poll.AnchorLive {
		t.Error("the reading in flight across the switch was labelled idle")
	}
	if entry.NextPollAt.After(prompt) {
		t.Errorf("next poll %s after the switch, want the prompt poll at %s", entry.NextPollAt.Sub(tickEpoch), prompt.Sub(tickEpoch))
	}
	if entry.Poll.PromptAt.IsZero() {
		t.Error("the prompt poll's record was cleared by a reading sent before the switch")
	}

	e.FetchUsage = func(context.Context, string) (*usage.Snapshot, error) { return windowsUsed(12, 5), nil }
	now = now.Add(pollpolicy.ServeTTL)
	if err := e.poll(context.Background(), a, cfg, configuredThresholds(cfg), []string{a.UUID}, true); err != nil {
		t.Fatal(err)
	}
	if entry, _ := cacheEntry(t, a.UUID); !entry.Poll.PromptAt.IsZero() {
		t.Error("the prompt poll, sent after the switch, left the record standing")
	}
}

// A pair is stored only within one live stint. A was read live twice, three
// minutes apart; the work then went elsewhere and came back, and A's next live
// reading is ten minutes after its last. That pair spans the absence -- its rate
// is diluted by however long the work was away -- so it is not stored, and the
// rate kept is the one measured before A was left.
func TestAPairAcrossAnAbsenceIsNotStored(t *testing.T) {
	isolateEngine(t)
	e := NewEngine()
	a := store.Account{UUID: "acct-1"}
	commit := func(five float64, at time.Duration) {
		e.commit(a, windowsUsed(five, 5), tickEpoch.Add(at), tickEpoch.Add(at), []string{a.UUID}, burnThresholds(), true, nil)
	}
	commit(10, 0)
	commit(13, 3*time.Minute)
	before, _ := cacheEntry(t, a.UUID)
	if err := switcher.SchedulePromptPoll(a.UUID, tickEpoch.Add(10*time.Minute), 1); err != nil {
		t.Fatal(err)
	}
	commit(14, 13*time.Minute)
	after, _ := cacheEntry(t, a.UUID)
	five := func(e usage.Entry) usage.LiveBurn {
		for _, b := range e.Poll.LiveBurns {
			if b.Window == usage.WindowFiveHour {
				return b
			}
		}
		return usage.LiveBurn{}
	}
	if got, want := five(after), five(before); got != want || math.Abs(want.PerMin-1) > 1e-9 {
		t.Errorf("five-hour rate %+v after the return, want the 1 a minute measured before it (%+v)", got, want)
	}
}

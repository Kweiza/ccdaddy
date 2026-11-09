package daemon

import (
	"context"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sync"
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
	// The prompt, derived again from the reading the in-flight poll took: the
	// next look as soon as that reading's serve floor allows, not the idle
	// cadence the poll was sent on.
	if limit := now.Add(pollpolicy.ServeTTL); entry.NextPollAt.After(limit) || prompt.After(limit) {
		t.Errorf("next poll %s after the switch, want the prompt poll by %s", entry.NextPollAt.Sub(tickEpoch), limit.Sub(tickEpoch))
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

// An engine state that cannot be read cannot say where the account's stint
// began, so a commit then makes no live pair: two live readings three minutes
// apart, with a state file that does not parse, store the reading and its
// schedule and no rate.
func TestAnUnreadableStateMakesNoLivePair(t *testing.T) {
	isolateEngine(t)
	e := NewEngine()
	a := store.Account{UUID: "acct-a"}
	path, err := strategy.StatePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.commit(a, windowsUsed(10, 5), tickEpoch, tickEpoch, []string{a.UUID}, burnThresholds(), true, nil)
	e.commit(a, windowsUsed(13, 5), tickEpoch.Add(3*time.Minute), tickEpoch.Add(3*time.Minute), []string{a.UUID}, burnThresholds(), true, nil)

	entry, ok := cacheEntry(t, a.UUID)
	if !ok || entry.Snapshot == nil || !entry.FetchedAt.Equal(tickEpoch.Add(3*time.Minute)) || entry.NextPollAt.IsZero() {
		t.Fatalf("the reading and its schedule were not kept: row %v, %+v", ok, entry)
	}
	if len(entry.Poll.LiveBurns) != 0 || !entry.Poll.LastPairAt.IsZero() {
		t.Errorf("a pair was made while the engine state could not be read: %+v", entry.Poll.LiveBurns)
	}
}

// A uuid removed and added again keeps nothing its predecessor measured. The
// row still holds the predecessor's reading from an hour before the account
// was added, with a rate and a running flag beside it; a switch onto the new
// account records itself, and the first reading commits. What the row then
// holds is that reading and nothing measured before it.
func TestACommitDropsAPredecessorsMeasurements(t *testing.T) {
	isolateEngine(t)
	e := NewEngine()
	e.Rand = midJitter
	a := store.Account{UUID: "acct-a", AddedAt: tickEpoch}
	if err := usage.WithCache(time.Second, func(c *usage.Cache) error {
		c.Put(a.UUID, usage.Entry{
			Snapshot: windowsUsed(90, 50), FetchedAt: tickEpoch.Add(-time.Hour),
			Poll: usage.PollState{
				AnchorLive: true, LastPairRunning: true, LastPairAt: tickEpoch.Add(-time.Hour),
				LiveBurns: []usage.LiveBurn{{PerMin: 2, Window: usage.WindowFiveHour, From: tickEpoch.Add(-63 * time.Minute), At: tickEpoch.Add(-time.Hour)}},
			},
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := switcher.SchedulePromptPoll(a.UUID, tickEpoch.Add(time.Minute), 1); err != nil {
		t.Fatal(err)
	}
	e.commit(a, windowsUsed(10, 5), tickEpoch.Add(2*time.Minute), tickEpoch.Add(2*time.Minute), []string{a.UUID}, burnThresholds(), true, nil)

	entry, _ := cacheEntry(t, a.UUID)
	if len(entry.Poll.LiveBurns) != 0 || entry.Poll.LastPairRunning || !entry.Poll.LastPairAt.IsZero() || entry.Poll.HasBurn {
		t.Errorf("the predecessor's measurements came back with the first reading: %+v", entry.Poll)
	}
	if !entry.FetchedAt.Equal(tickEpoch.Add(2 * time.Minute)) {
		t.Errorf("the row holds a reading from %s, want the first one", entry.FetchedAt.Sub(tickEpoch))
	}
}

// The backoff a re-added account earns survives its next commit. The row holds
// a predecessor's reading from before the account was added, and a switch's
// record since; the account's first two polls are refused with a 429 and no
// Retry-After. The second backoff is longer than the first: the schedule the
// first refusal earned is the account's own, and is not dropped with the
// predecessor's measurements.
func TestAReAddedAccountsBackoffSurvivesItsFirst429s(t *testing.T) {
	isolateEngine(t)
	a := store.Account{UUID: "acct-a", AddedAt: tickEpoch}
	if err := usage.WithCache(time.Second, func(c *usage.Cache) error {
		c.Put(a.UUID, usage.Entry{Snapshot: windowsUsed(90, 50), FetchedAt: tickEpoch.Add(-time.Hour)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := switcher.SchedulePromptPoll(a.UUID, tickEpoch.Add(time.Minute), 1); err != nil {
		t.Fatal(err)
	}
	now := tickEpoch.Add(2 * time.Minute)
	cfg := config.Config{Threshold: 80, CreditThreshold: 80}
	e := movableEngine(t, &now, func(context.Context, string) (*usage.Snapshot, error) {
		return nil, usage.StatusErrorFrom(429, http.Header{}, now)
	})
	e.setLive(a.UUID, true)
	backoff := func() time.Duration {
		t.Helper()
		_ = e.poll(context.Background(), a, cfg, configuredThresholds(cfg), []string{a.UUID}, true)
		entry, ok := cacheEntry(t, a.UUID)
		if !ok {
			t.Fatal("the poll left no row")
		}
		return entry.NextPollAt.Sub(now)
	}
	first := backoff()
	now = now.Add(first)
	if second := backoff(); second <= first {
		t.Errorf("the second 429's backoff is %s, the first's %s: the first was dropped with the predecessor's row", second, first)
	}
}

// A tick that switched and then could not re-read the cache dispatches from the
// copy it read before the switch, not from the empty cache a failed load
// returns -- which would have every account due at once.
func TestAFailedReReadAfterASwitchKeepsTheCacheReadBeforeIt(t *testing.T) {
	isolateEngine(t)
	if err := usage.WithCache(time.Second, func(c *usage.Cache) error {
		c.Put("acct-a", usage.Entry{Snapshot: snapshotWith(10), FetchedAt: tickEpoch, NextPollAt: tickEpoch.Add(time.Minute)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pre, err := usage.LoadCache()
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine()
	if got := e.cacheAfterSwitch(pre); got == pre {
		t.Error("a re-read that succeeded was not taken")
	}
	path, err := usage.CachePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := e.cacheAfterSwitch(pre); got != pre {
		t.Error("a failed re-read replaced the cache read before the switch")
	}
}

// A pair across an absence is not stored even when the cache never heard of the
// absence. A was read live twice, three minutes apart; the work went to B; then
// a switch back to A was recorded in the engine state, and the daemon stopped
// before the cache's record of it was written. A's next live reading, ten
// minutes after its last, would pair across the absence. It is not stored, so
// a later switch to B -- after which no selection-time check names A any more
// -- finds only the rate measured before A was left.
func TestAPairAcrossAnAbsenceOnlyTheStateRecordsIsNotStored(t *testing.T) {
	isolateEngine(t)
	e := NewEngine()
	a := store.Account{UUID: "acct-a"}
	commit := func(five float64, at time.Duration) {
		e.commit(a, windowsUsed(five, 5), tickEpoch.Add(at), tickEpoch.Add(at), []string{a.UUID}, burnThresholds(), true, nil)
	}
	recordSwitch := func(to string, at time.Duration) {
		if err := strategy.WithState(time.Second, func(st *strategy.State) error {
			st.RecordSwitch(to, tickEpoch.Add(at), strategy.Departure{})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	commit(10, 0)
	commit(13, 3*time.Minute)
	recordSwitch("acct-b", 4*time.Minute)
	if err := switcher.SchedulePromptPoll("acct-b", tickEpoch.Add(4*time.Minute), 1); err != nil {
		t.Fatal(err)
	}
	recordSwitch(a.UUID, 10*time.Minute)
	commit(14, 13*time.Minute)
	recordSwitch("acct-b", 14*time.Minute)

	entry, _ := cacheEntry(t, a.UUID)
	for _, b := range entry.Poll.LiveBurns {
		if b.Window == usage.WindowFiveHour && (!b.At.Equal(tickEpoch.Add(3*time.Minute)) || math.Abs(b.PerMin-1) > 1e-9) {
			t.Errorf("five-hour rate %+v, want the 1 a minute measured before A was left", b)
		}
	}
	if !entry.Poll.LastPairAt.Equal(tickEpoch.Add(3 * time.Minute)) {
		t.Errorf("A's latest pair is dated %s, want the one before it was left", entry.Poll.LastPairAt.Sub(tickEpoch))
	}
}

// Whether the account's latest live pair saw the work running, and when, is
// what the engine reads to say whether the work is running
// (strategy.Options.workSeenRunning), and it has them only from the cache row:
// a pair that moved records the work running, one that did not records a
// pause, each at its reading, and a reading that makes no live pair leaves the
// record as it was -- dated by its pair, so the engine sees how old it is
// rather than reading it as this reading's.
func TestTheCommitRecordsWhetherTheLatestLivePairRan(t *testing.T) {
	isolateEngine(t)
	e := NewEngine()
	e.Rand = midJitter
	a := store.Account{UUID: "acct-1"}
	commit := func(five float64, at time.Duration, live bool) (bool, time.Duration) {
		t.Helper()
		e.commit(a, windowsUsed(five, 5), tickEpoch.Add(at), tickEpoch.Add(at), []string{a.UUID}, burnThresholds(), live, nil)
		entry, ok := cacheEntry(t, a.UUID)
		if !ok {
			t.Fatal("commit() wrote no cache entry")
		}
		return entry.Poll.LastPairRunning, entry.Poll.LastPairAt.Sub(tickEpoch)
	}
	commit(10, 0, true)
	if running, at := commit(40, 10*time.Minute, true); !running || at != 10*time.Minute {
		t.Errorf("a live pair that moved thirty points recorded running %v at %s", running, at)
	}
	if running, at := commit(40, 20*time.Minute, true); running || at != 20*time.Minute {
		t.Errorf("a live pair that did not move recorded running %v at %s", running, at)
	}
	if running, at := commit(70, 30*time.Minute, true); !running || at != 30*time.Minute {
		t.Errorf("the work resuming recorded running %v at %s", running, at)
	}
	if running, at := commit(70, 40*time.Minute, false); !running || at != 30*time.Minute {
		t.Errorf("an idle reading, which makes no live pair, left running %v at %s; want the last pair's record, dated by that pair", running, at)
	}
}

// After a move, a pause opens no last resort before the new account has a live
// pair of its own -- through the commit that records the pairs and the
// evaluation that reads them. B ran first: 90% to 96% in three minutes, so its
// own record says running. The work then moved to A, on its last point, and a
// pair there saw a pause. It moved back to B, whose first live reading makes no
// pair; projected at the rate the work last ran, B is out before its second.
// The only place left is A's last point, and the work has paused: nothing
// moves, whatever B's own record says. Had A's pair seen the work running, the
// last resort would open, which is what says the case can tell the two apart.
func TestAPauseAfterAMoveOpensNoLastResortBeforeTheNewAccountsPair(t *testing.T) {
	for _, tc := range []struct {
		name   string
		aFrom  float64
		moving bool
	}{
		{"the work paused on A", 99, false},
		{"the work ran on A", 97, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateEngine(t)
			a := seedAccount(t, "acct-a", "org-a")
			b := seedAccount(t, "acct-b", "org-b")
			e := NewEngine()
			e.Rand = midJitter
			commit := func(acct store.Account, five float64, at time.Duration) {
				e.commit(acct, windowsUsed(five, 5), tickEpoch.Add(at), tickEpoch.Add(at), []string{acct.UUID}, burnThresholds(), true, nil)
			}
			switchTo := func(acct store.Account, at time.Duration) {
				if err := switcher.SchedulePromptPoll(acct.UUID, tickEpoch.Add(at), 1); err != nil {
					t.Fatal(err)
				}
			}
			commit(b, 90, 0)
			commit(b, 96, 3*time.Minute)
			switchTo(a, 3*time.Minute)
			commit(a, tc.aFrom, 4*time.Minute)
			commit(a, 99, 5*time.Minute)
			switchTo(b, 6*time.Minute)
			commit(b, 96, 6*time.Minute)
			liveAs(t, b.UUID)

			s, err := store.Open()
			if err != nil {
				t.Fatal(err)
			}
			cfg := config.Defaults()
			cfg.Hover = true
			at := tickEpoch.Add(8*time.Minute + 5*time.Second)
			ev, err := switcher.Evaluate(s, switcher.EvalOptions{Now: at, Config: func() (config.Config, error) { return cfg, nil }})
			if err != nil {
				t.Fatal(err)
			}
			live := findRanked(t, ev.Plan.Result, b.UUID)
			if live.AtLimit() == "" || live.RunsOutAt.After(at) {
				t.Fatalf("B is at limit %q, out at %s: the case is not a live account projected out", live.AtLimit(), live.RunsOutAt.Sub(tickEpoch))
			}
			if entry, _ := cacheEntry(t, b.UUID); !entry.Poll.LastPairRunning || !entry.NextPollAt.After(tickEpoch.Add(6*time.Minute)) {
				t.Fatalf("B's record says running %v, next poll %s: the case is B's stale record before its second reading",
					entry.Poll.LastPairRunning, entry.NextPollAt.Sub(tickEpoch))
			}
			moved := ev.Plan.Action == strategy.ActionSwitch && ev.Plan.Target.UUID == a.UUID
			if moved != tc.moving {
				t.Errorf("%s (%s) to %q; want a move to A's last point %v", ev.Plan.Action, ev.Plan.Reason, ev.Plan.Target.UUID, tc.moving)
			}
		})
	}
}

// The prompt a switch schedules holds in the tick that made the switch. Three
// accounts of one organization: the live one is nearly spent and u-2 has room,
// so the tick moves the login. u-2 was last read six minutes ago and left with
// a danger-band schedule three minutes past, so the cache the tick read before
// the switch has it due; the switch holds it to the identity's share of the
// serve floor, nine minutes from that reading, and the tick polls nobody.
func TestASwitchsPromptHoldsInTheTickThatMadeIt(t *testing.T) {
	isolateEngine(t)
	for _, uuid := range []string{"u-1", "u-2", "u-3"} {
		seedAccount(t, uuid, "org-1")
	}
	liveAs(t, "u-1")
	if err := usage.WithCache(time.Second, func(c *usage.Cache) error {
		c.Put("u-1", usage.Entry{Snapshot: snapshotWith(95), FetchedAt: tickEpoch.Add(-time.Minute), NextPollAt: tickEpoch.Add(time.Minute)})
		c.Put("u-2", usage.Entry{Snapshot: snapshotWith(5), FetchedAt: tickEpoch.Add(-6 * time.Minute), NextPollAt: tickEpoch.Add(-3 * time.Minute)})
		c.Put("u-3", usage.Entry{Snapshot: snapshotWith(50), FetchedAt: tickEpoch.Add(-time.Minute), NextPollAt: tickEpoch.Add(time.Hour)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var polled []string
	e := engineFor(t, tokensAreFine, func(_ context.Context, token string) (*usage.Snapshot, error) {
		mu.Lock()
		defer mu.Unlock()
		polled = append(polled, token)
		return snapshotWith(5), nil
	})
	tick(t, e)
	if got := liveUUID(t); got != "u-2" {
		t.Fatalf("live = %q, want u-2: the case is a tick that switches", got)
	}
	if len(polled) != 0 {
		t.Errorf("the tick that switched polled %v; the prompt holds u-2 to the identity's floor", polled)
	}
	if entry, _ := cacheEntry(t, "u-2"); entry.NextPollAt.Before(tickEpoch.Add(3 * time.Minute)) {
		t.Errorf("u-2's next poll %s, want the identity's share of the floor after its reading", entry.NextPollAt.Sub(tickEpoch))
	}
}

// findRanked is the account uuid as res ranked it.
func findRanked(t *testing.T, res strategy.Result, uuid string) strategy.Ranked {
	t.Helper()
	for _, r := range append(append([]strategy.Ranked(nil), res.Order...), res.Credit...) {
		if r.UUID == uuid {
			return r
		}
	}
	t.Fatalf("%s is not in the pass", uuid)
	return strategy.Ranked{}
}

// inFlightSwitch runs one poll of a during which a switch onto a lands: the
// poll is sent while a is idle, the switch writes its record, and the poll
// returns reply. It is the race the switch's record exists for. identity is the
// accounts a polls its budget with, a among them.
func inFlightSwitch(t *testing.T, e *Engine, now *time.Time, a store.Account, identity []string, reply func() (*usage.Snapshot, error)) {
	t.Helper()
	cfg := config.Config{Threshold: 80, CreditThreshold: 80}
	e.FetchUsage = func(context.Context, string) (*usage.Snapshot, error) {
		*now = now.Add(time.Second)
		if err := switcher.SchedulePromptPoll(a.UUID, *now, len(identity)); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(2 * time.Second)
		return reply()
	}
	_ = e.poll(context.Background(), a, cfg, configuredThresholds(cfg), identity, false)
}

// An account with no cache row yet -- its first poll is the one in flight --
// still gets the switch's record, so that first reading commits as the live
// account's and on the prompt's schedule rather than the idle cadence.
func TestASwitchOntoAnAccountWithNoRowStillRecordsIt(t *testing.T) {
	isolateEngine(t)
	a := store.Account{UUID: "acct-1"}
	now := tickEpoch
	e := movableEngine(t, &now, nil)
	e.setLive("someone-else", true)
	inFlightSwitch(t, e, &now, a, []string{a.UUID}, func() (*usage.Snapshot, error) { return windowsUsed(10, 5), nil })
	entry, ok := cacheEntry(t, a.UUID)
	if !ok || !entry.Poll.AnchorLive {
		t.Errorf("the first reading, in flight across the switch, was labelled idle (row %v)", ok)
	}
	if limit := now.Add(pollpolicy.ServeTTL); entry.NextPollAt.After(limit) {
		t.Errorf("next poll %s, want the prompt poll by %s", entry.NextPollAt.Sub(tickEpoch), limit.Sub(tickEpoch))
	}
}

// The prompt poll yields to a 429. The poll in flight across the switch was
// refused with Retry-After 600, and the next look waits that out: congestion
// outranks the prompt as it outranks every other clock here.
func TestAPromptPollYieldsToA429InFlight(t *testing.T) {
	isolateEngine(t)
	a := store.Account{UUID: "acct-1"}
	now := tickEpoch
	cfg := config.Config{Threshold: 80, CreditThreshold: 80}
	e := movableEngine(t, &now, func(context.Context, string) (*usage.Snapshot, error) { return windowsUsed(10, 5), nil })
	e.setLive("someone-else", true)
	if err := e.poll(context.Background(), a, cfg, configuredThresholds(cfg), []string{a.UUID}, false); err != nil {
		t.Fatal(err)
	}
	now = tickEpoch.Add(10 * time.Minute)
	inFlightSwitch(t, e, &now, a, []string{a.UUID}, func() (*usage.Snapshot, error) {
		return nil, usage.StatusErrorFrom(429, http.Header{"Retry-After": []string{"600"}}, now)
	})
	entry, _ := cacheEntry(t, a.UUID)
	if floor := now.Add(600 * time.Second); entry.NextPollAt.Before(floor) {
		t.Errorf("next poll %s after a 429 with Retry-After 600, want no sooner than %s",
			entry.NextPollAt.Sub(now), floor.Sub(now))
	}
}

// The prompt the commit derives again is the identity's, as the switch's was:
// three accounts of one organization share its serve floor three ways, and the
// poll in flight across the switch does not buy the new live account the whole
// of it. That is the budget TestPromptPollsKeepAnIdentityInsideItsBudget
// counts.
func TestTheCommitsPromptIsTheIdentitysShare(t *testing.T) {
	isolateEngine(t)
	a := store.Account{UUID: "acct-1"}
	identity := []string{a.UUID, "acct-2", "acct-3"}
	now := tickEpoch
	cfg := config.Config{Threshold: 80, CreditThreshold: 80}
	e := movableEngine(t, &now, func(context.Context, string) (*usage.Snapshot, error) { return windowsUsed(10, 5), nil })
	e.setLive("someone-else", true)
	if err := e.poll(context.Background(), a, cfg, configuredThresholds(cfg), identity, false); err != nil {
		t.Fatal(err)
	}
	now = tickEpoch.Add(10 * time.Minute)
	inFlightSwitch(t, e, &now, a, identity, func() (*usage.Snapshot, error) { return windowsUsed(11, 5), nil })
	entry, _ := cacheEntry(t, a.UUID)
	if !entry.Poll.AnchorLive {
		t.Fatal("the reading in flight across the switch was labelled idle")
	}
	if floor := now.Add(pollpolicy.PerIdentity(pollpolicy.ServeTTL, len(identity))); entry.NextPollAt.Before(floor) {
		t.Errorf("next poll %s after the switch, want no sooner than the identity's share of the floor, %s",
			entry.NextPollAt.Sub(now), floor.Sub(now))
	}
}

package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// rolledOver is a reading whose five-hour clock ran down `ago` before the
// fixture clock: the window still names the rollover it reached, and nothing has
// polled since.
func rolledOver(ago time.Duration) *usage.Snapshot {
	pct := 38.0
	at := tickEpoch.Add(-ago)
	return &usage.Snapshot{FiveHour: usage.NewWindow(&pct, &at)}
}

// running is a reading whose five-hour clock has `left` still to go.
func running(left time.Duration) *usage.Snapshot {
	pct := 12.0
	at := tickEpoch.Add(left)
	return &usage.Snapshot{FiveHour: usage.NewWindow(&pct, &at)}
}

// The rollover arm, end to end. A window whose reported reset has PASSED is a
// clock that ran down, and the cached reading is simply older than the event.
//
// The build this replaces could not see it: its predicate required 0% AND no
// reset, so it waited for one poll to write {0, null} over the expired reading
// and for a later tick to notice — about 900 s of stopped clock per cycle, all of
// it in the minutes after the rollover, which is where starting a clock is worth
// the most.
func TestTheDaemonWarmsAWindowWhoseClockHasRunDown(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedLiveHolder(t, "u-live")
	seedEntry(t, "u-1", usage.Entry{
		Snapshot:  rolledOver(time.Minute),
		FetchedAt: tickEpoch.Add(-10 * time.Minute),
	})

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return rolledOver(time.Minute), nil
	})
	probes := stubProbe(t, e)
	tick(t, e)

	if len(*probes) != 1 || (*probes)[0].uuid != "u-1" {
		t.Fatalf("probes = %+v, want exactly one for u-1 — its five-hour reset is a minute in the "+
			"past, so the clock has stopped and the reading is merely older than the event", *probes)
	}
	got, _ := cacheEntry(t, "u-1")
	if got.Probe.Window != usage.WindowFiveHour {
		t.Errorf("Probe.Window = %q, want five_hour — without it nothing can judge whether the "+
			"turn started the clock it was aimed at", got.Probe.Window)
	}
}

// The structural bound, and the reason the rollover arm carries no interval at
// all: whatever else goes wrong with the schedule around it, one rollover buys
// one turn.
func TestAClockWarmedSinceItsRolloverIsNotWarmedAgain(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedLiveHolder(t, "u-live")
	seedEntry(t, "u-1", usage.Entry{
		Snapshot:  rolledOver(30 * time.Minute),
		FetchedAt: tickEpoch.Add(-10 * time.Minute),
		Probe: usage.ProbeState{
			// After the rollover, and long past every rung of the ladder — so
			// only the rollover arm can be what refuses.
			LastAttemptAt: tickEpoch.Add(-20 * time.Minute),
			Window:        usage.WindowFiveHour,
		},
	})

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return rolledOver(30 * time.Minute), nil
	})
	probes := stubProbe(t, e)
	tick(t, e)

	if len(*probes) != 0 {
		t.Fatalf("probes = %+v — this rollover has already had its turn", *probes)
	}
}

// The verdict, and it is taken from the WINDOW rather than from the child's exit
// status: an exit code cannot tell a turn that was billed and then failed from
// one that never authenticated.
func TestAReadingThatFindsTheClockStillStoppedRecordsAStrike(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedLiveHolder(t, "u-live")
	seedEntry(t, "u-1", usage.Entry{
		Snapshot:  unusedWindow(),
		FetchedAt: tickEpoch.Add(-40 * time.Minute),
		// Past the confirm deadline and inside the ladder's floor, so this tick
		// polls rather than warming and the reading is what runs.
		Probe: usage.ProbeState{
			LastAttemptAt: tickEpoch.Add(-usage.ProbeConfirmAfter - time.Minute),
			Window:        usage.WindowFiveHour,
		},
	})

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return unusedWindow(), nil
	})
	probes := stubProbe(t, e)
	tick(t, e)

	if len(*probes) != 0 {
		t.Fatalf("probes = %+v inside the ladder's floor", *probes)
	}
	got, _ := cacheEntry(t, "u-1")
	if n := got.Probe.Strikes(usage.WindowFiveHour); n != 1 {
		t.Fatalf("streak = %d, want 1 — the reading found the clock still stopped", n)
	}
	// And the rung it bought: an hour rather than the six the flat gate charged.
	if got := got.Probe.NextAttemptAt(usage.WindowFiveHour); !got.Equal(tickEpoch.Add(-usage.ProbeConfirmAfter - time.Minute + time.Hour)) {
		t.Errorf("next attempt at %s; one strike is the one-hour rung", got)
	}
}

// The rule the measurement forced. The turn-to-reset lag measured on a live
// account was 61-62 s and ProbePollDelay is 60 s, so the poll a warm-up
// schedules for itself routinely arrives while the endpoint is still deciding.
// Striking on that reading would walk working accounts to the six-hour ceiling
// and re-create the cold hour this whole design removes.
func TestTheConfirmPollMayNotStrike(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedLiveHolder(t, "u-live")
	attempt := tickEpoch.Add(-usage.ProbePollDelay)
	seedEntry(t, "u-1", usage.Entry{
		Snapshot:   unusedWindow(),
		FetchedAt:  tickEpoch.Add(-40 * time.Minute),
		NextPollAt: tickEpoch.Add(-time.Second),
		Probe:      usage.ProbeState{LastAttemptAt: attempt, Window: usage.WindowFiveHour},
	})

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return unusedWindow(), nil
	})
	stubProbe(t, e)
	tick(t, e)

	got, _ := cacheEntry(t, "u-1")
	if n := got.Probe.Strikes(usage.WindowFiveHour); n != 0 {
		t.Fatalf("streak = %d after the confirm poll, want 0", n)
	}
	if !got.FetchedAt.Equal(tickEpoch) {
		t.Fatal("the poll never ran, so this proves nothing about what it judged")
	}
}

// Warmth from ANY source clears the record, and the clause runs whether or not
// an attempt is outstanding. An account somebody used by hand is as good an
// answer as a warm-up, and one nobody warmed for a day must not carry yesterday's
// ladder into tonight.
func TestAReadingThatFindsTheClockRunningClearsTheStreak(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedLiveHolder(t, "u-live")
	seedEntry(t, "u-1", usage.Entry{
		Snapshot:  unusedWindow(),
		FetchedAt: tickEpoch.Add(-40 * time.Minute),
		// Three strikes in, so the rung is four hours and an attempt an hour ago
		// leaves this account polling rather than warming — which is the point:
		// the reading is what clears the record, not another turn.
		Probe: usage.ProbeState{
			LastAttemptAt: tickEpoch.Add(-time.Hour),
			Window:        usage.WindowFiveHour,
			ColdStreaks:   map[usage.WindowName]int{usage.WindowFiveHour: 3},
		},
	})

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return running(4 * time.Hour), nil
	})
	stubProbe(t, e)
	tick(t, e)

	got, _ := cacheEntry(t, "u-1")
	if !got.FetchedAt.Equal(tickEpoch) {
		t.Fatal("the poll never ran, so this proves nothing about what it judged")
	}
	if n := got.Probe.Strikes(usage.WindowFiveHour); n != 0 {
		t.Fatalf("streak = %d, want 0 — the clock is running", n)
	}
}

// The clamp. Without it the schedule walks past the rollover on the idle
// cadence, and the account's clock is stopped for whatever is left of that
// interval.
func TestTheNextPollIsAimedAtTheRollover(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedLiveHolder(t, "u-live")
	seedEntry(t, "u-1", usage.Entry{
		Snapshot:  running(5 * time.Minute),
		FetchedAt: tickEpoch.Add(-10 * time.Minute),
	})

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return running(5 * time.Minute), nil
	})
	// A fixed jitter sample, so the margin is exactly ProbeWakeMargin and the
	// assertion below is an instant rather than a range.
	e.Rand = func() float64 { return 0 }
	stubProbe(t, e)
	tick(t, e)

	got, _ := cacheEntry(t, "u-1")
	want := tickEpoch.Add(5*time.Minute + usage.ProbeWakeMargin)
	if !got.NextPollAt.Equal(want) {
		t.Errorf("NextPollAt = %s, want %s — the idle cadence would look ten minutes out and find "+
			"a clock that stopped five minutes earlier", got.NextPollAt, want)
	}
}

// A stand-down outranks the clamp. Congestion is the one thing allowed to hold a
// warm-up back, and Entry.PollAt takes the later of the two — so the guarantee is
// "warmed at rollover unless the endpoint is refusing us" and the doc says so.
func TestAStandDownStillOutranksTheRolloverAim(t *testing.T) {
	e := usage.Entry{
		NextPollAt:     tickEpoch.Add(time.Minute),
		StandDownUntil: tickEpoch.Add(30 * time.Minute),
	}
	if got := e.PollAt(false); !got.Equal(tickEpoch.Add(30 * time.Minute)) {
		t.Errorf("PollAt = %s, want the stand-down", got)
	}
}

// warmClamp's three branches, and the middle one is the only surprising one: a
// poll landing shortly BEFORE the target refreshes the reading, and due()'s own
// freshness floor then refuses the poll at the target.
func TestWarmClampKeepsTheTargetPollReachable(t *testing.T) {
	now := tickEpoch
	ttl := 3 * time.Minute
	for _, tc := range []struct {
		name            string
		natural, target time.Time
		want            time.Time
	}{{
		name:    "no target: the cadence stands",
		natural: now.Add(10 * time.Minute),
		want:    now.Add(10 * time.Minute),
	}, {
		name:    "the cadence would walk past the rollover",
		natural: now.Add(10 * time.Minute),
		target:  now.Add(6 * time.Minute),
		want:    now.Add(6 * time.Minute),
	}, {
		name:    "the cadence lands shortly before it, so pull it back a full ttl",
		natural: now.Add(9 * time.Minute),
		target:  now.Add(10 * time.Minute),
		want:    now.Add(7 * time.Minute),
	}, {
		name:    "the cadence lands long before it: its reading ages out on its own",
		natural: now.Add(2 * time.Minute),
		target:  now.Add(20 * time.Minute),
		want:    now.Add(2 * time.Minute),
	}, {
		name:    "the pull-back would land in the past, and scheduling cannot age a reading",
		natural: now.Add(time.Minute),
		target:  now.Add(2 * time.Minute),
		want:    now.Add(time.Minute),
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := warmClamp(tc.natural, tc.target, now, ttl); !got.Equal(tc.want) {
				t.Errorf("warmClamp = %s, want %s", got.Sub(now), tc.want.Sub(now))
			}
		})
	}
}

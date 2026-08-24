package usage

import (
	"errors"
	"testing"
	"time"
)

// warm is a reading whose five-hour window has a clock running until at.
func warm(t *testing.T, at time.Time) *Snapshot {
	t.Helper()
	pct := 12.0
	return &Snapshot{FiveHour: NewWindow(&pct, &at)}
}

// cold is a reading whose five-hour window has no clock: nothing spent, nothing
// to roll over.
func cold(t *testing.T) *Snapshot {
	t.Helper()
	pct := 0.0
	return &Snapshot{FiveHour: NewWindow(&pct, nil)}
}

// The rollover arm. Its whole point is that it is NOT an interval: a window that
// has rolled over gets exactly one warm-up for that rollover, whenever the
// scheduler gets to it, and the next one belongs to the next rollover. An
// implementation that kept a duration here would pass the first two cases and
// fail the third, which is the one that costs a turn of somebody's quota.
func TestMayProbeGivesOneWarmUpPerRollover(t *testing.T) {
	now := mustTime(t, "2026-08-24T12:00:00Z")
	rollover := now.Add(-30 * time.Minute)
	for _, tc := range []struct {
		name string
		last time.Time
		want bool
	}{
		{"never warmed", time.Time{}, true},
		{"warmed before this rollover, so its clock ran down since", rollover.Add(-4 * time.Hour), true},
		{"warmed one second before the rollover", rollover.Add(-time.Second), true},
		{"warmed at the rollover instant", rollover, false},
		{"warmed after this rollover — its turn is spent", rollover.Add(time.Minute), false},
		{"stamped in the future by a clock that moved back", now.Add(time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := ProbeState{LastAttemptAt: tc.last, Window: WindowFiveHour}
			if got := p.MayProbe(now, WindowFiveHour, rollover, true); got != tc.want {
				t.Errorf("MayProbe = %v, want %v", got, tc.want)
			}
		})
	}
}

// The rollover arm must not hand a free turn to a window whose warm-ups wake
// nothing. Without the streak test in MayProbe this is the flat gate's failure
// mode with extra steps: one wasted turn per rollover, forever.
func TestAStreakOutranksTheRolloverArm(t *testing.T) {
	now := mustTime(t, "2026-08-24T12:00:00Z")
	rollover := now.Add(-30 * time.Minute)
	p := ProbeState{
		LastAttemptAt: now.Add(-20 * time.Minute), // after the rollover
		Window:        WindowFiveHour,
		ColdStreaks:   map[WindowName]int{WindowFiveHour: 1},
	}
	// The rollover arm alone would refuse (already warmed since the rollover);
	// the ladder alone would refuse too (20m < 1h). Move the clock past the rung
	// and only the ladder can be what lets it through.
	if p.MayProbe(now, WindowFiveHour, rollover, true) {
		t.Error("a window one strike in was warmed again 20 minutes later")
	}
	later := p.LastAttemptAt.Add(time.Hour)
	if !p.MayProbe(later, WindowFiveHour, rollover, true) {
		t.Error("the ladder never opened; a streak must back off, not stop forever")
	}
}

// The ladder, rung by rung. The last rung is the property that matters most: it
// is ProbeRetryAfter, so an account nothing can wake is never attempted MORE
// often than the flat six-hour gate this replaced attempted it.
func TestTheLadderBacksOffAndNeverExceedsTheOldFlatGate(t *testing.T) {
	last := mustTime(t, "2026-08-24T12:00:00Z")
	for _, tc := range []struct {
		strikes int
		want    time.Duration
	}{
		{0, ProbeSettleGap},
		{1, time.Hour},
		{2, 2 * time.Hour},
		{3, 4 * time.Hour},
		{4, ProbeRetryAfter},
		{9, ProbeRetryAfter},
	} {
		p := ProbeState{LastAttemptAt: last, Window: WindowFiveHour}
		if tc.strikes > 0 {
			p.ColdStreaks = map[WindowName]int{WindowFiveHour: tc.strikes}
		}
		if got := p.NextAttemptAt(WindowFiveHour).Sub(last); got != tc.want {
			t.Errorf("%d strikes: gap = %s, want %s", tc.strikes, got, tc.want)
		}
		if got := p.NextAttemptAt(WindowFiveHour).Sub(last); got > ProbeRetryAfter {
			t.Errorf("%d strikes: gap %s exceeds the flat gate it replaced (%s)",
				tc.strikes, got, ProbeRetryAfter)
		}
		// The never-spent arm is the ladder with no rollover to fall back on.
		if got := p.MayProbe(last.Add(tc.want-time.Second), WindowFiveHour, time.Time{}, false); got {
			t.Errorf("%d strikes: warmed one second early", tc.strikes)
		}
		if got := p.MayProbe(last.Add(tc.want), WindowFiveHour, time.Time{}, false); !got {
			t.Errorf("%d strikes: still refused at the rung itself", tc.strikes)
		}
	}
}

// Per window and not per account. probeModel cannot express a model VERSION, so
// a weekly cap scoped to a build --model no longer resolves to is a window
// warm-ups can never start; with one counter, every five-hour rollover would
// retarget the slot and reset that window's ladder to the bottom rung.
func TestTheLadderIsHeldPerWindow(t *testing.T) {
	now := mustTime(t, "2026-08-24T12:00:00Z")
	p := ProbeState{
		LastAttemptAt: now.Add(-3 * time.Hour),
		Window:        WindowSevenDayOpus,
		ColdStreaks:   map[WindowName]int{WindowSevenDayOpus: 4},
	}
	if got := p.NextAttemptAt(WindowSevenDayOpus).Sub(p.LastAttemptAt); got != ProbeRetryAfter {
		t.Fatalf("the hopeless window is not at the cap: %s", got)
	}
	// Three hours in, the hopeless window is still held by its own six-hour rung.
	if p.MayProbe(now, WindowSevenDayOpus, time.Time{}, false) {
		t.Error("the capped window was attempted three hours into a six-hour rung")
	}
	// The five-hour window beside it has its own clean record, so its rollover
	// arm answers on the rollover alone. With one shared counter this reads the
	// opus window's four strikes, lands on the ladder, and refuses.
	rollover := now.Add(-time.Hour)
	if !p.MayProbe(now, WindowFiveHour, rollover, true) {
		t.Error("a hopeless weekly window paced the five-hour window's rollover")
	}
}

// The verdict is taken from the WINDOW and never from the child's exit status,
// and these are the four ways that matters.
func TestJudgedReadsTheWindowRatherThanTheExitCode(t *testing.T) {
	now := mustTime(t, "2026-08-24T12:00:00Z")
	attempt := now.Add(-11 * time.Minute)
	stale := now.Add(-40 * time.Minute)
	base := func() ProbeState {
		return ProbeState{LastAttemptAt: attempt, Window: WindowFiveHour}
	}

	t.Run("a running clock clears the streak even with nothing outstanding", func(t *testing.T) {
		p := base()
		p.ColdStreaks = map[WindowName]int{WindowFiveHour: 3}
		// A reading NEWER than the attempt: the verdict already ran. Warmth from
		// any source — a human using the account — still clears the record.
		got := p.Judged(now.Add(-time.Minute), warm(t, now.Add(4*time.Hour)), now)
		if got.Strikes(WindowFiveHour) != 0 {
			t.Errorf("streak = %d, want 0", got.Strikes(WindowFiveHour))
		}
	})

	t.Run("a rollover already passed is not a running clock", func(t *testing.T) {
		// The reading names a reset, and a verdict that only asked "is there a
		// reset" would call this a success and clear the streak forever.
		got := base().Judged(stale, warm(t, now.Add(-time.Minute)), now)
		if got.Strikes(WindowFiveHour) != 1 {
			t.Errorf("streak = %d, want 1 — a reset in the past is a stopped clock",
				got.Strikes(WindowFiveHour))
		}
	})

	t.Run("an outstanding attempt past the deadline takes a strike", func(t *testing.T) {
		got := base().Judged(stale, cold(t), now)
		if got.Strikes(WindowFiveHour) != 1 {
			t.Errorf("streak = %d, want 1", got.Strikes(WindowFiveHour))
		}
	})

	t.Run("the confirm poll may clear and may never strike", func(t *testing.T) {
		// This is the load-bearing one. The measured turn-to-reset lag is 61-62s
		// and ProbePollDelay is 60s, so the poll a warm-up schedules for itself
		// routinely arrives while the endpoint is still deciding. Striking on it
		// would walk working accounts to the six-hour cap.
		p := base()
		p.LastAttemptAt = now.Add(-ProbePollDelay)
		if got := p.Judged(stale, cold(t), now); got.Strikes(WindowFiveHour) != 0 {
			t.Errorf("streak = %d after the confirm poll, want 0", got.Strikes(WindowFiveHour))
		}
		if now.Sub(p.LastAttemptAt) >= ProbeConfirmAfter {
			t.Fatal("fixture no longer sits inside the deadline it exists to test")
		}
	})

	t.Run("a reading that predates nothing is not a verdict", func(t *testing.T) {
		// FetchedAt newer than the attempt: this reading already judged it.
		if got := base().Judged(now.Add(-time.Minute), cold(t), now); got.Strikes(WindowFiveHour) != 0 {
			t.Error("one attempt was judged twice")
		}
	})

	t.Run("an entry from before Window existed is inconclusive", func(t *testing.T) {
		p := base()
		p.Window = ""
		if got := p.Judged(stale, cold(t), now); got.Strikes(WindowFiveHour) != 0 {
			t.Error("an upgrade was read as evidence about an account")
		}
	})

	t.Run("no reading is no verdict", func(t *testing.T) {
		if got := base().Judged(stale, nil, now); got.Strikes(WindowFiveHour) != 0 {
			t.Error("a poll that failed was read as a failed warm-up")
		}
	})
}

// The map is cloned rather than written through: usage.Entry is passed by value
// and a map written in place is the one field of a "copy" that is not one.
func TestJudgedDoesNotWriteThroughToTheCallersMap(t *testing.T) {
	now := mustTime(t, "2026-08-24T12:00:00Z")
	shared := map[WindowName]int{WindowFiveHour: 2}
	p := ProbeState{
		LastAttemptAt: now.Add(-11 * time.Minute),
		Window:        WindowFiveHour,
		ColdStreaks:   shared,
	}
	if got := p.Judged(now.Add(-40*time.Minute), cold(t), now); got.Strikes(WindowFiveHour) != 3 {
		t.Fatalf("streak = %d, want 3", got.Strikes(WindowFiveHour))
	}
	if shared[WindowFiveHour] != 2 {
		t.Errorf("the caller's map was mutated: %d", shared[WindowFiveHour])
	}
}

// A window that has never been used and a window that is simply not there answer
// the same, and neither answers "now". Reading a missing reset as the zero time
// would put every such rollover in 1970.
func TestResetForSeparatesAReportedResetFromNoneAtAll(t *testing.T) {
	at := mustTime(t, "2026-08-24T17:00:00Z")
	pct := 40.0
	s := &Snapshot{
		FiveHour: NewWindow(&pct, &at),
		SevenDay: NewWindow(&pct, nil),
	}
	if got, ok := s.ResetFor(WindowFiveHour); !ok || !got.Equal(at) {
		t.Errorf("ResetFor(five_hour) = %s, %v; want %s, true", got, ok, at)
	}
	if _, ok := s.ResetFor(WindowSevenDay); ok {
		t.Error("a window with no resets_at reported one")
	}
	if _, ok := s.ResetFor(WindowSevenDayOpus); ok {
		t.Error("a window the response never carried reported a reset")
	}
	var nilSnap *Snapshot
	if _, ok := nilSnap.ResetFor(WindowFiveHour); ok {
		t.Error("a reading that does not exist reported a reset")
	}
}

// The three things a stamp has to do at once: record the attempt and the window
// it aimed at, push the poll that will read what it started out to
// ProbePollDelay, and leave the reading alone. And the one thing it must NOT do:
// touch the streak. RecordProbe is written twice for one warm-up — once before
// the spawn and once by the child — so a streak advanced here would be advanced
// twice for one turn.
func TestRecordProbeStampsTheAttemptAndHoldsTheNextPollOff(t *testing.T) {
	isolate(t)
	at := mustTime(t, "2026-08-24T12:00:00Z")
	pct := 0.0
	if err := WithCache(time.Second, func(c *Cache) error {
		c.Put("u-1", Entry{
			FetchedAt: at.Add(-time.Hour),
			Snapshot:  &Snapshot{FiveHour: NewWindow(&pct, nil)},
			Probe:     ProbeState{ColdStreaks: map[WindowName]int{WindowFiveHour: 2}},
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := RecordProbe(time.Second, "u-1", at, WindowFiveHour, errors.New("claude exited 1")); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCache()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c.Get("u-1")
	if !ok {
		t.Fatal("RecordProbe dropped the entry it was stamping")
	}
	if !got.Probe.LastAttemptAt.Equal(at) {
		t.Errorf("LastAttemptAt = %s, want %s", got.Probe.LastAttemptAt, at)
	}
	if got.Probe.Window != WindowFiveHour {
		t.Errorf("Window = %q; without it Judged cannot say which clock the turn was meant to start",
			got.Probe.Window)
	}
	if got.Probe.LastError == "" {
		t.Error("a failed probe left no record of itself, and a detached probe reports to nobody else")
	}
	if got.Probe.Strikes(WindowFiveHour) != 2 {
		t.Errorf("streak = %d, want 2 — stamping an attempt is not a verdict on it, and this "+
			"function runs twice per warm-up", got.Probe.Strikes(WindowFiveHour))
	}
	if want := at.Add(ProbePollDelay); !got.NextPollAt.Equal(want) {
		t.Errorf("NextPollAt = %s, want %s — a poll now would spend the usage budget on top of the "+
			"inference budget the probe just spent", got.NextPollAt, want)
	}
	if got.Snapshot == nil {
		t.Error("RecordProbe erased the reading that said a probe was needed")
	}
}

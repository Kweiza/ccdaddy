package strategy

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// leftAt is a state in which the engine left `from` at a limit on `closing`, an
// hour before the fixture clock -- long enough that no cooldown is in force.
func leftAt(from string, closing usage.WindowName) *State {
	st := NewState()
	st.RecordSwitch("someone-else", now.Add(-time.Hour), Departure{From: from, Closing: closing})
	return st
}

// The account just left at its limit holds fifteen points of its five-hour
// window, nobody is spending it, and its own reading says it has room. Taking it
// back is the flap: the rate returns with the session, the projection fires
// again, and the two accounts alternate. It is held out until the window that
// had it at its limit has rolled.
//
// The window it is held on started three hours ago, before the departure, and
// that is the comparison that holds it: its reset alone is in the future, as
// every reading's reset is.
func TestTheAccountJustLeftAtItsLimitIsHeldOutUntilItsWindowRolls(t *testing.T) {
	pool := []Candidate{
		maxAcct("a-left", 85, 2*time.Hour, 40, 3*day),
		weekOnly("u-live", 30, 2*day),
	}
	p := Decide(pool, hoverOpts(), Config{}, leftAt("a-left", usage.WindowFiveHour), "u-live")
	want(t, p, ActionStay, ReasonAlreadyBest, "")
	r, _ := find(p.Result, "a-left")
	if r.HeldOut != usage.WindowFiveHour || r.Rung() != RungHeldOut {
		t.Errorf("a-left held out on %q at rung %d, want five_hour at RungHeldOut", r.HeldOut, r.Rung())
	}
}

// And it comes back the moment that window rolls -- read either way the
// endpoint reports it: a window nothing has spent against since, or a window
// that started after the session left.
func TestTheSessionReturnsWhenTheFiveHourWindowRollsOver(t *testing.T) {
	for _, tc := range []struct {
		name string
		five usage.Window
	}{
		{"unspent since it rolled", usage.NewWindow(nil, nil)},
		{"started after the departure", win(3, 5*time.Hour-time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := []Candidate{
				sub("a-left", &usage.Snapshot{FiveHour: tc.five, SevenDay: win(40, 3*day)}),
				weekOnly("u-live", 30, 2*day),
			}
			p := Decide(pool, hoverOpts(), Config{}, leftAt("a-left", usage.WindowFiveHour), "u-live")
			want(t, p, ActionSwitch, ReasonShortWindowRoom, "a-left")
		})
	}
}

// A hold is a rung and never a filter: when everything else is at its limit or
// empty, the account held out is exactly where the session should go -- by
// whichever arm reaches it first, the pre-emptive walk or the ordinary one, so
// the case asserts the move and not the arm.
func TestAHeldOutAccountIsStillReachedWhenNothingElseCanServe(t *testing.T) {
	pool := []Candidate{
		maxAcct("a-left", 85, 2*time.Hour, 40, 3*day),
		weekOnly("u-live", 100, 2*day),
	}
	p := Decide(pool, hoverOpts(), Config{}, leftAt("a-left", usage.WindowFiveHour), "u-live")
	if p.Action != ActionSwitch || p.Target.UUID != "a-left" {
		t.Errorf("%s (%s) to %q, want a switch to the held-out account: nothing else can serve", p.Action, p.Reason, p.Target.UUID)
	}
}

// The marks are per account. One remembered departure would forget the first of
// two five-hour accounts the moment the second was left, and the first -- roomy
// on its reading, with nobody spending it -- would take the session straight
// back.
func TestTwoFiveHourAccountsLeftAtTheirLimitsAreBothHeldOut(t *testing.T) {
	st := NewState()
	st.RecordSwitch("a2", now.Add(-2*time.Hour), Departure{From: "a1", Closing: usage.WindowFiveHour})
	st.RecordSwitch("u-live", now.Add(-time.Hour), Departure{From: "a2", Closing: usage.WindowFiveHour})
	pool := []Candidate{
		maxAcct("a1", 85, 2*time.Hour, 40, 3*day),
		maxAcct("a2", 88, 3*time.Hour, 40, 4*day),
		weekOnly("u-live", 30, 2*day),
	}
	want(t, Decide(pool, hoverOpts(), Config{}, st, "u-live"), ActionStay, ReasonAlreadyBest, "")
}

// A mark holds an account out of being TAKEN BACK, and the live account is not
// being taken anywhere. The engine returns to a held-out account when nothing
// else could serve; once it is live it is a five-hour account with room, and a
// seat freeing up beside it is no reason to leave.
func TestALiveAccountIsNeverHeldOutOfItsOwnSession(t *testing.T) {
	pool := []Candidate{
		maxAcct("a-live", 85, 2*time.Hour, 40, 3*day),
		weekOnly("u-ultra", 30, 2*day),
	}
	p := Decide(pool, hoverOpts(), Config{}, leftAt("a-live", usage.WindowFiveHour), "a-live")
	want(t, p, ActionStay, ReasonAlreadyBest, "")
}

// Only a departure at a limit is remembered. A return, a move to a sooner week,
// a user's own switch -- none leaves an account anywhere it has to be held from.
func TestTheHoldIsOnlyForALimitDeparture(t *testing.T) {
	st := NewState()
	st.RecordSwitch("u2", now, Departure{From: "u1"})
	if got := st.departuresFor(""); len(got) != 0 {
		t.Errorf("departures = %v after a departure that named no closing window", got)
	}

	// And it does not clear an older mark either: leaving an account for some
	// other reason says nothing about whether its window has rolled.
	st = leftAt("a", usage.WindowFiveHour)
	st.RecordSwitch("u", now, Departure{From: "a"})
	if _, held := st.departuresFor("")["a"]; !held {
		t.Error("a departure that named no closing window cleared the mark on a")
	}
}

// Every switch says what it leaves, and names a closing window exactly when the
// account it leaves is at a limit. The pre-emptive and last-point arms are held
// by their own cases; these are the rest.
func TestADepartureNamesTheClosingWindowOnlyWhenALimitDroveTheMove(t *testing.T) {
	t.Run("off an empty account", func(t *testing.T) {
		pool := []Candidate{weekOnly("u-live", 100, 2*day), weekOnly("u-room", 30, 3*day)}
		p := Decide(pool, hoverOpts(), Config{}, NewState(), "u-live")
		if p.Action != ActionSwitch || p.Departure != (Departure{From: "u-live", Closing: usage.WindowSevenDay}) {
			t.Errorf("%s to %q, Departure = %+v, want u-live left on its empty week", p.Action, p.Target.UUID, p.Departure)
		}
	})
	t.Run("a return to a five-hour account", func(t *testing.T) {
		pool := []Candidate{maxAcct("a-max", 10, 3*time.Hour, 40, 3*day), weekOnly("u-live", 30, 2*day)}
		p := Decide(pool, hoverOpts(), Config{}, NewState(), "u-live")
		want(t, p, ActionSwitch, ReasonShortWindowRoom, "a-max")
		if p.Departure != (Departure{From: "u-live"}) {
			t.Errorf("Departure = %+v, want u-live left with nothing closing", p.Departure)
		}
	})
}

// The incident the cooldown alone could not stop: a session moved out and back
// two minutes and one second apart. Driven tick by tick through one five-hour
// cycle at a load that fills the window in two hours, the session leaves the
// five-hour account once, at its limit, and comes back once, when the window
// rolls -- and nothing in between.
func TestNoOutAndBackInsideAFiveHourCycle(t *testing.T) {
	fleet := []tierAccount{
		{uuid: "a-max", short: true, weekUtil: 10, weekAt: tierSimEpoch.Add(4 * day)},
		{uuid: "u-ultra", weekUtil: 10, weekAt: tierSimEpoch.Add(5 * day)},
	}
	// Five hours and ten minutes: the window started on the first turn, so it
	// rolls at five hours, and the poll that sees it is a margin after that.
	res := runTierSim(t, "one five-hour cycle", fleet, 0.025, 5*time.Hour+10*time.Minute)
	if res.total != 2 {
		t.Errorf("%d switches in one five-hour cycle, want 2: out at the limit and back at the rollover", res.total)
	}
	if res.switches[ReasonShortWindowRoom.String()] != 1 {
		t.Errorf("switches = %v, want exactly one return", res.switches)
	}
	if res.stopped != 0 {
		t.Errorf("%d ticks stopped by a limit", res.stopped)
	}
}

// ---- the mark on disk -----------------------------------------------------

// A daemon that restarted would otherwise forget every mark and take the
// session straight back, which is the cooldown's own reason for being on disk.
func TestTheDepartureSurvivesARestart(t *testing.T) {
	isolate(t)
	if err := WithState(time.Second, func(s *State) error {
		s.RecordSwitch("u-live", now.Add(-time.Hour), Departure{From: "a-left", Closing: usage.WindowFiveHour})
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}
	reloaded, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	m, ok := reloaded.departuresFor("")["a-left"]
	if !ok || m.Closing != usage.WindowFiveHour || !m.At.Equal(now.Add(-time.Hour)) {
		t.Fatalf("reloaded mark = %+v (%v), want five_hour an hour ago", m, ok)
	}
}

// The marks are the Claude lane's: the Codex lane runs no hover order to read
// them with, and its view of the state must not carry them.
func TestTheCodexViewCarriesNoDeparture(t *testing.T) {
	st := leftAt("a-left", usage.WindowFiveHour)
	if got := st.ForProvider(provider.Codex).departuresFor(""); len(got) != 0 {
		t.Errorf("the Codex view carries %v", got)
	}
	if got := st.ForProvider(provider.Claude).departuresFor(""); len(got) != 1 {
		t.Errorf("the Claude view carries %v, want the one mark", got)
	}
}

// A removed account must not come back held out when it is added again under
// the same uuid.
func TestPruneDropsTheDepartureOfARemovedAccount(t *testing.T) {
	st := leftAt("a-left", usage.WindowFiveHour)
	st.Prune(map[string]bool{"someone-else": true})
	if got := st.departuresFor(""); len(got) != 0 {
		t.Errorf("departures = %v after the account was removed", got)
	}
}

// ---- when a window has rolled ---------------------------------------------

// Every answer that cannot hold an account reads as rolled, and the two that
// cannot place the window in time hold it.
func TestRolledSinceReadsTheWindowsStartAndNotItsReset(t *testing.T) {
	left := now.Add(-time.Hour)
	five := func(w usage.Window) *usage.Snapshot { return &usage.Snapshot{FiveHour: w} }
	for _, tc := range []struct {
		name string
		s    *usage.Snapshot
		want bool
	}{
		{"started before the departure", five(win(85, 2*time.Hour)), false},
		{"started after the departure", five(win(3, 5*time.Hour-time.Minute)), true},
		// A reset a minute past the window's length is a window that has just
		// started, seen through a clock a little behind the endpoint's.
		{"a reset past the window's length", five(win(0, 5*time.Hour+time.Minute)), true},
		{"unspent since it rolled", five(usage.NewWindow(nil, nil)), true},
		{"none used and no reset", five(usage.NewWindow(pct(0), nil)), true},
		{"a reset already passed", five(win(85, -time.Minute)), true},
		{"no such window in the reading", &usage.Snapshot{SevenDay: win(40, 3*day)}, true},
		{"no reading at all", nil, true},
		{"used, with no reset to place it", five(usage.NewWindow(pct(85), nil)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rolledSince(tc.s, usage.WindowFiveHour, left, now); got != tc.want {
				t.Errorf("rolledSince = %v, want %v", got, tc.want)
			}
		})
	}
}

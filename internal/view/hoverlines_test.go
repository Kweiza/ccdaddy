package view

import (
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// hoverRow is one account's row under the table: a five-hour window resetting
// two hours after the fixture clock and a week, read and labelled by address.
func hoverRow(uuid, email string, active bool) Row {
	r := fleetRow(uuid, 50, 40, nil, 0)
	r.Account = store.Account{UUID: uuid, Email: email}
	r.Active = active
	return r
}

// The order is hover's whole decision and no cell shows it, so every account
// gets a line saying where the order put it and the one fact that put it there
// -- including the account the session is on, and one nobody could read.
func TestHoverLinesSayWhereTheOrderPutEachAccountAndWhy(t *testing.T) {
	room := strategy.Headroom{Known: true, MinPct: 50, MinAnyModelPct: 50, MinAnyModelWindow: usage.WindowSevenDay}
	s := Snapshot{
		Now:   colNow,
		Hover: true,
		Rows: []Row{
			hoverRow("u-a", "a@example.com", true),
			hoverRow("u-b", "b@example.com", false),
			hoverRow("u-h", "h@example.com", false),
			hoverRow("u-l", "l@example.com", false),
			hoverRow("u-e", "e@example.com", false),
		},
		HoverOrder: []strategy.Ranked{
			{UUID: "u-a", Headroom: room, ShortWindow: usage.WindowFiveHour,
				WeeklyResetsAt: colNow.Add(40 * time.Hour), HasWeeklyReset: true},
			{UUID: "u-b", Headroom: room},
			{UUID: "u-h", Headroom: room, ShortWindow: usage.WindowFiveHour, HeldOut: usage.WindowFiveHour},
			{UUID: "u-x"},
			{UUID: "u-l", Headroom: room, Closing: usage.WindowFiveHour},
			{UUID: "u-e", Headroom: strategy.Headroom{Known: true, MinAnyModelWindow: usage.WindowFiveHour}},
		},
	}
	want := []string{
		HoverNote,
		"hover:    1. a@example.com  short-window · week resets Sun 04:00 · live",
		"hover:    2. b@example.com  weekly-only · no weekly reset",
		"hover:    3. h@example.com  held out until five_hour rolls over at Fri 14:00",
		"hover:    4. u-x  unreadable",
		"hover:    5. l@example.com  at its limit on five_hour",
		"hover:    6. e@example.com  nothing left on five_hour",
	}
	if got := s.HoverLines(); !eq(got, want) {
		t.Errorf("HoverLines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// In recovery mode the order in force is recovery's, not hover's rungs, and the
// lines have to say so: a rung printed beside an order the rungs did not make
// explains a decision nobody took. They also have to say what that order IS,
// and it is not simply soonest-back: an empty account goes last however soon it
// returns, and outside the hour the account least past its line goes first
// however late it returns. The order here is the engine's own, ranked from
// readings, so the lines are checked against what lessRecovery did rather than
// against what a fixture says it did.
func TestHoverLinesSayRecoveryWhenTheOrderIsRecovery(t *testing.T) {
	if strategy.DefaultRecoveryHorizon != time.Hour {
		t.Fatalf("the recovery horizon is %s; HoverRecoveryNote says \"within the hour\"", strategy.DefaultRecoveryHorizon)
	}
	reading := func(uuid string, s *usage.Snapshot) strategy.Candidate {
		return strategy.Candidate{UUID: uuid, Kind: identity.KindSubscription, Usage: s}
	}
	five := func(pct float64, in time.Duration) *usage.Snapshot {
		return &usage.Snapshot{FiveHour: window(pct, colNow.Add(in))}
	}
	week := func(pct float64, in time.Duration) *usage.Snapshot {
		return &usage.Snapshot{SevenDay: window(pct, colNow.Add(in))}
	}
	res := strategy.Rank([]strategy.Candidate{
		reading("u-d", five(100, 10*time.Minute)),
		reading("u-b", week(99.8, 48*time.Hour)),
		reading("u-a", week(99.2, 72*time.Hour)),
		reading("u-c", five(99.5, 30*time.Minute)),
	}, strategy.Options{Now: colNow, Hover: true})
	if res.Mode != strategy.ModeRecovery {
		t.Fatalf("mode = %s; every account is past its last point, so the fixture is not in recovery", res.Mode)
	}

	s := Snapshot{
		Now: colNow, Hover: true, HasMode: true, Mode: res.Mode, HoverOrder: res.Order,
		Rows: []Row{
			hoverRow("u-a", "a@example.com", true), hoverRow("u-b", "b@example.com", false),
			hoverRow("u-c", "c@example.com", false), hoverRow("u-d", "d@example.com", false),
		},
	}
	want := []string{
		HoverNote,
		// Spelled out rather than HoverRecoveryNote, so the sentence is held to
		// the order below it and not to itself.
		"hover:    recovery: every account is past its last point; accounts with something left come " +
			"before empty ones, and within each, those back within the hour come first, soonest first, " +
			"then the rest, the least past its line first",
		"hover:    1. c@example.com  back at Fri 12:30",
		"hover:    2. a@example.com  0.2 past its line · back at Mon 12:00 · live",
		"hover:    3. b@example.com  0.8 past its line · back at Sun 12:00",
		"hover:    4. d@example.com  nothing left on five_hour · back at Fri 12:10",
	}
	if got := s.HoverLines(); !eq(got, want) {
		t.Errorf("HoverLines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Off hover there is nothing to say: the order is not hover's, and the rule the
// first line states is not the one moving the session.
func TestHoverLinesAreEmptyWithHoverOff(t *testing.T) {
	s := Snapshot{Now: colNow, Rows: []Row{hoverRow("u-a", "a@example.com", true)},
		HoverOrder: []strategy.Ranked{{UUID: "u-a"}}}
	if got := s.HoverLines(); len(got) != 0 {
		t.Errorf("HoverLines = %q with hover off, want nothing", got)
	}
}

// An account whose levels say it has room is never "at its limit" -- its rung
// reads levels alone -- and when the work running now would run it out before
// its first live reading, the line says that instead, with when. An idle Max with
// eight points at three points a minute is the case: the hand-off passes it over,
// and a line that printed only its rung would leave that unexplained.
func TestHoverLinesSayWhenAnAccountCannotCarryTheWork(t *testing.T) {
	five := func(pct float64) *usage.Snapshot {
		return &usage.Snapshot{FiveHour: window(pct, colNow.Add(3*time.Hour)), SevenDay: window(40, colNow.Add(40*time.Hour))}
	}
	live := strategy.Candidate{UUID: "u-a", Kind: identity.KindSubscription, Usage: five(40),
		FetchedAt: colNow, NextPollAt: colNow.Add(3 * time.Minute),
		LiveBurns: []usage.LiveBurn{{PerMin: 3, Window: usage.WindowFiveHour, From: colNow.Add(-3 * time.Minute), At: colNow}}}
	idle := strategy.Candidate{UUID: "u-b", Kind: identity.KindSubscription, Usage: five(92),
		FetchedAt: colNow, NextPollAt: colNow.Add(10 * time.Minute)}
	// A carrier and an account on its last point, so the note is seen to be
	// the non-carrier's alone: the one carries the work, and the other is at a
	// limit, which its rung already says.
	roomy := strategy.Candidate{UUID: "u-c", Kind: identity.KindSubscription, Usage: five(10),
		FetchedAt: colNow, NextPollAt: colNow.Add(10 * time.Minute)}
	last := strategy.Candidate{UUID: "u-d", Kind: identity.KindSubscription, Usage: five(99.5),
		FetchedAt: colNow, NextPollAt: colNow.Add(10 * time.Minute)}
	o := strategy.Options{Now: colNow, Hover: true}
	p := strategy.Decide([]strategy.Candidate{live, idle, roomy, last}, o, strategy.Config{}, strategy.NewState(), "u-a")

	s := Snapshot{Now: colNow, Hover: true, HasMode: true, Mode: p.Result.Mode, HoverOrder: p.Result.Order,
		Rows: []Row{hoverRow("u-a", "a@example.com", true), hoverRow("u-b", "b@example.com", false),
			hoverRow("u-c", "c@example.com", false), hoverRow("u-d", "d@example.com", false)}}
	lines := s.HoverLines()
	got := strings.Join(lines, "\n")
	if want := "b@example.com  short-window · week resets Sun 04:00 · cannot carry the work running now (runs out in 3 min)"; !strings.Contains(got, want) {
		t.Errorf("HoverLines =\n%s\nwant a line ending %q", got, want)
	}
	for _, l := range lines {
		if strings.Contains(l, "at its limit") && !strings.Contains(l, "d@example.com") {
			t.Errorf("HoverLines =\n%s\nsay an account with room is at its limit", got)
		}
		if strings.Contains(l, "cannot carry") && !strings.Contains(l, "b@example.com") {
			t.Errorf("line %q says it cannot carry the work: only b's should", l)
		}
	}
}

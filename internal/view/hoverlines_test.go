package view

import (
	"strings"
	"testing"
	"time"

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

// In recovery mode the order in force is soonest-back, not hover's rungs, and the
// lines have to say so: a rung printed beside an order the rungs did not make
// explains a decision nobody took.
func TestHoverLinesSayRecoveryWhenTheOrderIsRecovery(t *testing.T) {
	s := Snapshot{
		Now: colNow, Hover: true, HasMode: true, Mode: strategy.ModeRecovery,
		Rows: []Row{hoverRow("u-a", "a@example.com", true), hoverRow("u-b", "b@example.com", false)},
		HoverOrder: []strategy.Ranked{
			{UUID: "u-b", ShortWindow: usage.WindowFiveHour, HasRecovery: true, RecoversAt: colNow.Add(2 * time.Hour)},
			{UUID: "u-a", ShortWindow: usage.WindowFiveHour},
		},
	}
	want := []string{
		HoverNote,
		HoverRecoveryNote,
		"hover:    1. b@example.com  back at Fri 14:00",
		"hover:    2. a@example.com  names no rollover · live",
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

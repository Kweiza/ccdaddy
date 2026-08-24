package tui

import (
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// progress.ViewAs takes a float64 and has no absence channel: measured on a
// 10-cell bar, 0.0, -0.5, NaN and +Inf all render "..........". An unread
// account would be byte-identical to one at 0%, which is the bug that parked
// cswap's engine -- one expired token made every account look empty and it
// settled on whichever reset last.
//
// There are TWO absences here and they arrive by different routes.
func TestAnAccountThatCouldNotBeReadRendersAQuestionMarkAndNoBar(t *testing.T) {
	g := newGauge()
	for _, tc := range []struct {
		name string
		row  view.Row
	}{
		{"no cache entry at all", rowWithNoEntry()},
		{"an entry whose headroom is unknown", rowWithUnknownHeadroom()},
		{"a present window that reported no utilization", rowWithSilentWindow(usage.WindowFiveHour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := usedCell(tc.row, g)
			if got != view.Unreadable {
				t.Fatalf("usedCell = %q, want exactly %q", got, view.Unreadable)
			}
			if strings.ContainsAny(got, "#.[]") {
				t.Fatalf("usedCell = %q: a bracket implies a reading, and there was none", got)
			}
		})
	}
}

// The other half. Zero is a READING and must render as one, or the fix for the
// test above is "never draw a bar".
func TestAnAccountAtZeroPercentStillRendersAnEmptyBarAndNotAQuestionMark(t *testing.T) {
	got := usedCell(rowAtPercent(0), newGauge())
	want := "[..........]   0%"
	if got != want {
		t.Fatalf("usedCell(0%%) = %q, want %q", got, want)
	}
}

// AccountState is a STRING type, so a switch with no default falls out of every
// case and leaves the caller holding its zero value -- which reads as active.
// The document contract is additive and guarantees a newer daemon publishing a
// state this binary has never heard of, on upgrade day.
//
// stateCell's third return isn't asserted here: lipgloss.Style carries a
// []color.Color field (border blending) and a func field, so it has no ==
// and every style var is today's identical lipgloss.NewStyle() zero value
// anyway -- colour is a later commit's call, per the task. glyph and text
// already prove the value was carried through rather than silently zeroed,
// which is the failure this test exists to catch.
func TestAStateThisBinaryHasNeverHeardOfIsCarriedThroughAndNeverReadsAsActive(t *testing.T) {
	glyph, text, _ := stateCell(daemon.AccountState("draining"))
	if glyph != "?" {
		t.Errorf("glyph = %q, want %q", glyph, "?")
	}
	if text != "draining" {
		t.Errorf("text = %q, want the raw value carried through", text)
	}
}

// The empty state is real: AccountStatus.State is json:"state,omitempty" and is
// filled from a map lookup that returns the zero value on a miss, so an account
// no daemon has ever published carries "".
func TestAnAccountNoDaemonHasEverPublishedRendersADashAndNoGlyph(t *testing.T) {
	glyph, text, _ := stateCell("")
	if glyph != "" || text != "-" {
		t.Fatalf("stateCell(\"\") = (%q, %q), want (\"\", \"-\")", glyph, text)
	}
}

// Six named arms, the empty arm, and the default. Eight, and the test counts
// them so that deleting one is not a silent narrowing.
func TestEveryAccountStateHasItsOwnCell(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range []daemon.AccountState{
		daemon.StateActive, daemon.StateCandidate, daemon.StateExhausted,
		daemon.StateQuarantined, daemon.StateDisabled, daemon.StateUnknown,
	} {
		glyph, text, _ := stateCell(s)
		if text != string(s) {
			t.Errorf("stateCell(%q) text = %q, want the state's own name", s, text)
		}
		key := glyph + " " + text
		if seen[key] {
			t.Errorf("two states render the same cell %q", key)
		}
		seen[key] = true
	}
	if len(seen) != 6 {
		t.Fatalf("six named states rendered %d distinct cells", len(seen))
	}
}

// The mockup's per-row "Best"/"Nearest" name strategies that exist nowhere in
// this tree. The column is a rotation policy and carries two strings.
func TestAutoIsARotationPolicyAndNotAStrategyName(t *testing.T) {
	if got := autoCell(enabledRow()); got != "yes" {
		t.Errorf("autoCell(enabled) = %q, want \"yes\"", got)
	}
	if got := autoCell(disabledRow()); got != "no" {
		t.Errorf("autoCell(disabled) = %q, want \"no\"", got)
	}
}

// Head-preserving with an ASCII suffix, because the head is where the local
// part of an address is and that is what tells two accounts apart. The suffix
// is ".." and not a Unicode ellipsis: this repository emits zero non-ASCII
// bytes and a box-drawing or ellipsis character is a Windows code-page bet
// nobody has made.
func TestALongAddressLosesItsTailAndKeepsItsHead(t *testing.T) {
	got := accountCell("enterprise@co.example.com", 22)
	if len([]rune(got)) != 22 {
		t.Fatalf("accountCell(..., 22) is %d runes wide: %q", len([]rune(got)), got)
	}
	if !strings.HasSuffix(got, "..") || !strings.HasPrefix(got, "enterprise@") {
		t.Fatalf("accountCell = %q, want a kept head and a \"..\" tail", got)
	}
}

// The gauge is 17 columns at full width and 4 when the ladder collapses it.
// Both forms are asserted, because the collapsed one is a different code path
// and is the one no fixture above 43 columns exercises.
func TestTheCollapsedGaugeIsTheBarePercentageAndKeepsTheAbsenceRule(t *testing.T) {
	if got := usedCellCollapsed(rowAtPercent(87)); got != "87%" {
		t.Errorf("usedCellCollapsed(87%%) = %q, want \"87%%\"", got)
	}
	if got := usedCellCollapsed(rowWithNoEntry()); got != view.Unreadable {
		t.Errorf("usedCellCollapsed(unread) = %q, want %q", got, view.Unreadable)
	}
}

// The styles are all their zero value in this task -- lipgloss v2 has no
// auto-adaptive fallback, so the concrete colours are a later commit's call.
// This pins that the plain path emits no escape byte today, so that later
// commit cannot slip one into a fixture without this test turning red.
func TestThePlainPathEmitsNoEscapeByte(t *testing.T) {
	g := newGauge()
	rows := []view.Row{rowAtPercent(0), rowAtPercent(87), rowAtPercent(100), rowWithNoEntry(), enabledRow(), disabledRow()}
	for _, r := range rows {
		for _, cell := range []string{
			usedCell(r, g),
			usedCellCollapsed(r),
			autoCell(r),
			accountCell(r.ListLabel(), 20),
		} {
			if strings.ContainsRune(cell, 0x1b) {
				t.Fatalf("cell %q carries an escape byte on the plain, uncoloured path", cell)
			}
		}
	}
	for _, s := range []daemon.AccountState{
		daemon.StateActive, daemon.StateCandidate, daemon.StateExhausted,
		daemon.StateQuarantined, daemon.StateDisabled, daemon.StateUnknown, "",
	} {
		glyph, text, _ := stateCell(s)
		if strings.ContainsRune(glyph, 0x1b) || strings.ContainsRune(text, 0x1b) {
			t.Fatalf("stateCell(%q) carries an escape byte on the plain, uncoloured path", s)
		}
	}
}

// rowWithNoEntry is the first absence: no cache entry at all, so HasEntry is
// false and Reported never gets as far as looking at Headroom.
func rowWithNoEntry() view.Row {
	return view.Row{}
}

// rowWithUnknownHeadroom is the second absence: there is an entry, but no
// window reported a utilization, so Headroom.Known is false. Reported() checks
// this before ReportedName ever runs.
func rowWithUnknownHeadroom() view.Row {
	return view.Row{
		HasEntry: true,
		Entry:    usage.Entry{Snapshot: &usage.Snapshot{}},
		Headroom: strategy.Headroom{Known: false},
	}
}

// rowWithSilentWindow is a row whose binding window IS present -- Reported()
// finds it in AllWindows() -- but that window reported no utilization, so
// Percent() is the one that says no. usage.Window's tri-state fields are
// unexported, so NewWindow(nil, nil) is the only way to build a present window
// that read nothing, rather than an absent one.
func rowWithSilentWindow(name usage.WindowName) view.Row {
	snap := &usage.Snapshot{}
	setNamedWindow(snap, name, usage.NewWindow(nil, nil))
	return view.Row{
		HasEntry: true,
		Entry:    usage.Entry{Snapshot: snap},
		Headroom: strategy.Headroom{Known: true, Binding: name},
	}
}

// rowAtPercent is a row whose binding window read a real percentage -- the
// case usedCell must draw a bar for, including zero.
func rowAtPercent(pct float64) view.Row {
	return view.Row{
		HasEntry: true,
		Entry:    usage.Entry{Snapshot: &usage.Snapshot{FiveHour: usage.NewWindow(&pct, nil)}},
		Headroom: strategy.Headroom{Known: true, Binding: usage.WindowFiveHour},
	}
}

func enabledRow() view.Row  { return view.Row{Account: store.Account{Disabled: false}} }
func disabledRow() view.Row { return view.Row{Account: store.Account{Disabled: true}} }

// setNamedWindow assigns w to the one of Snapshot's five fixed fields name
// identifies. Snapshot has no map here -- each window is its own field, the
// schema's own shape -- so a generic-by-name test builder needs the switch.
func setNamedWindow(s *usage.Snapshot, name usage.WindowName, w usage.Window) {
	switch name {
	case usage.WindowFiveHour:
		s.FiveHour = w
	case usage.WindowSevenDay:
		s.SevenDay = w
	case usage.WindowSevenDayOAuthApps:
		s.SevenDayOAuthApps = w
	case usage.WindowSevenDayOpus:
		s.SevenDayOpus = w
	case usage.WindowSevenDaySonnet:
		s.SevenDaySonnet = w
	}
}

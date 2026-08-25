// Package tui is the interactive dashboard's rendering layer: the row cells,
// the gauge, the state map, and eventually the whole page.
package tui

import (
	"fmt"
	"time"

	"charm.land/bubbles/v2/progress"
	"charm.land/lipgloss/v2"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/theme"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// The styles this file used to hand out, and the one it still does. stateCell
// answers with a theme.Role now, so five of the six below are reached by
// nothing; only styleHeader is still read, by cellStyle. They are kept rather
// than deleted because deleting them is a decision about COLOUR, and this
// commit swaps the glyph vocabulary and paints nothing -- putting the two in
// one commit is what would make a glyph failure that reproduces on one
// operating system impossible to bisect.
//
// Every one of them is the zero lipgloss.Style, and that is load-bearing rather
// than provisional: the two escape-byte gates in this package assert that a
// page drawn through these emits no SGR byte at all, which is what lets the
// seven golden pages under testdata be compared as bytes. Colour is redundant
// emphasis in this column anyway -- the glyph and the word carry the meaning,
// so a monochrome terminal and NO_COLOR lose nothing -- and lipgloss v2 has no
// auto-adaptive fallback (v1's renderer chose for a light or dark background
// and v2 does not), so a concrete value picked here without a palette would be
// illegible on half the terminals in the world.
var (
	styleActive      = lipgloss.NewStyle()
	styleCandidate   = lipgloss.NewStyle()
	styleExhausted   = lipgloss.NewStyle()
	styleQuarantined = lipgloss.NewStyle()
	styleMuted       = lipgloss.NewStyle()
	styleHeader      = lipgloss.NewStyle()
)

// newGauge is the ten-cell bar. Ten cells, so a full bar is ten characters and
// one character is ten percent -- a scale a reader can count without a legend.
//
// The fill characters come from the glyph set rather than from here, and the
// pair chosen for the Unicode set is a full block against a medium shade
// because both are ambiguous-width: in east-asian mode they measure two columns
// EACH, so a ten-cell bar is twenty columns at every fill level and the cell's
// total does not move with the value. The obvious-looking alternative, a full
// block against a light shade, is worse for exactly that reason and not for the
// reason it looks worse: the shade is width-stable while the block is not, so
// the same bar measures seventeen columns empty and twenty-seven columns full,
// and a column that changes width with the number in it destroys the table
// around it. The page never draws that pair anyway, because a process in
// east-asian mode is handed the ASCII set, but the rule holds for whoever adds
// the third one.
//
// The percentage is printed beside the bar rather than inside it, because
// progress's own percentage would be styled by the library and this one has to
// line up in a fixed-width column.
//
// progress.New defaults FullColor and EmptyColor to its own purple/gray pair
// and paints them unconditionally -- there is no "no colour" Option, only
// exported fields. lipgloss.NoColor{} is the library's own documented way to
// say a role carries no colour, and NoColor is the whole of what this file says
// about colour in this commit: nothing here paints yet. The bar's fill takes a
// role from the palette in the commit that applies it, per row; until then it
// is the terminal's own foreground, which is the one colour a library default
// could never have got right on a terminal nobody has measured.
func newGauge(g Glyphs) progress.Model {
	p := progress.New(
		progress.WithFillCharacters(g.GaugeFull, g.GaugeEmpty),
		progress.WithoutPercentage(),
		progress.WithWidth(10),
	)
	p.FullColor = lipgloss.NoColor{}
	p.EmptyColor = lipgloss.NoColor{}
	return p
}

const unreadable = view.Unreadable

// usedCell mirrors the dashboard's USED column exactly. The gauge is built
// INSIDE the Percent() arm and nowhere else: ViewAs takes a float64 with no
// absence channel, so 0, a negative and a NaN all render as an empty bar, and
// an unread account would be byte-identical to one at zero. That is the bug
// that parked cswap's engine.
//
// Both absences return the bare "?" with no bracket and no bar. An empty
// bracket pair is forbidden -- a bracket implies a reading.
func usedCell(r view.Row, g progress.Model) string {
	bw, ok := r.Reported()
	if !ok {
		return unreadable
	}
	pct, ok := bw.Percent()
	if !ok {
		return unreadable
	}
	return "[" + g.ViewAs(pct/100) + "] " + fmt.Sprintf("%3.0f%%", pct)
}

// usedCellCollapsed is USED at 4 columns instead of 17, for the rung of the
// width ladder below 56 columns. It is the bare percentage and it keeps the
// same two absences: a narrow terminal is not a reason to invent a number.
func usedCellCollapsed(r view.Row) string { return r.UsedLabel() }

// stateCell is one self-describing cell: a glyph, a word, and the ROLE the pair
// is painted in. The glyph is redundant emphasis and the word carries the
// meaning, so the column survives NO_COLOR and a monochrome terminal.
//
// It hands back a role and not a style, and the difference is what makes this
// function testable. A lipgloss.Style carries a []color.Color and a func field
// and has no ==, so a caller can compare two styles only by rendering them; a
// role is an int, and the mapping from a state to its emphasis is therefore a
// value a test pins directly. Building the style is the caller's job, because
// the caller is the one holding the palette.
//
// The default arm is mandatory. AccountState is a string type, so a switch
// without one falls out of every case and leaves the caller holding whatever it
// initialised -- most naturally a zero glyph and the zero Role, which is the
// terminal's own foreground and reads as "nothing unusual here". The document
// format is additive by contract: a newer daemon may publish a state this
// binary has never heard of, and that happens on the day somebody upgrades one
// half of a machine. Carry the value through and render it.
//
// The empty string is its own arm and it is not an error. AccountStatus.State
// is omitempty and is filled from a map lookup that returns the zero value on a
// miss, so an account no daemon has ever published carries "".
//
// The glyph here is deliberately NOT the character the two tables use for the
// live account, and that is a reversal of what this comment used to say. The
// live marker answers "which login would a session get right now", read from
// the credential file; this answers "what did the ranking last decide about
// this account", read from the daemon's own status document. Two documents, two
// questions, and when they disagree that disagreement is the most useful thing
// on the page -- which the old shared "*" hid, because agreement and a
// rendering coincidence looked identical.
func stateCell(g Glyphs, s daemon.AccountState) (glyph, text string, role theme.Role) {
	switch s {
	case daemon.StateActive:
		return g.Active, "active", theme.RoleActive
	case daemon.StateCandidate:
		return g.Candidate, "candidate", theme.RoleCandidate
	case daemon.StateExhausted:
		return g.Exhausted, "exhausted", theme.RoleExhausted
	case daemon.StateEmpty:
		return g.Empty, "empty", theme.RoleExhausted
	case daemon.StateQuarantined:
		return g.Quarantined, "quarantined", theme.RoleQuarantined
	case daemon.StateDisabled:
		return g.Disabled, "disabled", theme.RoleMuted
	case daemon.StateUnknown:
		return g.Unknown, "unknown", theme.RoleMuted
	case "":
		return "", "-", theme.RoleMuted
	}
	return g.Unknown, string(s), theme.RoleMuted
}

// autoCell is whether the engine may rotate to this account. It is a rotation
// policy and not a lock: an explicit `ccdad switch` still activates a disabled
// account.
//
// The mockup showed per-row strategy names here. There is no per-account
// strategy anywhere in this tree -- strategy is one global config key with two
// values -- so the column carries the fact that does exist. The global name is
// in the header line.
func autoCell(r view.Row) string {
	if r.Account.Disabled {
		return "no"
	}
	return "yes"
}

// accountCell truncates a label to width, head-preserving, ending in the page's
// own cut cue. The head is where the local part of an address is, and that is
// what tells two accounts apart -- the tail is the part a reader can afford to
// lose.
//
// The cue arrives as an argument rather than being spelled here, and the reason
// is that there is exactly one cue on the page and it used to be written out in
// four places that could each be changed alone. It is two ASCII characters in
// both glyph sets and it stays that way on purpose: this cut lands on a
// measured column boundary, and the Unicode ellipsis is one column wide in the
// ordinary width mode and two in east-asian mode, so the one string on the page
// whose whole job is to fit would be the one that stopped fitting.
//
// The arithmetic counts RUNES and not display columns, which is what it counted
// before the cue was a parameter. That is correct only while every label and
// every cue here is single-width, and the width tests are what enforce it.
func accountCell(label string, width int, cue string) string {
	r := []rune(label)
	if len(r) <= width {
		return label + spaces(width-len(r))
	}
	c := []rune(cue)
	if width <= len(c) {
		return string(r[:width])
	}
	return string(r[:width-len(c)]) + cue
}

func spaces(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}
	return string(b)
}

// The pass-throughs the page reads. Each is one line over a view.Row method
// (or, for idxCell, the one field the fixtures' IDX column shows), existing
// so the column table has one shape: every cell this package draws is a
// func(view.Row, ...) string, not a mix of methods and functions.
func idxCell(r view.Row) string                   { return fmt.Sprintf("%d", r.Account.Idx) }
func typeCell(r view.Row) string                  { return r.TypeLabel() }
func windowCell(r view.Row) string                { return r.WindowLabel() }
func resetsCell(r view.Row, now time.Time) string { return r.ResetsLabel(now) }
func leftCell(r view.Row) string                  { return r.LeftLabel() }
func tierCell(r view.Row) string                  { return r.TierLabel() }

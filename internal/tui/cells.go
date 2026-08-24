// Package tui is the interactive dashboard's rendering layer: the row cells,
// the gauge, the state map, and eventually the whole page.
package tui

import (
	"fmt"
	"time"

	"charm.land/bubbles/v2/progress"
	"charm.land/lipgloss/v2"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// The six semantic roles the STATE column paints. Colour is redundant emphasis
// here: the glyph and the word carry the meaning, so a monochrome terminal and
// NO_COLOR lose nothing. The concrete values are unset because lipgloss v2 has
// no auto-adaptive fallback -- v1's renderer chose for a light or dark
// background and v2 does not -- and choosing one that is illegible on half the
// terminals in the world is worse than choosing none.
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
// The fill characters are ASCII: this binary emits no non-ASCII byte, and a
// half-block character is a Windows code-page bet nobody here has made. The
// percentage is printed beside the bar rather than inside it, because
// progress's own percentage would be styled by the library and this one has to
// line up in a fixed-width column.
//
// progress.New defaults FullColor and EmptyColor to its own purple/gray pair
// and paints them unconditionally -- there is no "no colour" Option, only
// exported fields. lipgloss.NoColor{} is the library's own documented way to
// say a role carries no colour, which is this file's own open call on the
// STATE styles below.
func newGauge() progress.Model {
	g := progress.New(
		progress.WithFillCharacters('#', '.'),
		progress.WithoutPercentage(),
		progress.WithWidth(10),
	)
	g.FullColor = lipgloss.NoColor{}
	g.EmptyColor = lipgloss.NoColor{}
	return g
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

// stateCell is one self-describing cell: a glyph, a word, and a style. The
// glyph is redundant emphasis and the word carries the meaning, so the column
// survives NO_COLOR and a monochrome terminal.
//
// The default arm is mandatory. AccountState is a string type, so a switch
// without one falls out of every case and leaves the caller holding whatever it
// initialised -- most naturally a zero glyph and a zero style, which reads as
// "active". The document format is additive by contract: a newer daemon may
// publish a state this binary has never heard of, and that happens on the day
// somebody upgrades one half of a machine. Carry the value through and render
// it.
//
// The empty string is its own arm and it is not an error. AccountStatus.State
// is omitempty and is filled from a map lookup that returns the zero value on a
// miss, so an account no daemon has ever published carries "".
//
// The glyph "*" is deliberately the character the two tables already use for
// the active row, and "?" is deliberately the character this binary already
// spells "could not be read". The active MARKER and StateActive are different
// facts from different documents -- the live credentials file and the daemon's
// own status -- and their disagreeing is real information.
func stateCell(s daemon.AccountState) (glyph, text string, style lipgloss.Style) {
	switch s {
	case daemon.StateActive:
		return "*", "active", styleActive
	case daemon.StateCandidate:
		return "+", "candidate", styleCandidate
	case daemon.StateExhausted:
		return "!", "exhausted", styleExhausted
	case daemon.StateEmpty:
		return "0", "empty", styleExhausted
	case daemon.StateQuarantined:
		return "x", "quarantined", styleQuarantined
	case daemon.StateDisabled:
		return "-", "disabled", styleMuted
	case daemon.StateUnknown:
		return "?", "unknown", styleMuted
	case "":
		return "", "-", styleMuted
	}
	return "?", string(s), styleMuted
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

// accountCell truncates a label to width, head-preserving with an ASCII ".."
// suffix. The head is where the local part of an address is, and that is what
// tells two accounts apart -- the tail is the part a reader can afford to
// lose. The suffix is ".." rather than a Unicode ellipsis because this
// repository emits zero non-ASCII bytes and a box-drawing or ellipsis
// character is a Windows code-page bet nobody has made.
func accountCell(label string, width int) string {
	r := []rune(label)
	if len(r) <= width {
		return label + spaces(width-len(r))
	}
	if width <= 2 {
		return string(r[:width])
	}
	return string(r[:width-2]) + ".."
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

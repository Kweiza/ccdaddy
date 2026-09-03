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
// say a role carries no colour, and it is what this constructor leaves them at:
// the terminal's own foreground, which is the one colour a library default
// could never have got right on a terminal nobody has measured.
//
// The page then overwrites both fields on its own COPY of this model, per row,
// from gaugeRole and RoleGaugeEmpty -- a progress.Model is a value, and the
// dashboard holds exactly one of them. That is why the pair is set here at all
// rather than left at the library's default: a row whose palette answers
// NoColor must fall back to a bar this constructor already made colourless,
// not to a purple somebody else chose.
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

// warnBand is how close to its threshold a row has to be before the bar turns
// amber. Ten points, and it is a DISPLAY constant: nothing in this repository
// decides anything by it, no engine reads it, and moving it changes the colour
// of a bar and nothing else.
//
// It is deliberately NOT strategy's hysteresis_pct, which is the number a
// reader reaches for first, and any one of these four objections is enough on
// its own. hysteresis_pct is a PAIRWISE displacement margin -- how far a
// candidate has to beat the ACTIVE account before the engine will move the
// credential -- so it carries no information at all about one row's own
// distance to breach. It is applied in exactly one place in the ranking and
// nowhere near a row. Its defaulting substitutes ten for any value at or below
// zero, so a user who deliberately set it to nothing would get a band they
// never asked for. And under hover the engine does not read it at all -- which
// is precisely the "the table says one thing and the engine does another"
// failure a band on a dashboard exists to avoid.
//
// A named display constant claims only "close to the threshold", claims it in
// its own name, and no engine number can drift away from it.
const warnBand = 10.0

// gaugeRole is the bar's colour.
//
// The emptiness clause is FIRST and it asks an ACCOUNT-level question rather
// than a question about the window the bar happens to be drawing. A blown
// five-hour window can never become a floor -- the floor rule requires a weekly
// window -- so when a weekly binds on slack the reported window is the weekly
// and the five-hour window with nothing left in it is invisible to any test
// asked of the reported window. Reproduced against the tree: five-hour 100%
// used at 95% elapsed, seven-day 40% used at 30% elapsed, the bar reads 40%,
// and the daemon has already filed the account as empty. Painting that green is
// the whole reason this clause runs ahead of the band.
//
// The band then reads the REPORTED window's slack and never the binding
// window's, because the bar's LENGTH came from the reported window. An account
// whose weekly floor is blown draws a full bar off the weekly while its binding
// five-hour window still carries 3.667 points of slack, and a band fed the
// binding number paints a dead account green under a bar that reads 100%.
// Colour and length describe one window or they describe nothing.
//
// An account nobody could read takes RoleMuted rather than a gauge role.
// Unknown is not empty, and the cell it lands in draws no bar at all -- the
// bare question mark, with no bracket, because a bracket implies a reading --
// so the absence is carried by there being nothing to paint, never by a bar.
func gaugeRole(r view.Row) theme.Role {
	if empty, known := r.Empty(); known && empty {
		return theme.RoleGaugeOver
	}
	// The SECOND emptiness question, and it has to be here since 0.10.0.
	//
	// Row.Empty is an account verdict, and it now answers false for an account
	// whose only blown window caps one model family — correctly, because that
	// account can still serve every other model. The bar, though, is drawn from
	// Reported(), and Reported() is that blown cap: the length says 100% while
	// Empty says not empty, and the band below then colours it from the floor's
	// slack. Under hover a threshold is an unclamped PACE TARGET, so a window
	// far enough through its cycle with nothing left in it reports POSITIVE
	// slack — measured on a live four-account fleet: +17, past warnBand, so
	// RoleGaugeOK. A full bar drawn off a week that is gone, painted green.
	//
	// That is the exact failure the clause above exists to prevent, reached
	// through the one door widening the account verdict opened. Colour answers
	// for the window the bar DREW, which is what the paragraph above promises
	// and what this restores.
	if r.ReportedEmpty() {
		return theme.RoleGaugeOver
	}
	slack, _, ok := r.ReportedSlack()
	if !ok {
		return theme.RoleMuted
	}
	switch {
	case slack <= 0:
		return theme.RoleGaugeOver
	case slack <= warnBand:
		return theme.RoleGaugeWarn
	}
	return theme.RoleGaugeOK
}

// stateCell is one self-describing cell: a glyph, a word, and the role that
// colours both. The glyph is redundant emphasis and the word carries the
// meaning, so the column survives NO_COLOR, a monochrome terminal and the None
// theme with nothing lost.
//
// The third return is a theme.Role and not a lipgloss.Style, and that is the
// whole reason this signature changed. This function used to hand back one of
// six empty styles held in a package var block, justified by the claim that
// lipgloss v2 has no auto-adaptive fallback. That claim was wrong. What v2
// removed is the GLOBAL RENDERER: a Style no longer consults anything about the
// terminal on its own, so the background-darkness boolean has to be threaded in
// from the program that asked the terminal for it. Threading it is what the
// palette does, and once a palette exists there is nothing left for an empty
// package var to hold.
//
// So the style is manufactured from a palette at the moment a caller renders.
// This function needs no palette, stays a pure map from a document value to a
// vocabulary, and cannot hand two callers styles built from two different
// themes. It is also what lets a test assert the mapping by value, since
// lipgloss.Style carries a slice field and a func field and therefore has no ==.
//
// The default arm is mandatory. AccountState is a string type, so a switch
// without one falls out of every case and leaves the caller holding whatever it
// initialised -- most naturally a zero glyph and the zero Role, which reads as
// "the terminal's own foreground on an active account". The document format is
// additive by contract: a newer daemon may publish a state this binary has
// never heard of, and that happens on the day somebody upgrades one half of a
// machine. Carry the value through and render it.
//
// The empty string is its own arm and it is not an error. AccountStatus.State
// is omitempty and is filled from a map lookup that returns the zero value on a
// miss, so an account no daemon has ever published carries "".
//
// StateEmpty takes RoleExhausted rather than a role of its own. The two are
// different facts -- one is past the number it was given, the other has nothing
// left -- and the GLYPH is what keeps them apart; a second red would spend a
// role on a distinction the reader is already being shown.
//
// The glyph is deliberately NOT the character the two tables use for the live
// account. The live marker answers "which login would a session get right now",
// read from the credential file; this answers "what did the ranking last decide
// about this account", read from the daemon's own status document. Two
// documents, two questions, and when they disagree that disagreement is the
// most useful thing on the page -- which the old shared "*" hid, because
// agreement and a rendering coincidence looked identical.
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
func windowCell(r view.Row) string                { return r.WindowLabelShort() }
func resetsCell(r view.Row, now time.Time) string { return r.ResetsLabel(now) }
func leftCell(r view.Row) string                  { return r.LeftLabel() }
func tierCell(r view.Row) string                  { return r.TierLabel() }

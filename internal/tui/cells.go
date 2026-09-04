// Package tui is the interactive dashboard's rendering layer: the row cells,
// the gauge, the state map, and eventually the whole page.
package tui

import (
	"fmt"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/theme"
	"github.com/Kweiza/ccdaddy/internal/usage"
	"github.com/Kweiza/ccdaddy/internal/view"
)

const unreadable = view.Unreadable

// cellRole is one window cell's colour, and it is the whole of what replaced
// the gauge.
//
// EMPTY IS ANSWERED FIRST, ahead of the band, and that order is load-bearing:
// under hover a threshold is an unclamped pace target, so a window far enough
// through its own cycle with nothing left in it can be measured against a
// figure above 100. The engine now clamps that at its source, and this asks
// anyway -- a cell must not depend on an invariant held one package away to
// avoid painting a spent window the colour of a healthy one.
func cellRole(r view.Row, n usage.WindowName) theme.Role {
	pct, state := r.WindowPct(n)
	switch state {
	case view.WindowUnreadable, view.WindowAbsent:
		return theme.RoleMuted
	default:
		return theme.UtilizationRole(pct)
	}
}

// worstCell is the whole quota block in one cell, for a terminal too narrow to
// carry the block itself.
//
// It is what the ladder does INSTEAD of dropping window columns, and the reason
// it is safe where dropping is not: every cell reads percentage USED, so the
// worst window is the MAX, and nothing this cell hides is worse than what it
// shows. A partial column set can make no such statement -- the limit it left
// out could be the one that is gone.
//
// It names the window as well as the number. A percentage with no window beside
// it is precisely what this table stopped doing.
func worstCell(r view.Row, cols view.Columns) string {
	worst, header, any := "", "", false
	unread := false
	best := -1.0
	for _, w := range cols.Windows {
		pct, state := r.WindowPct(w.Name)
		switch state {
		case view.WindowUnreadable:
			unread = true
		case view.WindowRead:
			if pct > best {
				best, header, any = pct, w.Header, true
			}
		}
	}
	if !any {
		return view.Unreadable
	}
	worst = fmt.Sprintf("%.0f%% %s", best, header)
	if unread {
		// One window could not be read, so the max is a lower bound. Saying so
		// costs one character and stops the cell claiming more than it knows.
		worst += "+"
	}
	return worst
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
	case daemon.StateServing:
		// The codex proxy's account. It takes the ACTIVE glyph and the active
		// role because it answers the same question for the other provider --
		// "which account would a session started now be billed to" -- and the
		// WORD is what tells the two apart, exactly as it does for empty and
		// exhausted, which share a role for the same kind of reason.
		return g.Active, "serving", theme.RoleActive
	case daemon.StateNeedsRelogin:
		// A dead grant is held out of rotation until a person runs a command,
		// which is what quarantined means on the Claude side, so it is painted
		// the same and the word carries the difference: a quarantine lapses on
		// a timer and this one does not.
		return g.Quarantined, "needs-relogin", theme.RoleQuarantined
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
func idxCell(r view.Row) string  { return fmt.Sprintf("%d", r.Account.Idx) }
func typeCell(r view.Row) string { return r.TypeLabel() }
func tierCell(r view.Row) string { return r.TierLabel() }

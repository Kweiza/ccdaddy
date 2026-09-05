package tui

import (
	"os"
	"strconv"

	"charm.land/lipgloss/v2"
)

// Glyphs is every character this page draws that is not a letter, a digit, or
// part of a value somebody else read. Two sets exist and one of them is chosen
// per process; nothing in this package spells a marker, a frame piece or a
// gauge cell inline any more.
//
// The reason for the type is not tidiness. Before it, the same three questions
// were answered at nine separate sites across five files -- the frame in the
// page renderer, the gauge in the cell builder, the state markers in a switch,
// the cursor in a const, the picker's own cursor in a string literal, the cut
// cue in four more -- and each of those answers was independently reversible. A
// machine that could not draw one of them could not be given a set that avoided
// all of them, because there was no set; there were nine decisions that
// happened to agree.
//
// THE WIDTH RULE, which is what the two sets are really for. The width engine
// this binary measures with reads RUNEWIDTH_EASTASIAN once at startup and, when
// it is on, every box-drawing and block character becomes two columns wide
// while the rest stay one. So the glyphs divide into two classes and they are
// treated differently:
//
//   - A MARKER sits inside a measured column, beside text, in a table whose
//     column widths were computed from the content. It must measure one column
//     in both modes or every column to its right moves. Each marker below was
//     measured in both modes before it was chosen.
//   - The FRAME and the GAUGE may be ambiguous, because a frame is drawn at a
//     width it was told and a gauge is a fixed cell whose total does not change
//     with the percentage in it. That exemption is closed at exactly eight
//     characters -- four corners, two rules, two gauge cells -- and it is safe
//     only because PickGlyphs hands back the ASCII set to any process running
//     with the east-asian mode on.
//
// The cut cue and the two scroll marks are ASCII in BOTH sets. They are emitted
// at a measured column boundary by definition, and the three characters that
// would read better there -- the ellipsis and the two arrows -- are all
// ambiguous, which would have made the exemption above eleven characters wide
// and mode-dependent in the one place a page has already run out of room.
type Glyphs struct {
	// Name is which set this is, "unicode" or "ascii". It exists so a failure
	// message can say which vocabulary produced the page it is complaining
	// about, and so a test can pin the set it asked for rather than inferring
	// it from a character.
	Name string
	// Art is whether this set may draw the pixel chrome, and it is the width
	// rule's THIRD answer rather than a convenience field.
	//
	// The rule above divides this vocabulary in two. A MARKER must measure one
	// column in both width modes, because a table's columns were computed
	// around it. The FRAME and the GAUGE may be ambiguous, because a frame is
	// drawn at a width it was told and a gauge is a fixed cell whose total does
	// not move with the value in it.
	//
	// ART is ambiguous AND its total moves with the mode, which is neither of
	// those, so it gets neither answer. Measured: a 48-cell row of U+2580 is 48
	// columns ordinarily and 96 in east-asian mode. Cutting art in cell space
	// keeps ansi.Truncate off its path, but the frame still measures every
	// content row it is handed, and a 96-column row inside a box asked for 78
	// breaks the box rather than the drawing.
	//
	// So this is false in the east-asian mode even when the caller spelled
	// "unicode", which is the one place the escape hatch is narrowed. The
	// narrowing is exact: the frame, the cursor and the eight markers are still
	// Unicode there, because their widths are still ones the page can predict.
	Art        bool
	Border     lipgloss.Border
	GaugeFull  rune
	GaugeEmpty rune
	Cursor     string
	// Grabbed is the row the move key has picked up: the one the arrow keys
	// are reordering rather than moving between.
	//
	// It is a THIRD marker in that column and not the cursor drawn twice,
	// because move mode changes what the arrow keys do. A reader who cannot
	// tell the two modes apart cannot tell whether the next press walks the
	// list or reorders it, and both look like a cursor moving down one row.
	//
	// U+21C5 measures one column in BOTH width modes, which is the rule every
	// marker in this column is chosen against; the ASCII set takes '=' for the
	// grip, which is the one unused ASCII marker in this vocabulary.
	Grabbed     string
	Active      string
	Candidate   string
	Exhausted   string
	Empty       string
	Quarantined string
	Disabled    string
	Unknown     string
	// Cue is what a line that was cut ends in. Two characters, so that a reader
	// who sees it knows something was removed rather than taking the remainder
	// for the whole value.
	Cue string
	// MoreAbove and MoreBelow say which way the rows a scrolled table is not
	// showing lie. The count alone cannot: the window is a slice, so rows can
	// be off the top, off the bottom, or both, and a bare number reads as "off
	// the bottom" to everyone.
	MoreAbove string
	MoreBelow string
}

// UnicodeGlyphs is the default vocabulary, and the three marker alphabets in it
// are deliberately three and not one.
//
// The live-account MARKER stays "*", spelled by the row itself and not by this
// set. The CURSOR is U+276F. The active STATE glyph is U+25AA. Before this, all
// three were "*" or ">", which made two facts from two different documents --
// which credential a session would actually get, and what the daemon last
// decided about that account -- look like one fact when they agreed and look
// like a rendering bug when they did not. They are different questions from
// different files and their disagreeing is information.
var UnicodeGlyphs = Glyphs{
	Name:        "unicode",
	Art:         true,
	Border:      lipgloss.RoundedBorder(),
	GaugeFull:   '█',
	GaugeEmpty:  '▒',
	Cursor:      "❯",
	Grabbed:     "⇅",
	Active:      "▪",
	Candidate:   "◉",
	Exhausted:   "✗",
	Empty:       "✕",
	Quarantined: "⚠",
	Disabled:    "−",
	Unknown:     "◌",
	Cue:         "..",
	MoreAbove:   "^",
	MoreBelow:   "v",
}

// ASCIIGlyphs is the vocabulary this page had before there was a choice, and it
// is not a degraded mode: it is the correct answer on a console that cannot
// carry UTF-8, and the only correct answer in east-asian width mode, where the
// frame is not merely ugly but arithmetically wrong -- lipgloss sizes the
// vertical border with one width function and the horizontal rules with
// another, and only the second reads the environment variable, so a frame asked
// for twenty columns renders its rules at twenty and its content rows at
// twenty-two. The ASCII frame is exact at twenty in both modes.
//
// The eight markers are the characters the state column already used, kept
// rather than re-invented, so a user who switches the key has a page they can
// still read and a maintainer has one fewer table to keep in step.
var ASCIIGlyphs = Glyphs{
	Name:        "ascii",
	Art:         false,
	Border:      lipgloss.ASCIIBorder(),
	GaugeFull:   '#',
	GaugeEmpty:  '.',
	Cursor:      ">",
	Grabbed:     "=",
	Active:      "*",
	Candidate:   "+",
	Exhausted:   "!",
	Empty:       "0",
	Quarantined: "x",
	Disabled:    "-",
	Unknown:     "?",
	Cue:         "..",
	MoreAbove:   "^",
	MoreBelow:   "v",
}

// PickGlyphs is the whole of the choice: the configured value, and two facts
// about the process it will draw in.
//
// An explicit "unicode" or "ascii" is honoured unconditionally, including
// against a console this binary has just been told cannot carry it. That is
// what an escape hatch is for, and it works in both directions -- somebody
// whose console reports a code page it is lying about needs a way to say so,
// and a value that a probe could veto is not a setting.
//
// "auto" is the default, and anything this function does not recognise takes
// the same arm rather than an error path: the config layer is what validates
// the key, and an Options nobody filled in is a caller who did not choose, not
// a caller who chose wrongly.
//
// The east-asian test is spelled to match the width engine's own predicate
// exactly -- ParseBool, and false unless it parses cleanly to true. That
// matters more than it looks: with a looser test ("is it set at all"), a
// machine with RUNEWIDTH_EASTASIAN=yes would get the ASCII page while the
// engine measuring it stayed in the ordinary mode, so the two would disagree in
// the same direction this whole rule exists to prevent, only quietly and in the
// safe-looking direction.
func PickGlyphs(configured string, utf8Console bool) Glyphs {
	switch configured {
	case "unicode":
		// Honoured, minus the one member of the set whose width the page cannot
		// predict in this mode. See Glyphs.Art for why that is the drawing and
		// not the frame. The copy matters: clearing the field on the package's
		// own value would disable the art for every later caller in the
		// process, including ones that never asked.
		g := UnicodeGlyphs
		g.Art = !eastAsianWidth()
		return g
	case "ascii":
		return ASCIIGlyphs
	}
	if !utf8Console || eastAsianWidth() {
		return ASCIIGlyphs
	}
	return UnicodeGlyphs
}

// eastAsianWidth reports whether the width engine this binary measures with was
// started in its east-asian mode.
//
// It is read here, at call time, and not cached: the value is a fact about the
// process, the process reads it once at startup, and a package var would put a
// third copy of the same answer in the tree for no gain. What it must not
// become is a probe of the terminal -- this asks what THIS PROCESS will measure
// with, which is the question the page's arithmetic depends on, and not what
// the terminal will do with the bytes.
func eastAsianWidth() bool {
	ea, err := strconv.ParseBool(os.Getenv("RUNEWIDTH_EASTASIAN"))
	return err == nil && ea
}

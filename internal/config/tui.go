package config

import (
	"fmt"
	"slices"

	"github.com/Kweiza/ccdaddy/internal/theme"
)

// The display half of config.toml: which palette ccdad's own screens paint
// with, and which glyphs they draw with.
//
// It is the one group in this file that governs nothing the daemon does, and it
// is kept in its own file for the reason hover.go is: a group with an argument
// behind it reads better beside the argument than scattered through the loader.

// defaultTUITheme and defaultTUIGlyphs are both `auto`, and that is an ANSWER
// rather than a deferral.
//
// Every other default here names a number or a mode. These two name a
// MEASUREMENT -- the terminal's background darkness, and the console's code
// page -- taken by whichever command owns a terminal at the moment it draws.
// Naming `dark` instead would be this file guessing at a fact the drawing
// command can simply read, and guessing it identically for every machine.
const (
	defaultTUITheme  = string(theme.Auto)
	defaultTUIGlyphs = glyphsAuto
)

// The glyph set names, spelled HERE rather than imported from the package that
// resolves them.
//
// The theme names come from internal/theme because that package is a leaf. The
// glyph names cannot travel the same way: the picker lives in internal/tui,
// internal/tui reaches internal/daemon, and internal/daemon imports this
// package -- so the import is not one that can be made, it is a cycle and a
// build failure.
//
// What is left is three literals and a pin on them.
// TestTheGlyphSetNamesAreTheThreeLiterals fails if this list is renamed, which
// makes a change on this side deliberate; the risk that remains is a name
// accepted here and unmatched by the picker, and the defence against it is that
// the list is three words long and readable in one line.
const (
	glyphsAuto    = "auto"
	glyphsUnicode = "unicode"
	glyphsASCII   = "ascii"
)

// glyphNames is the accepted set, in the order a refusal lists them: `auto`
// first, because it is the default and the right answer for almost everyone.
func glyphNames() []string {
	return []string{glyphsAuto, glyphsUnicode, glyphsASCII}
}

// themeNames is theme.Names() as plain strings, and it exists for the REFUSAL
// MESSAGE alone. It is deliberately not what decides whether a name is
// accepted: a list of strings on this side that a validator compared against
// would be a second copy of a namespace this package does not own, and the
// first time the two disagreed the config file would accept a word the
// renderer cannot draw -- exactly the drift keys.go exists to prevent, one
// package over.
//
// It is derived per call rather than cached in a package var, because the list
// is five elements long and is consulted once per `ccdad config set`.
func themeNames() []string {
	names := theme.Names()
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = string(n)
	}
	return out
}

// validTheme refuses a palette name this build does not have, and the refusal
// names every one it does. That is parseStrategy's shape and it is here for
// parseStrategy's reason: the message that told a user their word was wrong is
// the only place they find the word that is right.
//
// The ACCEPTANCE goes through theme.Parse and not through a comparison this
// package makes for itself. theme.Of cannot refuse a name -- it answers the
// default palette for anything it does not recognise, which is the right answer
// for an unset key and a catastrophe for a validator -- so Parse is that
// package's own statement of which five spellings exist, and asking it is what
// keeps this function from becoming a sixth opinion on the question.
//
// `auto` is ACCEPTED here and resolved elsewhere. This package must not resolve
// it -- the answer depends on the terminal's background, and the daemon that
// loads this file has no terminal to ask.
func validTheme(v string) error {
	if _, ok := theme.Parse(v); ok {
		return nil
	}
	return fmt.Errorf("unknown theme %q: one of %s", v, joinNames(themeNames()))
}

// validGlyphs refuses a glyph set name this build does not have.
//
// There is deliberately no way to write a fourth. A glyph set is a table of
// literal runes whose display width the renderer has measured, not a preference
// a user can extend from a config file: a name that reached the picker
// unmatched would draw a frame that does not close.
func validGlyphs(v string) error {
	if slices.Contains(glyphNames(), v) {
		return nil
	}
	return fmt.Errorf("unknown glyph set %q: one of %s", v, joinNames(glyphNames()))
}

package theme

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// The three coloured tables. Every value here is a contract, not an
// illustration: each was solved against three gates at once and each is
// re-checked by a test in this package on every run, on every operating
// system CI builds for.
//
// The gates, in the order they bind:
//
//   - Contrast. Text roles reach 7:1 and mark or fill roles reach 4.5:1
//     against every ground in the sets the contrast test carries, because the
//     runtime signal is a boolean and "dark" means any dark ground, not one
//     particular one. The palette is pale for that reason and the paleness was
//     chosen knowingly.
//   - Downsample. The runtime path is a 6x6x6 cube quantisation with a grey
//     tiebreak by HSLuv distance, and that tiebreak takes the hue difference
//     without wrap-around -- so a red just clockwise of 0 degrees scores badly
//     against its own cube cell and well against flat grey, and lands on the
//     grey ramp. Every red here is far enough round the wheel to stay
//     chromatic, and the downsample test proves it through the same functions
//     the writer calls.
//   - Colour vision. The five state roles stay 10 dE00 apart under
//     severity-1.0 protanopia, deuteranopia and tritanopia in both themes.
//     There is no daltonised theme because colour is never the sole carrier
//     here: every distinction also has a glyph and, wherever the column
//     survives, its word.
//
// A hex typed wrong parses to NoColor rather than to a wrong colour, which is
// silent. The role-coverage test is what makes it loud: it asserts by type
// that no role in a coloured theme is NoColor.
var (
	dark = Palette{name: Dark, c: [numRoles]color.Color{
		RoleAccent:      lipgloss.Color("#f09574"),
		RoleActive:      lipgloss.Color("#acfb8f"),
		RoleCandidate:   lipgloss.Color("#6ed5eb"),
		RoleExhausted:   lipgloss.Color("#fdb5aa"),
		RoleQuarantined: lipgloss.Color("#dacb19"),
		RoleMuted:       lipgloss.Color("#d4d4d4"),
		RoleHeader:      lipgloss.Color("#ffffff"),
		RoleNotice:      lipgloss.Color("#fff7bf"),
		RoleGaugeCool:   lipgloss.Color("#7db7ff"),
		RoleGaugeOK:     lipgloss.Color("#3ae0a5"),
		RoleGaugeWarn:   lipgloss.Color("#f3b818"),
		RoleGaugeHigh:   lipgloss.Color("#ffad66"),
		RoleGaugeOver:   lipgloss.Color("#ff7469"),
		RoleGaugeEmpty:  lipgloss.Color("#e4e4e4"),
	}}

	// Light is not an inversion of dark. Inverting a palette solved for a
	// 7:1 floor on black gives colours that clear nothing on white, because
	// the two grounds are not symmetric in relative luminance.
	light = Palette{name: Light, c: [numRoles]color.Color{
		RoleAccent:      lipgloss.Color("#73493d"),
		RoleActive:      lipgloss.Color("#134429"),
		RoleCandidate:   lipgloss.Color("#00237a"),
		RoleExhausted:   lipgloss.Color("#5b0001"),
		RoleQuarantined: lipgloss.Color("#6b410c"),
		RoleMuted:       lipgloss.Color("#4c4c4c"),
		RoleHeader:      lipgloss.Color("#1a1a1a"),
		RoleNotice:      lipgloss.Color("#36310f"),
		RoleGaugeCool:   lipgloss.Color("#003f8c"),
		RoleGaugeOK:     lipgloss.Color("#08724a"),
		RoleGaugeWarn:   lipgloss.Color("#835406"),
		RoleGaugeHigh:   lipgloss.Color("#a64700"),
		RoleGaugeOver:   lipgloss.Color("#7d0e1e"),
		RoleGaugeEmpty:  lipgloss.Color("#000000"),
	}}

	// The sixteen standard slots, symbolic. No hex appears here and none may:
	// the point of this theme is that the terminal resolves it, so a reader
	// who has retuned their own red gets their own red. It is named ansi16
	// rather than ansi because the tests import the library package that
	// spells the conversion functions and one of the two would have to be
	// renamed at the import site otherwise.
	ansi16 = Palette{name: ANSI, c: [numRoles]color.Color{
		RoleAccent:      lipgloss.BrightRed,
		RoleActive:      lipgloss.Green,
		RoleCandidate:   lipgloss.Cyan,
		RoleExhausted:   lipgloss.BrightRed,
		RoleQuarantined: lipgloss.Yellow,
		RoleMuted:       lipgloss.BrightBlack,
		RoleHeader:      lipgloss.White,
		RoleNotice:      lipgloss.Yellow,
		RoleGaugeCool:   lipgloss.BrightBlue,
		RoleGaugeOK:     lipgloss.Green,
		RoleGaugeWarn:   lipgloss.Yellow,
		RoleGaugeHigh:   lipgloss.BrightYellow,
		RoleGaugeOver:   lipgloss.BrightRed,
		RoleGaugeEmpty:  lipgloss.BrightBlack,
	}}
)

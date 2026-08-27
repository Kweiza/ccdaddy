package tui

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/Kweiza/ccdaddy/internal/theme"
)

// The pixel chrome: a drawing stored one character per PIXEL and folded two
// pixel rows into each terminal row.
//
// WHY A FOLD AT ALL. A terminal cell is about twice as tall as it is wide, so a
// drawing that uses one cell per pixel is stretched by a factor of two. Split
// the cell with U+2580 -- foreground paints the top half, background the bottom
// -- and the two halves are square. That is the whole of the technique, and it
// is why the grids below have an even number of rows.
//
// THE VOCABULARY IS TWO RUNES AND A SPACE, and the absence of U+2588 is a
// constraint rather than an oversight. gaugeClass in golden_test.go is
// {U+2588, U+2592} and TestEveryGoldenPageCarriesTheGlyphClassesItsRungAllows
// counts, per fixture, how many LINES carry one of them. Art drawn with U+2588
// would turn a gaugeRows of 3 into 8 on a page drawing only the wordmark and
// into 14 on the full page, and the assertion would stop meaning what it says.
// A cell inked on both halves is U+2580 with the same colour on both channels
// instead, which renders solid and costs one extra SGR parameter.
//
// THE RUNE IS CHOSEN BY INK AND NEVER BY COLOUR. Both halves empty is a space
// whatever the palette says; both halves inked is U+2580 whatever the palette
// says. That is what lets theme.None draw the same bytes as theme.Dark, which
// TestPaintingThePageChangesNoByteOfItsTextAndNoColumnOfItsWidth compares
// across thirteen page sizes -- nine of which draw art. The cost is paid where
// every colour is stripped: a solid region reads as U+2580 on every row, a 50%
// horizontal dither rather than a filled shape. The silhouette survives and the
// fill does not, and that is the cheaper side of the trade against U+2588.
//
// BACKGROUND IS SET ONLY ON INK. A cell with no ink sets neither colour, so the
// terminal's own ground shows through and the page never paints a rectangle
// over itself. That is what lets one authored tone serve both themes: measured,
// the mockup's own dark ground against a light terminal's is 16.7:1, which is
// not a background but a slab.

const (
	// artGround is the character a grid uses for a pixel that is not drawn.
	// Every other character is ink. One tone is all either grid needs -- the
	// mockup's figures are 49% one salmon over ground and the dark "outline"
	// between creatures is the ground showing through -- so the ink character
	// carries no meaning beyond "not ground" and '1' is used by convention.
	artGround = '.'

	// The two runes. U+2580 UPPER HALF BLOCK and U+2584 LOWER HALF BLOCK. Both
	// are EastAsianAmbiguous, which is what Glyphs.Art exists to answer.
	artUpper = '▀'
	artLower = '▄'
)

// artGrid is a drawing. Rows is 2*height pixel rows, top to bottom, each
// exactly W characters of 7-bit ASCII -- so a byte index into a row is a pixel
// index, which is the only reason the fold below can index bytes.
type artGrid struct {
	W    int
	Rows []string
}

// height is the grid's height in TERMINAL rows, which is what the height
// ladder budgets and what render.go loops over.
func (g artGrid) height() int { return len(g.Rows) / 2 }

// artCell is one folded cell: the rune that draws it, whether it takes a
// foreground, and whether it takes a background as well. It is comparable so
// that artRow can coalesce a run of identical cells into one Render.
type artCell struct {
	Rune  rune
	Ink   bool
	Solid bool
}

// foldArt answers the cell at (row, col) of the folded grid. The four cases are
// the whole of the encoding; see TestTheFoldIsTheFourCasesAndNothingElse.
func foldArt(g artGrid, row, col int) artCell {
	top := g.Rows[2*row][col] != artGround
	bottom := g.Rows[2*row+1][col] != artGround
	switch {
	case !top && !bottom:
		return artCell{Rune: ' '}
	case top && !bottom:
		return artCell{Rune: artUpper, Ink: true}
	case !top && bottom:
		return artCell{Rune: artLower, Ink: true}
	default:
		return artCell{Rune: artUpper, Ink: true, Solid: true}
	}
}

// artRow renders one terminal row of a grid, and it is the reason the art is
// safe where the ASCII blocks would not be.
//
// IT DOES NOT CALL truncate, and that is the point. truncate is
// ansi.Truncate, which MEASURES -- and measured on this tree, a 48-cell row of
// U+2580 is 96 columns under RUNEWIDTH_EASTASIAN=1, so that path keeps 39 of
// 48 glyphs at the 80-column pages and 29 of 48 at a 60-column terminal, with
// nothing reporting the loss. A cell count is not a width, so counting cells
// cannot make that mistake.
//
// That does not make the row unmeasured. render.go hands the whole page to a
// frame rendered at a width it was told, and the frame measures every content
// row. Glyphs.Art is what answers THAT, and it is why this function is only
// ever reached through it.
//
// IT STYLES A RUN AT A TIME, for the reason addRole styles a row at a time:
// lipgloss right-pads a multi-line Render out to its widest line, so nothing
// here may hand it more than one line. Runs of identical cells are coalesced
// because a per-cell Render would put a full SGR pair on every one of 48 cells
// for no visible difference.
//
// A row whose role has no colour -- theme.None, NO_COLOR, a redirect -- goes
// through unstyled rather than through a Style whose foreground is NoColor. The
// two are not the same on the wire, and the None theme's contract is zero
// escape bytes. This mirrors Palette.Style, which makes the same distinction
// for every other surface on the page.
func (m Model) artRow(g artGrid, row, inner int, role theme.Role, tail string) string {
	if inner <= 0 {
		return ""
	}
	n := g.W
	if n > inner {
		n = inner
	}

	c := m.Pal.Color(role)
	painted := !isNoColor(c)

	var b strings.Builder
	for i := 0; i < n; {
		cell := foldArt(g, row, i)
		j := i + 1
		for j < n && foldArt(g, row, j) == cell {
			j++
		}
		run := strings.Repeat(string(cell.Rune), j-i)
		if painted && cell.Ink {
			st := lipgloss.NewStyle().Foreground(c)
			if cell.Solid {
				st = st.Background(c)
			}
			b.WriteString(st.Render(run))
		} else {
			b.WriteString(run)
		}
		i = j
	}

	if tail != "" && n < inner {
		b.WriteString(truncate(tail, inner-n))
	}
	return b.String()
}

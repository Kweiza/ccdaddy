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

// wordArt is the "CCDaddy" wordmark, traced and then hand-redrawn from
// docs/tui-mockup.png. 48x10 pixels (48x5 terminal cells folded).
//
// The mockup's wordmark is a thin glowing outline font, and at this canvas a
// straight trace of it disappears: resizing 840x130 source pixels down to
// 48x10 averages every stroke into the ground around it, and even a fraction-
// coverage threshold on a pre-binarized mask produced static, not letters --
// there is no single row of pixels in the source thin enough to survive that
// compression and thick enough to read as ink afterward. So this is drawn
// instead of traced: seven capital letters in a bold seven-segment style
// (open-C, open-C, rectangle-D, A with no foot, rectangle-D twice more, and a
// Y with a descender tail echoing the mockup's own flourish under its "y"),
// spelling CCDADDY, each 5 pixels wide with a 1-pixel gap. The mockup also
// carries a smaller "(ccdad)" alias beside the wordmark; there is no width
// left to draw both legibly at 48 columns, so that alias is dropped here and
// only the primary name is drawn.
var wordArt = artGrid{W: 48, Rows: []string{
	`...11111.11111.11111.11111.11111.11111.1...1....`,
	`...1.....1.....1...1.1...1.1...1.1...1.1...1....`,
	`...1.....1.....1...1.1...1.1...1.1...1.1...1....`,
	`...1.....1.....1...1.1...1.1...1.1...1.1...1....`,
	`...1.....1.....1...1.11111.1...1.1...1.11111....`,
	`...1.....1.....1...1.1...1.1...1.1...1...1......`,
	`...1.....1.....1...1.1...1.1...1.1...1...1......`,
	`...1.....1.....1...1.1...1.1...1.1...1...1......`,
	`...1.....1.....1...1.1...1.1...1.1...1...1......`,
	`...11111.11111.11111.......11111.11111...1......`,
}}

// figureArt is the row of small creatures and the moustached "Daddy" figure
// beneath the wordmark in docs/tui-mockup.png. 48x12 pixels (48x6 terminal
// cells folded).
//
// Traced first (crop (203,328)-(916,621) of the mockup, thresholded against
// its salmon ink at #eb9384), then hand-redrawn twice. The first hand pass
// drew the four creatures as overlapping rectangles sharing one silhouette,
// which read as one fused blob with eye-holes rather than four bodies: three
// of its twelve pixel rows -- including the whole row at the "waist" -- had
// no gap anywhere across the 32-column cluster, so there was nothing for a
// reader's eye to trace between one creature and the next. This is the
// second pass: each creature is its own lane (0-7, 9-16, 18-25, 27-31) and
// columns 8, 17 and 26 are a deliberate seam of ground that no creature's
// rectangle ever reaches, in ANY of the twelve rows -- not a notch cut into a
// shared mass. The back-center creature is tallest and alone at the top; the
// others start lower, and each carries its own punched pair of eyes. A
// 5-column gap separates the cluster from the "Daddy" figure in the last 11
// columns: a wide hat brim over a narrower crown, a head with two eye-slit
// punches, a moustache carved as a gap under the cheeks (so it folds as ink
// on the chin half and ground on the lip half), shoulders, and its own set of
// legs -- unchanged from the first pass, which already read correctly.
var figureArt = artGrid{W: 48, Rows: []string{
	`.........11111111....................11111111111`,
	`.........11111111......................1111111..`,
	`11111111.11.11.11..........11111......11.111.11.`,
	`11111111.11.11.11.11111111.11111......11.111.11.`,
	`11.11.11.11111111.11111111.1.1.1......111111111.`,
	`11.11.11.11111111.11.11.11.1.1.1......111111111.`,
	`11111111.11111111.11.11.11.11111......11.....11.`,
	`11111111.11111111.11111111.11111......111111111.`,
	`11111111.11111111.11111111.11111.....11111111111`,
	`.11..11..11..11....11..11...11.......11111111111`,
	`.11..11..11..11....11..11...11.......11.11.11.11`,
	`.11..11..11..11....11..11...11.......11.11.11.11`,
}}

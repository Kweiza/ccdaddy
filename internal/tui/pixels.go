package tui

import (
	"fmt"
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
		cut := truncate(tail, inner-n)
		if painted {
			cut = lipgloss.NewStyle().Foreground(c).Render(cut)
		}
		b.WriteString(cut)
	}
	return b.String()
}

// wordArt is the "CCDaddy" wordmark, traced and then hand-redrawn from the
// wordmark mockup. 48x10 pixels (48x5 terminal cells folded).
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

// A pixelSprite is one unfurled figure before it is placed in the family. Its
// rows use the same raw-pixel alphabet as artGrid, but a sprite is allowed to
// be shorter than the final grid: the babies are deliberately eight pixels
// tall while Daddy is twelve.
type pixelSprite struct {
	W    int
	Rows []string
}

type familyKind uint8

const (
	babyClaude familyKind = iota
	babyCodex
	daddyClaude
)

type placedFigure struct {
	Kind   familyKind
	Sprite *pixelSprite
	X, Y   int
}

const (
	figureWidth  = 48
	figureHeight = 12
	babyWidth    = 10
	babyOverlap  = 3
)

// Both babies keep the rectangular body, paired eyes, side arms and four short
// legs of the README's original family. Codex changes only the tiny negative-
// space face: three pixels form a chevron and two form its cursor, so it still
// belongs to the same family instead of becoming an antenna-and-screen robot.
var babyClaudeArt = pixelSprite{W: babyWidth, Rows: []string{
	`.11111111.`,
	`.11111111.`,
	`.11.11.11.`,
	`.11.11.11.`,
	`1111111111`,
	`1111111111`,
	`..1.1.1.1.`,
	`..1.1.1.1.`,
}}

var babyCodexArt = pixelSprite{W: babyWidth, Rows: []string{
	`.11111111.`,
	`.11111111.`,
	`.11.11111.`,
	`.111.1111.`,
	`111.1..111`,
	`1111111111`,
	`..1.1.1.1.`,
	`..1.1.1.1.`,
}}

// Daddy is the original figure's proportions rather than a baby with a hat:
// a wider crown and brim, a twelve-pixel body, inset eyes and moustache, broad
// shoulders and four separated legs.
var daddyClaudeArt = pixelSprite{W: 14, Rows: []string{
	`...11111111...`,
	`...11111111...`,
	`11111111111111`,
	`.111111111111.`,
	`.11..1111..11.`,
	`.11..1111..11.`,
	`.111..11..111.`,
	`.1111....1111.`,
	`11111111111111`,
	`.111111111111.`,
	`..11.11.11.11.`,
	`..11.11.11.11.`,
}}

// familyFigures is in paint order, left to right. Every baby is ten pixels
// wide and advances seven, so each adjacent pair overlaps by exactly three
// pixels -- 30 percent -- while the whole huddle stays clear of Daddy at x=34.
// The shallow vertical stagger echoes the README huddle's uneven top line.
// Keeping the occlusion direction consistent also leaves every baby readable;
// putting both neighbours in front of a ten-pixel middle baby would expose only
// four columns of it at this resolution.
var familyFigures = []placedFigure{
	{Kind: babyClaude, Sprite: &babyClaudeArt, X: 0, Y: 3},
	{Kind: babyCodex, Sprite: &babyCodexArt, X: 7, Y: 2},
	{Kind: babyClaude, Sprite: &babyClaudeArt, X: 14, Y: 3},
	{Kind: babyCodex, Sprite: &babyCodexArt, X: 21, Y: 2},
	{Kind: daddyClaude, Sprite: &daddyClaudeArt, X: 34, Y: 0},
}

// composeFigureArt treats every sprite as an opaque raw-pixel rectangle. A
// foreground figure never clears outside its own horizontal span, so a ten-
// pixel baby placed seven pixels after its neighbour covers exactly three
// pixels. Where footprints overlap, the foreground edge becomes a one-pixel
// ground contour; the rows immediately above and below its footprint are
// cleared only within that span so a covered foot cannot survive as a detached
// fragment.
func composeFigureArt(w, h int, figures []placedFigure) (artGrid, error) {
	if w <= 0 || h <= 0 || h%2 != 0 {
		return artGrid{}, fmt.Errorf("figure canvas must have positive width and positive even height, got %dx%d", w, h)
	}

	canvas := make([][]byte, h)
	for y := range canvas {
		canvas[y] = []byte(strings.Repeat(string(artGround), w))
	}
	for i, figure := range figures {
		if figure.Sprite == nil {
			return artGrid{}, fmt.Errorf("figure %d has no sprite", i)
		}
		sprite := figure.Sprite
		if sprite.W <= 0 || len(sprite.Rows) == 0 {
			return artGrid{}, fmt.Errorf("figure %d has an empty %dx%d sprite", i, sprite.W, len(sprite.Rows))
		}
		if figure.X < 0 || figure.Y < 0 || figure.X+sprite.W > w || figure.Y+len(sprite.Rows) > h {
			return artGrid{}, fmt.Errorf("figure %d at (%d,%d) with size %dx%d exceeds %dx%d canvas",
				i, figure.X, figure.Y, sprite.W, len(sprite.Rows), w, h)
		}
		for sy, row := range sprite.Rows {
			if len(row) != sprite.W {
				return artGrid{}, fmt.Errorf("figure %d row %d is %d pixels wide, want %d", i, sy, len(row), sprite.W)
			}
			for sx := 0; sx < len(row); sx++ {
				if row[sx] != artGround && row[sx] != '1' {
					return artGrid{}, fmt.Errorf("figure %d row %d column %d is %q, want %q or '1'", i, sy, sx, row[sx], artGround)
				}
			}
		}

		contourLeft, contourRight := false, false
		for j := 0; j < i; j++ {
			prior := figures[j]
			if prior.Sprite == nil {
				continue // The earlier validation would already have returned.
			}
			horizontal := figure.X < prior.X+prior.Sprite.W && prior.X < figure.X+sprite.W
			vertical := figure.Y < prior.Y+len(prior.Sprite.Rows) && prior.Y < figure.Y+len(sprite.Rows)
			if !horizontal || !vertical {
				continue
			}
			switch {
			case prior.X < figure.X:
				contourLeft = true
			case prior.X > figure.X:
				contourRight = true
			}
		}

		// The opaque rectangle removes covered pixels. Its one-row vertical
		// caps remove fragments caused by the family's shallow Y stagger, but
		// remain inside the X footprint so the overlap does not grow past 30%.
		top := max(0, figure.Y-1)
		bottom := min(h, figure.Y+len(sprite.Rows)+1)
		for y := top; y < bottom; y++ {
			for x := figure.X; x < figure.X+sprite.W; x++ {
				canvas[y][x] = artGround
			}
		}
		for sy, row := range figure.Sprite.Rows {
			copy(canvas[figure.Y+sy][figure.X:figure.X+figure.Sprite.W], row)
		}
		for y := figure.Y; y < figure.Y+len(sprite.Rows); y++ {
			if contourLeft {
				canvas[y][figure.X] = artGround
			}
			if contourRight {
				canvas[y][figure.X+sprite.W-1] = artGround
			}
		}
	}

	rows := make([]string, h)
	for y := range canvas {
		rows[y] = string(canvas[y])
	}
	return artGrid{W: w, Rows: rows}, nil
}

func mustComposeFigureArt(w, h int, figures []placedFigure) artGrid {
	g, err := composeFigureArt(w, h, figures)
	if err != nil {
		panic(err)
	}
	return g
}

// figureArt remains 48x12 pixels (48x6 terminal cells folded), so improving
// the family changes no rung of the dashboard's height ladder.
var figureArt = mustComposeFigureArt(figureWidth, figureHeight, familyFigures)

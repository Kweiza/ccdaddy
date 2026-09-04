package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/theme"
)

// probeGrid is four cells wide and one terminal row tall, and its four columns
// are the fold's four cases in order: neither half inked, the top only, the
// bottom only, both. Every assertion about the fold reads this fixture, so the
// cases are visible in the source rather than inferred from a drawing.
var probeGrid = artGrid{W: 4, Rows: []string{
	`.11.`,
	`..11`,
}}

// The fold is a TABLE and not a heuristic, and each row of it is the reason a
// different invariant elsewhere survives.
//
// A cell with neither half inked is a space with no style, so the terminal's
// own ground shows through and the page paints no rectangle -- which is what
// lets one authored tone serve the light theme as well as the dark.
//
// A cell with both halves inked is U+2580 carrying the SAME colour on the
// foreground and the background, which renders solid. U+2588 would be the
// obvious rune and it is forbidden: gaugeClass is {U+2588, U+2592} and
// TestEveryGoldenPageCarriesTheGlyphClassesItsRungAllows counts the lines
// carrying it per fixture, so art drawn with it turns a gaugeRows of 3 into 8
// where only the wordmark is drawn and into 14 on the full page.
//
// The rune is chosen by INK and never by colour. That is what keeps theme.None
// drawing the same bytes as theme.Dark, which is the whole premise of
// TestPaintingThePageChangesNoByteOfItsTextAndNoColumnOfItsWidth -- and it is
// measured rather than assumed: nine of that test's thirteen cases draw the
// wordmark.
func TestTheFoldIsTheFourCasesAndNothingElse(t *testing.T) {
	for _, tc := range []struct {
		name string
		col  int
		want artCell
	}{
		{"neither half is inked", 0, artCell{Rune: ' '}},
		{"the top half only", 1, artCell{Rune: artUpper, Ink: true}},
		{"both halves", 2, artCell{Rune: artUpper, Ink: true, Solid: true}},
		{"the bottom half only", 3, artCell{Rune: artLower, Ink: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := foldArt(probeGrid, 0, tc.col); got != tc.want {
				t.Errorf("column %d folds to %+v, want %+v", tc.col, got, tc.want)
			}
		})
	}
}

// A row is cut in CELL space, and the cut is the whole reason artRow exists
// beside addRole rather than inside it.
//
// addRole truncates with ansi.Truncate, which measures. Measured on this tree:
// a 48-cell row of U+2580 is 96 columns under RUNEWIDTH_EASTASIAN=1, so that
// path keeps 39 of 48 glyphs at the 80-column pages' inner of 78 and 29 of 48
// at a 60-column terminal's 58. Counting cells cannot do that, because a cell
// count is not a width.
func TestAnArtRowIsCutInCellsAndNeverByTheWidthEngine(t *testing.T) {
	m := fixtureModel(113, 26)
	for _, tc := range []struct {
		name  string
		inner int
		want  string
	}{
		{"wider than the grid", 10, " " + string(artUpper) + string(artUpper) + string(artLower)},
		{"exactly the grid", 4, " " + string(artUpper) + string(artUpper) + string(artLower)},
		{"narrower than the grid", 2, " " + string(artUpper)},
		{"one cell", 1, " "},
		{"no room at all", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := m.artRow(probeGrid, 0, tc.inner, theme.RoleAccent, "")
			if got != tc.want {
				t.Errorf("artRow at inner %d is %q, want %q", tc.inner, got, tc.want)
			}
		})
	}
}

// The tail is how the version string rides on the wordmark's own last row
// without being appended to it.
//
// Today render.go concatenates titleLine onto wordmark's last string. A pixel
// row cannot carry appended text, but a row being assembled in cell space can:
// the art's cells end and the tail's begin. The rung accounting in layout.go is
// unchanged because the observable shape is.
func TestTheTailRidesAfterTheArtAndIsCutToWhatIsLeft(t *testing.T) {
	m := fixtureModel(113, 26)
	art := " " + string(artUpper) + string(artUpper) + string(artLower)
	for _, tc := range []struct {
		name  string
		inner int
		tail  string
		want  string
	}{
		{"room for all of it", 12, "ccdad v1", art + "ccdad v1"},
		{"room for some of it", 7, "ccdad v1", art + "ccd"},
		{"no room left", 4, "ccdad v1", art},
		{"no tail asked for", 12, "", art},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := m.artRow(probeGrid, 0, tc.inner, theme.RoleAccent, tc.tail); got != tc.want {
				t.Errorf("artRow with tail %q at inner %d is %q, want %q", tc.tail, tc.inner, got, tc.want)
			}
		})
	}
}

// Painting the art changes its escapes and never its runes, and this is the
// local statement of a property the whole page depends on.
//
// It is worth a test of its own even though colour_test asserts it across
// thirteen pages: a failure there reports a whole rendered page and a failure
// here reports one row.
func TestPaintingAnArtRowChangesNoRuneOfIt(t *testing.T) {
	plain := fixtureModel(113, 26).artRow(probeGrid, 0, 10, theme.RoleAccent, "v1")
	dark := darkModel(113, 26).artRow(probeGrid, 0, 10, theme.RoleAccent, "v1")

	if strings.ContainsRune(plain, 0x1b) {
		t.Errorf("the None palette painted an art row: %q", plain)
	}
	if !strings.ContainsRune(dark, 0x1b) {
		t.Fatalf("the Dark palette painted nothing, so the comparison below proves nothing: %q", dark)
	}
	if got := ansi.Strip(dark); got != plain {
		t.Errorf("painting the row changed its text:\n painted+stripped: %q\n plain:            %q", got, plain)
	}
}

// A solid cell carries the colour on BOTH channels, which is what makes it read
// as solid without U+2588.
func TestASolidCellIsPaintedOnBothChannels(t *testing.T) {
	solid := artGrid{W: 1, Rows: []string{`1`, `1`}}
	half := artGrid{W: 1, Rows: []string{`1`, `.`}}
	empty := artGrid{W: 1, Rows: []string{`.`, `.`}}

	got := darkModel(113, 26).artRow(solid, 0, 1, theme.RoleAccent, "")
	if !strings.Contains(got, "48;2;") {
		t.Errorf("a cell inked on both halves set no background, so it will not read as solid: %q", got)
	}
	if got := darkModel(113, 26).artRow(half, 0, 1, theme.RoleAccent, ""); strings.Contains(got, "48;2;") {
		t.Errorf("a cell inked on one half painted the ground behind it: %q", got)
	}
	if got := darkModel(113, 26).artRow(empty, 0, 1, theme.RoleAccent, ""); strings.Contains(got, "38;2;") {
		t.Errorf("an empty cell painted a foreground colour when it should not: %q", got)
	}
}

// The two baby masks are the visual approval boundary. A page golden cannot
// hold this by itself: under the unpainted theme a fully inked cell and a cell
// with only its top half inked both print U+2580, so changing one raw pixel can
// leave every golden byte-identical. These literals preserve the README
// family's block bodies while keeping Codex's distinction to a five-pixel
// face cue instead of the old antenna-and-screen silhouette.
func TestTheBabySpritesMatchTheirApprovedClaudeAndCodexSilhouettes(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  pixelSprite
		want []string
	}{
		{"baby Claude", babyClaudeArt, []string{
			`.11111111.`,
			`.11111111.`,
			`.11.11.11.`,
			`.11.11.11.`,
			`1111111111`,
			`1111111111`,
			`..1.1.1.1.`,
			`..1.1.1.1.`,
		}},
		{"baby Codex", babyCodexArt, []string{
			`.11111111.`,
			`.11111111.`,
			`.11.11111.`,
			`.111.1111.`,
			`111.1..111`,
			`1111111111`,
			`..1.1.1.1.`,
			`..1.1.1.1.`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got.W != babyWidth {
				t.Errorf("sprite width = %d, want %d", tc.got.W, babyWidth)
			}
			if got, want := strings.Join(tc.got.Rows, "\n"), strings.Join(tc.want, "\n"); got != want {
				t.Errorf("sprite changed:\n got:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

type inkBounds struct {
	left, top, right, bottom int
}

func boundsOf(s pixelSprite) inkBounds {
	b := inkBounds{left: s.W, top: len(s.Rows), right: -1, bottom: -1}
	for y, row := range s.Rows {
		for x := 0; x < len(row); x++ {
			if row[x] == artGround {
				continue
			}
			if x < b.left {
				b.left = x
			}
			if x > b.right {
				b.right = x
			}
			if y < b.top {
				b.top = y
			}
			if y > b.bottom {
				b.bottom = y
			}
		}
	}
	return b
}

func (b inkBounds) width() int  { return b.right - b.left + 1 }
func (b inkBounds) height() int { return b.bottom - b.top + 1 }

// Size and overlap are properties of the component figures, not of the final
// union mask. Keeping the placements is what makes both claims measurable:
// four anonymous silhouettes baked into figureArt could be separated, fused,
// or adult-sized with no reliable way to recover their intended boundaries.
func TestTheBabiesAreSmallerThanDaddyAndOverlapByThirtyPercent(t *testing.T) {
	var babies []placedFigure
	var daddy *placedFigure
	claudes, codices := 0, 0
	for i := range familyFigures {
		figure := &familyFigures[i]
		switch figure.Kind {
		case babyClaude:
			claudes++
			babies = append(babies, *figure)
		case babyCodex:
			codices++
			babies = append(babies, *figure)
		case daddyClaude:
			daddy = figure
		}
	}
	if len(babies) != 4 || claudes != 2 || codices != 2 {
		t.Fatalf("family has %d babies (%d Claude, %d Codex), want four (two and two)", len(babies), claudes, codices)
	}
	if daddy == nil {
		t.Fatal("family has no Daddy Claude")
	}

	daddyBounds := boundsOf(*daddy.Sprite)
	for i, baby := range babies {
		b := boundsOf(*baby.Sprite)
		if b.left != 0 || b.right != baby.Sprite.W-1 || b.top != 0 || b.bottom != len(baby.Sprite.Rows)-1 {
			t.Errorf("baby %d has padded bounds %+v inside %dx%d", i, b, baby.Sprite.W, len(baby.Sprite.Rows))
		}
		if b.width() >= daddyBounds.width() || b.height() >= daddyBounds.height() {
			t.Errorf("baby %d is %dx%d, want both dimensions below Daddy's %dx%d",
				i, b.width(), b.height(), daddyBounds.width(), daddyBounds.height())
		}
	}

	for i := 0; i+1 < len(babies); i++ {
		left, right := babies[i], babies[i+1]
		lb, rb := boundsOf(*left.Sprite), boundsOf(*right.Sprite)
		leftEdge := left.X + lb.left
		leftEnd := left.X + lb.right
		rightEdge := right.X + rb.left
		overlap := leftEnd - rightEdge + 1
		if rightEdge <= leftEdge {
			t.Fatalf("babies are not ordered left to right at pair %d: %d then %d", i, leftEdge, rightEdge)
		}
		if overlap != babyOverlap || 10*overlap != 3*lb.width() || 10*overlap != 3*rb.width() {
			t.Errorf("babies %d and %d overlap by %d pixels across widths %d and %d; want %d pixels, exactly 30%%",
				i, i+1, overlap, lb.width(), rb.width(), babyOverlap)
		}
	}
}

// The raw composition is an approval boundary in addition to the page golden.
// Folding two raw rows into one terminal row can hide a one-pixel regression,
// and a union-only assertion cannot distinguish four children from one fused
// shape. This pins the source-like faces, the overlap contours and Daddy's
// two-lobed moustache before folding loses that information.
func TestTheComposedFamilyMatchesItsApprovedRawSilhouette(t *testing.T) {
	want := []string{
		`.....................................11111111...`,
		`.....................................11111111...`,
		`........111111........11111111....11111111111111`,
		`.111111.111111.111111.11111111.....111111111111.`,
		`.111111.11.111.111111.11.11111.....11..1111..11.`,
		`.11.11..111.11.11.11..111.1111.....11..1111..11.`,
		`.11.11..11.1...11.11..11.1..111....111..11..111.`,
		`1111111.111111.111111.111111111....1111....1111.`,
		`1111111..1.1.1.111111..1.1.1.1....11111111111111`,
		`..1.1.1..1.1.1..1.1.1..1.1.1.1.....111111111111.`,
		`..1.1.1.........1.1.1...............11.11.11.11.`,
		`....................................11.11.11.11.`,
	}
	if got := strings.Join(figureArt.Rows, "\n"); got != strings.Join(want, "\n") {
		t.Errorf("composed family changed:\n got:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// Each visible baby must remain one useful silhouette after occlusion. Four
// components rule out fused bodies; the size floor rules out the detached
// one- and two-pixel feet which a rectangular overwrite used to leave behind.
func TestEveryComposedBabyIsSeparateAndHasNoDetachedFragments(t *testing.T) {
	const babyClusterWidth = 31
	seen := make([][]bool, len(figureArt.Rows))
	for y := range seen {
		seen[y] = make([]bool, babyClusterWidth)
	}

	type point struct{ x, y int }
	var sizes []int
	for y, row := range figureArt.Rows {
		for x := 0; x < babyClusterWidth; x++ {
			if row[x] != '1' || seen[y][x] {
				continue
			}
			queue := []point{{x: x, y: y}}
			seen[y][x] = true
			size := 0
			for len(queue) > 0 {
				p := queue[0]
				queue = queue[1:]
				size++
				for _, d := range []point{{x: -1}, {x: 1}, {y: -1}, {y: 1}} {
					nx, ny := p.x+d.x, p.y+d.y
					if nx < 0 || nx >= babyClusterWidth || ny < 0 || ny >= len(figureArt.Rows) || seen[ny][nx] {
						continue
					}
					if figureArt.Rows[ny][nx] == '1' {
						seen[ny][nx] = true
						queue = append(queue, point{x: nx, y: ny})
					}
				}
			}
			sizes = append(sizes, size)
		}
	}

	if len(sizes) != 4 {
		t.Fatalf("baby cluster has %d connected ink silhouettes with sizes %v, want four", len(sizes), sizes)
	}
	for i, size := range sizes {
		if size < 30 {
			t.Errorf("baby silhouette %d is only %d pixels, likely a detached fragment", i, size)
		}
	}
}

func TestTheFigureComposerCutsAContinuousGroundContourAtAnOverlap(t *testing.T) {
	block := pixelSprite{W: 3, Rows: []string{`111`, `111`}}
	g, err := composeFigureArt(5, 2, []placedFigure{
		{Sprite: &block, X: 0, Y: 0},
		{Sprite: &block, X: 2, Y: 0},
	})
	if err != nil {
		t.Fatalf("compose a valid overlap: %v", err)
	}
	for y, row := range g.Rows {
		if row != `11.11` {
			t.Errorf("row %d = %q, want a ground pixel between the two bodies", y, row)
		}
	}
}

func TestTheFigureComposerRejectsMalformedSpritesAndPlacements(t *testing.T) {
	good := pixelSprite{W: 2, Rows: []string{`11`, `11`}}
	empty := pixelSprite{}
	short := pixelSprite{W: 2, Rows: []string{`1`, `11`}}
	badPixel := pixelSprite{W: 2, Rows: []string{`1x`, `11`}}

	for _, tc := range []struct {
		name        string
		w, h        int
		figures     []placedFigure
		wantInError string
	}{
		{"odd canvas height", 4, 3, nil, "positive even height"},
		{"nil sprite", 4, 2, []placedFigure{{}}, "no sprite"},
		{"empty sprite", 4, 2, []placedFigure{{Sprite: &empty}}, "empty"},
		{"negative placement", 4, 2, []placedFigure{{Sprite: &good, X: -1}}, "exceeds"},
		{"overflowing placement", 4, 2, []placedFigure{{Sprite: &good, X: 3}}, "exceeds"},
		{"short row", 4, 2, []placedFigure{{Sprite: &short}}, "row 0"},
		{"unknown pixel", 4, 2, []placedFigure{{Sprite: &badPixel}}, "column 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := composeFigureArt(tc.w, tc.h, tc.figures)
			if err == nil || !strings.Contains(err.Error(), tc.wantInError) {
				t.Fatalf("error = %v, want one containing %q", err, tc.wantInError)
			}
		})
	}
}

// The grids are hand-maintained data, and every one of these is a way a hand
// edit goes wrong silently.
//
// An odd row count folds the last row against a row that is not there and
// panics at render time, on one page size, in front of a user. A short row
// indexes out of range the same way. A stray character is worse than either
// because it does not panic: it becomes ink, and a drawing acquires a pixel
// nobody put there.
func TestTheGridsAreWellFormed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		w          int
		rows       []string
		foldedRows int
	}{
		{"the wordmark", wordArt.W, wordArt.Rows, 5},
		{"the composed figures", figureArt.W, figureArt.Rows, 6},
		{"baby Claude", babyClaudeArt.W, babyClaudeArt.Rows, 4},
		{"baby Codex", babyCodexArt.W, babyCodexArt.Rows, 4},
		{"Daddy Claude", daddyClaudeArt.W, daddyClaudeArt.Rows, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.rows)%2 != 0 {
				t.Fatalf("the grid has %d pixel rows, which is odd: the fold takes them in pairs", len(tc.rows))
			}
			if got := len(tc.rows) / 2; got != tc.foldedRows {
				t.Errorf("the grid is %d terminal rows, want %d", got, tc.foldedRows)
			}
			for i, row := range tc.rows {
				if len(row) != tc.w {
					t.Errorf("pixel row %d is %d characters, want %d: %q", i, len(row), tc.w, row)
				}
				for j := 0; j < len(row); j++ {
					if row[j] != artGround && row[j] != '1' {
						t.Errorf("pixel row %d column %d is %q, want %q or '1'", i, j, row[j], artGround)
					}
				}
			}
		})
	}
}

// A drawing with no ink is a bug that every other test in this file would call
// correct, because a grid of pure ground folds to a row of spaces and satisfies
// the fold table, the cut, the tail and the strip-equality assertion at once.
//
// The half-inked cell is named separately and it is not decoration: it is the
// premise of the wordmark's case in TestEachRoleLandsOnTheCellItNames, which
// looks for the accent's opening sequence immediately followed by U+2580. A
// solid cell opens with a foreground AND a background, so that assertion only
// has something to find if some cell somewhere is inked on the top half alone.
func TestEachGridDrawsInkAndAtLeastOneHalfInkedCell(t *testing.T) {
	for _, tc := range []struct {
		name string
		g    artGrid
	}{
		{"the wordmark", wordArt},
		{"the figures", figureArt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ink, halfTop := 0, 0
			for r := 0; r < tc.g.height(); r++ {
				for c := 0; c < tc.g.W; c++ {
					switch cell := foldArt(tc.g, r, c); {
					case cell == artCell{Rune: artUpper, Ink: true}:
						halfTop++
						ink++
					case cell.Ink:
						ink++
					}
				}
			}
			if ink == 0 {
				t.Fatalf("the grid draws nothing at all")
			}
			if halfTop == 0 {
				t.Errorf("no cell is inked on the top half alone, so TestEachRoleLandsOnTheCellItNames has nothing to match")
			}
			t.Logf("%d inked cells of %d, %d of them inked on the top half alone", ink, tc.g.height()*tc.g.W, halfTop)
		})
	}
}

// The drawing is the same width in both width modes, measured in CELLS.
//
// A child process is the only way to ask this, and glyphs_test.go says why: the
// width engine reads RUNEWIDTH_EASTASIAN once in its own package init, into two
// unexported values nothing exports a setter for. t.Setenv would change an
// environment nobody reads again, and the naive version of this test measures
// the narrow mode twice.
//
// The assertion is on the CELL count and not on ansi.StringWidth, deliberately.
// StringWidth is what says 96 in that mode; the claim here is the narrower one
// that the renderer emits the same runes either way, which is what makes
// Glyphs.Art the only thing standing between the art and a broken frame.
func TestAnArtRowIsTheSameCellsInBothWidthModes(t *testing.T) {
	m := fixtureModel(113, 26)
	for _, tc := range []struct {
		name string
		g    artGrid
	}{
		{"the wordmark", wordArt},
		{"the figures", figureArt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for r := 0; r < tc.g.height(); r++ {
				row := m.artRow(tc.g, r, 111, theme.RoleAccent, "")
				if got := len([]rune(row)); got != tc.g.W {
					t.Errorf("row %d is %d runes, want %d", r, got, tc.g.W)
				}
			}
		})
	}
	alsoInEastAsianMode(t)
}

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

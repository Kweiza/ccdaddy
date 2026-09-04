package tui

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The seven pages, by the size that produced each. The size is in the name
// because it is the first thing a reader needs and the only thing that makes
// two of them different.
const (
	goldenFullPage     = "full-page-113x26.txt"
	goldenDesignTarget = "design-target-80x24.txt"
	goldenShort        = "short-80x13.txt"
	goldenNarrow       = "narrow-56x10.txt"
	goldenCollapsed    = "collapsed-43x9.txt"
	goldenNotice       = "notice-80x21.txt"
	goldenZeroAccounts = "zero-accounts-80x13.txt"
)

// update rewrites the pages under testdata from what the renderer produced.
//
// Regeneration had no mechanism before this: the seven pages were raw string
// literals in a test file, column-aligned by hand, and the only way to change
// one was to print the page and paste it back between two backticks. That is
// not a procedure, it is a dare -- and the change that motivated this one
// touches all seven at once.
//
//	go test ./internal/tui -update -count=1
//
// The flag WRITES WHATEVER THE RENDERER SAID, including a page that is wrong.
// It is a transcription tool and never an oracle, and the discipline that makes
// it safe is reading the diff it leaves: a regeneration is reviewed like any
// other change to a file, because that is what it is.
var update = flag.Bool("update", false, "rewrite the golden pages under testdata from what the renderer produced")

// wroteGolden is what -update has already put in each file during this run.
//
// Two tests render the same page and compare it against the same file -- the
// ladder test and the one that proves an unclaimed forecast moves no golden --
// and without this the second would silently overwrite the first. Then a
// renderer that drew those two cases differently would leave a green suite and
// one file holding whichever page happened to be written last, which is the
// exact failure the second test exists to catch.
var wroteGolden = map[string]string{}

// checkGolden compares a rendered page against the file that holds it.
//
// The page is stripped of escape sequences before the comparison, and on the
// seven pages that reach here the strip still removes nothing -- but the reason
// has moved, and the difference is the whole of what a reader needs to know.
// It is no longer that this package emits no escape byte: it emits them on five
// screens now, which TestTheNoneThemeEmitsNoEscapeBytesAndTheDarkThemeDoes
// asserts in both directions. What is true is narrower and is a fact about the
// FIXTURE: every page compared here is built by fixtureModel, fixtureModel
// carries the None palette, and under None every role answers NoColor and
// Palette.Style hands back a style with no foreground set, so there is nothing
// for ansi.Strip to take off.
//
// The strip stays, and it is not dead weight. It is what stops that narrow fact
// from being load-bearing: a fixture rebuilt under any other palette would pin
// the exact truecolor spelling of every role into testdata, and a palette
// change nobody meant to review would then arrive as seven unreadable diffs.
// Stripping is what lets the goldens go on answering the question they were
// written for -- where does every character sit -- and leaves which role was
// painted on which cell to the tests that ask that directly.
func checkGolden(t *testing.T, name, page string) {
	t.Helper()
	got := ansi.Strip(page)
	want := goldenWant(t, name, got)
	if got != want {
		t.Fatalf("%s:\ngot:\n%s\nwant:\n%s", name, got, want)
	}
}

// readGolden is the page on disk, for the one test that builds its expectation
// out of another page rather than out of a render.
func readGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading the golden page: %v", err)
	}
	return strings.TrimSuffix(string(b), "\n")
}

// goldenWant is the file's contents, or -- under -update -- the page that is
// about to become them.
//
// The trailing newline is added on write and taken off on read so the files are
// ordinary text a terminal can cat without eating the last line. Nothing else
// about the bytes is touched: several of these pages end their lines in
// SIGNIFICANT trailing spaces, because a page with no frame pads its rows to
// the terminal width, and an editor or a hook that trims whitespace on save
// will redden this suite. The line endings are the repository's own, held to LF
// on every platform by the top-level gitattributes rule, which is what keeps a
// Windows checkout comparing the same bytes a Linux one does.
func goldenWant(t *testing.T, name, got string) string {
	t.Helper()
	if !*update {
		return readGolden(t, name)
	}
	if before, seen := wroteGolden[name]; seen && before != got {
		t.Fatalf("two renders of %s disagree, so -update would keep only the last:\nfirst:\n%s\nsecond:\n%s",
			name, before, got)
	}
	wroteGolden[name] = got
	if err := os.WriteFile(filepath.Join("testdata", name), []byte(got+"\n"), 0o644); err != nil {
		t.Fatalf("rewriting the golden page: %v", err)
	}
	return got
}

// The seven pages, checked against the LADDER instead of against the renderer
// that drew them.
//
// This is the assertion that closes the hole -update leaves open. Six of these
// files are transcriptions of what the renderer produced, so checkGolden on its
// own is a renderer agreeing with itself: regenerate under a bug and the bug is
// written to disk and blessed, with nothing between that and a release but a
// human reading a diff. Every number below is read off the two ladders in
// layout.go -- which rung drops the border, which drops the STATE column, which
// collapses the gauge -- and off the fixture pool's own shape, so a golden that
// recorded the wrong page fails here even while matching the renderer perfectly.
//
// WHAT EACH NUMBER IS, and not one of them is an observation:
//
//   - FRAME is all-or-nothing and never a count in between. saveBorder is a
//     single rung of the height ladder, and a page that kept its border draws a
//     rule on every one of its rows: the two edge rows from the corners and the
//     horizontal rule, every content row from the two vertical ones. 56x10 and
//     43x9 are below that rung; the four 80- and 113-column pages are above it.
//   - GAUGE is three rows on every page that draws the bar, because the fixture
//     pool is four accounts of which exactly three have a reading. The fourth
//     is unreadable and renders as a bare "?" with no bracket and no bar, which
//     is usedCell's absence rule and not a rounding. 43x9 sits below collapseAt
//     where USED is the bare percentage, and the zero-accounts page has no
//     account row to draw one on.
//   - STATE is four rows wherever col(ColState) survived the WIDTH ladder, one per
//     account. 56 and 43 columns have both dropped that column by then; the
//     zero-accounts page keeps the heading and has nothing under it.
//   - WIDEST is exactly the width and never merely within it. footer pads
//     itself to the full width at every rung, framed or not, so a page whose
//     widest line came out short is a page that lost a column somewhere -- a
//     failure an upper bound alone would pass.
//   - LINES is within the height the page was planned for, which is the whole
//     of what the height ladder promises.
//   - ASCII says the entire file is 7-bit, and only the 43x9 page is. That rung
//     has dropped the frame, collapsed the gauge and dropped the STATE column;
//     the chrome above it is dropped by the ladder at this height, so there is
//     no art vocabulary at stake either; the cut cue and the two scroll
//     marks are ASCII in both sets; and no page here claims a forecast, so the
//     one line that would carry a computed value's own U+00B7 is never drawn.
//     It is a stronger claim than the three counts above precisely because it
//     catches a character from a class nobody thought to enumerate.
//
// The classes are spelled from UnicodeGlyphs rather than as literals, and that
// does not weaken anything: WHICH characters the set holds is pinned by value
// in glyphs_test.go, and what is asserted here is WHERE they are allowed to
// appear. Two tests, two claims, neither restating the other. Reading them out
// of ASCIIGlyphs instead would be meaningless rather than merely different --
// its frame is "+-|" and its markers are "*+!0x-?", every one of which the
// wordmark and the page's own prose already contain, so every row of every page
// would count as framed. That is one more reason the fixtures name their glyph
// set explicitly instead of detecting it.
func TestEveryGoldenPageCarriesTheGlyphClassesItsRungAllows(t *testing.T) {
	for _, tc := range []struct {
		file          string
		width, height int
		frame         bool
		gaugeRows     int
		stateRows     int
		sevenBitASCII bool
	}{
		// gaugeRows is 0 on every page now. The gauge is retired: it was
		// seventeen columns of ONE window, and which window was the derivation
		// this table stopped making -- three windows would be fifty-one columns
		// of bar, and one bar would be the derived window back under a new
		// name. The row of percentages is the gauge, read across, and it draws
		// no glyphs at all.
		//
		// narrow-56x10 is now entirely 7-bit ASCII for the same reason and for
		// no other: it carries no state markers at its rung, and the gauge was
		// the only other non-ASCII thing on it.
		{goldenFullPage, 113, 26, true, 0, 4, false},
		{goldenDesignTarget, 80, 24, true, 0, 4, false},
		{goldenShort, 80, 13, true, 0, 4, false},
		{goldenNotice, 80, 21, true, 0, 4, false},
		{goldenZeroAccounts, 80, 13, true, 0, 0, false},
		{goldenNarrow, 56, 10, false, 0, 0, true},
		{goldenCollapsed, 43, 9, false, 0, 0, true},
	} {
		t.Run(tc.file, func(t *testing.T) {
			lines := strings.Split(readGolden(t, tc.file), "\n")
			if len(lines) > tc.height {
				t.Errorf("%s is %d lines, %d more than the %d-row terminal it was planned for",
					tc.file, len(lines), len(lines)-tc.height, tc.height)
			}
			widest := 0
			for i, line := range lines {
				got := ansi.StringWidth(line)
				if got > tc.width {
					t.Errorf("%s line %d is %d columns, want at most %d: %q", tc.file, i, got, tc.width, line)
				}
				if got > widest {
					widest = got
				}
			}
			if widest != tc.width {
				t.Errorf("%s is %d columns at its widest, want exactly %d: the footer pads to the full width at every rung",
					tc.file, widest, tc.width)
			}
			wantFrame := 0
			if tc.frame {
				wantFrame = len(lines)
			}
			if got := linesCarrying(lines, frameClass(UnicodeGlyphs)); got != wantFrame {
				t.Errorf("%s draws a frame character on %d of its %d lines, want %d", tc.file, got, len(lines), wantFrame)
			}
			if got := linesCarrying(lines, gaugeClass(UnicodeGlyphs)); got != tc.gaugeRows {
				t.Errorf("%s draws a gauge cell on %d lines, want %d", tc.file, got, tc.gaugeRows)
			}
			if got := linesCarrying(lines, stateClass(UnicodeGlyphs)); got != tc.stateRows {
				t.Errorf("%s draws a state marker on %d lines, want %d", tc.file, got, tc.stateRows)
			}
			if got := sevenBitASCII(lines); got != tc.sevenBitASCII {
				t.Errorf("%s is entirely 7-bit ASCII: %v, want %v", tc.file, got, tc.sevenBitASCII)
			}
		})
	}
}

// linesCarrying counts LINES holding at least one rune of a class, not runes.
//
// A line is the unit because every claim above is about which ROWS of the page
// a class is allowed on: three rows have a gauge, four have a state marker,
// every row or no row has a frame. Counting runes would answer a different
// question -- how many cells a ten-wide bar filled -- which moves with the
// percentages in the fixture pool and would make this table a transcription of
// the renderer again, which is the one thing it exists not to be.
func linesCarrying(lines []string, class map[rune]bool) int {
	n := 0
	for _, line := range lines {
		for _, r := range line {
			if class[r] {
				n++
				break
			}
		}
	}
	return n
}

// frameClass is the eight border runes a bordered Style actually draws. The
// Middle fields are left out for the reason drawnRunes leaves them out: the
// only bordered thing on this page is the outer frame, and the account table is
// built with an empty Border and every side switched off.
func frameClass(g Glyphs) map[rune]bool {
	return runeSet(
		g.Border.Top, g.Border.Bottom, g.Border.Left, g.Border.Right,
		g.Border.TopLeft, g.Border.TopRight, g.Border.BottomLeft, g.Border.BottomRight,
	)
}

// gaugeClass is the bar's two fill cells.
func gaugeClass(g Glyphs) map[rune]bool {
	return runeSet(string(g.GaugeFull), string(g.GaugeEmpty))
}

// stateClass is the seven markers the STATE column can draw, and the cursor is
// deliberately not among them. Every page here is rendered with the cursor at
// row zero, which is the fixture pool's live account, and the live marker wins
// that column -- so the cursor glyph appears on none of these pages and adding
// it would be adding a column of zeroes. Where it would matter is the 43x9
// page, and the 7-bit assertion there catches it along with everything else.
func stateClass(g Glyphs) map[rune]bool {
	return runeSet(g.Active, g.Candidate, g.Exhausted, g.Empty, g.Quarantined, g.Disabled, g.Unknown)
}

func runeSet(ss ...string) map[rune]bool {
	out := map[rune]bool{}
	for _, s := range ss {
		for _, r := range s {
			out[r] = true
		}
	}
	return out
}

// sevenBitASCII reports whether every BYTE of the page is below 0x80. It is
// spelled over bytes and not over runes because that is the claim: a rune loop
// would decode U+2588 into one value below no threshold anything measures, and
// the question here is whether a byte a legacy console cannot render is present
// at all.
func sevenBitASCII(lines []string) bool {
	for _, line := range lines {
		for i := 0; i < len(line); i++ {
			if line[i] > 0x7f {
				return false
			}
		}
	}
	return true
}

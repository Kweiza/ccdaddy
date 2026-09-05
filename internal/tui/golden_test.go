package tui

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The eight pages, by the size that produced each. The size is in the name
// because it is the first thing a reader needs and the only thing that makes
// two of them different.
//
// The notice page moved from 80x21 to 80x23 and the trailer page is new. Both
// are the same fact about the same commit: the table grew two section headings
// and the fleet grew a fifth account and a second Active line, so a page that
// used to have room for a notice at 21 rows does not, and a page that draws the
// trailer needs more height than any fixture here had. A fixture whose size no
// longer produces the rung it is named for pins the absence and calls it the
// presence, so the size in the name moves with the rung.
//
// 23 and not 22, and the reason for it is no longer the one it was. The trailer
// is given up ahead of the notice at every height now -- its rung sits above the
// family art and the note's below it -- so the two never trade places and 22 is
// simply where the note itself gives. What 23 buys is that the fixture is not
// standing ON its own rung: a page pinned one row above the height where its
// block disappears is one grown block away from recording the absence and
// calling it the presence. It costs nothing, because the ladder has decided
// every block by 22 and does not move again until 29, where the trailer comes
// back: the same 22 lines are drawn at every height in between.
const (
	goldenFullPage     = "full-page-113x26.txt"
	goldenTrailer      = "trailer-113x34.txt"
	goldenDesignTarget = "design-target-80x24.txt"
	goldenShort        = "short-80x13.txt"
	goldenNarrow       = "narrow-56x10.txt"
	goldenCollapsed    = "collapsed-43x9.txt"
	goldenNotice       = "notice-80x23.txt"
	goldenZeroAccounts = "zero-accounts-80x13.txt"
)

// update rewrites the pages under testdata from what the renderer produced.
//
// Regeneration had no mechanism before this: the pages were raw string
// literals in a test file, column-aligned by hand, and the only way to change
// one was to print the page and paste it back between two backticks. That is
// not a procedure, it is a dare -- and the change that motivated this one
// touched all of them at once.
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
// eight pages that reach here the strip still removes nothing -- but the reason
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
// change nobody meant to review would then arrive as eight unreadable diffs.
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

// The eight pages, checked against the LADDER instead of against the renderer
// that drew them.
//
// This is the assertion that closes the hole -update leaves open. Seven of
// these files are transcriptions of what the renderer produced, so checkGolden
// on its own is a renderer agreeing with itself: regenerate under a bug and the
// bug is written to disk and blessed, with nothing between that and a release
// but a human reading a diff. Every number below is read off the two ladders in
// layout.go -- which rung drops the border, which drops the STATE column, which
// collapses the gauge -- and off the fixture pool's own shape, so a golden that
// recorded the wrong page fails here even while matching the renderer perfectly.
//
// WHAT EACH NUMBER IS, and not one of them is an observation:
//
//   - FRAME is all-or-nothing and never a count in between. saveBorder is a
//     single rung of the height ladder, and a page that kept its border draws a
//     rule on every one of its rows: the two edge rows from the corners and the
//     horizontal rule, every content row from the two vertical ones. THREE
//     pages are below that rung. 56x10 and 43x9 always were; 80x13 joined them
//     when the table grew two section headings and a fifth account and the
//     summary above it grew a second Active line, which is four rows the ladder
//     had to find at a height that had none to spare.
//
//     80x13 does NOT get its frame back now that the headings have a rung of
//     their own, and the arithmetic says why rather than the page. With every
//     block on, that page needs 34 rows for this fleet. The tagline, the blank
//     separators, the trailer and the family art take it to 21, the wordmark to
//     17 -- and the frame's rung is reached at 17 against a terminal of 13, so
//     the frame goes and 15 is still two too many. The headings' rung is the
//     next one down and it lands the page on exactly 13. So the two rows the
//     headings hand back are spent on the title line and the summary block, one
//     rung further down again, and not on the frame that had already gone. The
//     113s, 80x24, the notice page and the empty store stay above the rung.
//
//   - GAUGE is 0 everywhere. The gauge is retired: it was seventeen columns of
//     ONE window, and which window was the derivation this table stopped
//     making. The row of percentages is the gauge, read across, and it draws no
//     glyph from either fill cell.
//
//   - STATE is one row per ACCOUNT ROW DRAWN, wherever view.ColumnState
//     survived the WIDTH ladder -- five now, not four, because the fixture pool
//     gained a codex seat whose serving state takes the active marker.
//
//     "Drawn" is the word that has to be checked rather than assumed, and the
//     section headings are why: they are table rows, so a page whose budget the
//     headings ate would show four accounts and a count, and would report four
//     here while looking entirely reasonable. It has not happened on any of
//     these five, and NONE of the eight scrolls. The clamp is the table's own
//     length -- rows plus the headings on a page that kept them, rows alone on
//     one that gave them up -- so it is 7 on the four pages that draw headings
//     and 5 on the three that do not, and each has at least that much budget:
//     113x34 has 31 before the clamp, 113x26 has 24, 80x24 has 21, the notice
//     page has 20, and 80x13 has 10 against a table of 5. 56x10 has 7 and 43x9
//     lands on exactly 5, which is the whole fleet -- 43x9 was the one page here
//     that scrolled, and it stopped when the headings' rung handed it back the
//     two rows that were pushing two accounts off. Both have dropped STATE at
//     their width anyway, so neither can report a marker either way.
//
//   - WIDTH is an upper bound. Framed pages fill it by construction; frameless
//     pages need not manufacture trailing spaces after the old combined
//     summary line was split into short, independent facts.
//
//   - LINES is within the height the page was planned for, which is the whole
//     of what the height ladder promises.
//
//   - ASCII says the entire file is 7-bit, and the two narrowest pages are.
//     Both rungs have dropped the frame and the STATE column, both are below
//     the height at which any chrome survives so there is no art vocabulary at
//     stake, and no page here claims a forecast, so the one line that would
//     carry a computed value's own U+00B7 is never drawn. Neither scrolls, so
//     neither draws a scroll mark -- and if one did the claim would survive it,
//     because MoreAbove and MoreBelow are "^" and "v" in BOTH glyph sets, the
//     same reason the cut cue that DOES appear on both of these pages is "..".
//     It is a stronger claim than the counts above precisely because it catches
//     a character from a class nobody thought to enumerate.
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
		// The five rows carrying a state marker are the five accounts, and the
		// fifth is the codex seat: StateServing takes the ACTIVE glyph, which
		// stateClass holds, so it counts exactly as the other four do. The
		// section headings themselves carry none -- every cell of a heading row
		// but ACCOUNT is empty, STATE included.
		{goldenTrailer, 113, 34, true, 0, 5, false},
		{goldenFullPage, 113, 26, true, 0, 5, false},
		{goldenDesignTarget, 80, 24, true, 0, 5, false},
		{goldenNotice, 80, 23, true, 0, 5, false},
		// 80x13 has lost its frame and its section headings, and has kept the
		// title line and every fact in the summary. Its five state markers are
		// what makes it non-ASCII on their own now, where before the frame was
		// carrying that claim as well.
		{goldenShort, 80, 13, false, 0, 5, false},
		// The zero-accounts page keeps every column heading and has nothing
		// under them but the two section headings and one sentence about the
		// store, none of which is an account. It is the same 13 rows as the page
		// above and keeps its frame and its headings where that one gives both
		// up, which is the fleet and not the height: four account rows fewer is
		// four rows the ladder never has to find.
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
			for i, line := range lines {
				got := ansi.StringWidth(line)
				if got > tc.width {
					t.Errorf("%s line %d is %d columns, want at most %d: %q", tc.file, i, got, tc.width, line)
				}
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

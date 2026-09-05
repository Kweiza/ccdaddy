package tui

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// eastAsianChild is how a re-exec'd copy of this test binary knows it is the
// child, so it runs the assertions and does not spawn a third process.
const eastAsianChild = "CCDAD_TEST_EAST_ASIAN_CHILD"

// alsoInEastAsianMode runs the calling test again, in a fresh process, with the
// width engine put in its east-asian mode.
//
// A subprocess is the only way to ask this question and that is a property of
// the library, not a preference here. x/ansi reads RUNEWIDTH_EASTASIAN exactly
// once, inside its own package init, into two unexported condition values that
// nothing exports a setter for. By the time any test function runs, the engine
// has already been chosen for the life of the process, so t.Setenv changes an
// environment nobody will read again: the naive version of this test passes
// while measuring the narrow mode twice.
//
// The child runs the same test by name, which is what keeps the two modes
// measured by the same assertions rather than by two lists that drift apart.
func alsoInEastAsianMode(t *testing.T) {
	t.Helper()
	if os.Getenv(eastAsianChild) == "1" {
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	cmd.Env = append(os.Environ(), eastAsianChild+"=1", "RUNEWIDTH_EASTASIAN=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s under RUNEWIDTH_EASTASIAN=1: %v\n%s", t.Name(), err, out)
	}
	if !strings.Contains(string(out), "PASS") {
		t.Fatalf("%s under RUNEWIDTH_EASTASIAN=1 produced no PASS line, so it may not have run:\n%s", t.Name(), out)
	}
}

// The glyphs are pinned as BYTES and never as the characters they render to,
// and the difference is not pedantry. U+26A0 alone measures one column in both
// width modes; U+26A0 followed by U+FE0F -- which is what a paste from a
// browser or an emoji picker produces, and which looks identical in an editor
// -- measures two on the grapheme path this package's width function takes and
// one on the wc path, so the two engines in this dependency stack disagree
// about a string that reads as one character. A source file that acquired the
// selector by accident would move a column on some terminals and no assertion
// about widths could see it, because both spellings measure one somewhere.
func TestTheUnicodeGlyphsAreTheExactCodepointsAndCarryNoVariationSelector(t *testing.T) {
	for _, tc := range []struct{ name, got, want string }{
		{"cursor", UnicodeGlyphs.Cursor, "\u276f"},
		{"active", UnicodeGlyphs.Active, "\u25aa"},
		{"candidate", UnicodeGlyphs.Candidate, "\u25c9"},
		{"exhausted", UnicodeGlyphs.Exhausted, "\u2717"},
		{"empty", UnicodeGlyphs.Empty, "\u2715"},
		{"quarantined", UnicodeGlyphs.Quarantined, "\u26a0"},
		{"disabled", UnicodeGlyphs.Disabled, "\u2212"},
		{"unknown", UnicodeGlyphs.Unknown, "\u25cc"},
	} {
		if tc.got != tc.want {
			t.Errorf("the unicode %s glyph is %q (% x), want %q (% x)", tc.name, tc.got, tc.got, tc.want, tc.want)
		}
	}
	if UnicodeGlyphs.GaugeFull != '\u2588' || UnicodeGlyphs.GaugeEmpty != '\u2592' {
		t.Errorf("the unicode gauge is %q/%q, want U+2588/U+2592", UnicodeGlyphs.GaugeFull, UnicodeGlyphs.GaugeEmpty)
	}
	// The cue and the two scroll marks are ASCII in BOTH sets, and this is the
	// assertion that keeps them there. They are emitted at a measured column
	// boundary by definition -- a cue is what a cut line ends in -- and the
	// three characters that would read better there, U+2026, U+2191 and U+2193,
	// all measure two columns in east-asian mode.
	for _, set := range []Glyphs{UnicodeGlyphs, ASCIIGlyphs} {
		for _, tc := range []struct{ name, s string }{
			{"cue", set.Cue}, {"more-above", set.MoreAbove}, {"more-below", set.MoreBelow},
		} {
			for _, b := range []byte(tc.s) {
				if b > 0x7f {
					t.Errorf("%s's %s is %q, which is not 7-bit ASCII", set.Name, tc.name, tc.s)
				}
			}
		}
		if set.Cue != ".." || set.MoreAbove != "^" || set.MoreBelow != "v" {
			t.Errorf("%s carries cue=%q above=%q below=%q, want \"..\" \"^\" \"v\" in both sets",
				set.Name, set.Cue, set.MoreAbove, set.MoreBelow)
		}
	}
}

// Every glyph that sits inside a measured column beside text measures one
// column in BOTH width modes. That is the whole rule the marker vocabulary was
// chosen against: a marker whose width flips with an environment variable moves
// every column to the right of it on exactly the machines nobody here is
// looking at.
func TestEveryMarkerGlyphMeasuresOneColumnInBothWidthModes(t *testing.T) {
	for _, set := range []Glyphs{UnicodeGlyphs, ASCIIGlyphs} {
		for _, m := range markerGlyphs(set) {
			if got := ansi.StringWidth(m.glyph); got != 1 {
				t.Errorf("%s's %s glyph %q measures %d columns under RUNEWIDTH_EASTASIAN=%q, want 1",
					set.Name, m.name, m.glyph, got, os.Getenv("RUNEWIDTH_EASTASIAN"))
			}
		}
	}
	alsoInEastAsianMode(t)
}

// The ambiguous set is CLOSED for the fixed VOCABULARY this test walks --
// drawnRunes' markers, cue, gauge and border, nothing more -- and this is what
// closes it. Six frame glyphs and two gauge glyphs in that vocabulary are
// allowed to measure two columns in east-asian mode; every other character in
// it must measure one in both. The frame and the gauge get the exemption
// because a frame is drawn at a known width and a gauge is a fixed-width cell
// whose total does not move with the value -- and because the auto glyph set
// falls back to ASCII in that mode anyway, which is what makes the exemption
// safe rather than merely declared.
//
// Nothing joins this list. A ninth entry in that vocabulary means a page whose
// width arithmetic is right on one machine and two columns out on another,
// with no error anywhere.
//
// The pixel ART is not part of this vocabulary and this test never sees it:
// drawnRunes does not walk the art grids, only the markers, the cue, the gauge
// and the border. The art is separately ambiguous-width BY DESIGN -- it draws
// U+2580 and U+2584 on purpose -- and nothing here bounds that. What bounds it
// instead is Glyphs.Art being false whenever the width engine is in east-asian
// mode, which is what TestAnExplicitUnicodeKeepsItsGlyphsAndLosesOnlyTheArtInEastAsianMode
// and TestAnExplicitUnicodeGlyphSetStillFallsBackToTheTypedBlocksInEastAsianMode
// verify.
func TestTheAmbiguousGlyphSetIsExactlyTheFrameAndTheGauge(t *testing.T) {
	ambiguous := map[rune]bool{
		'\u256d': true, '\u256e': true, '\u2570': true, '\u256f': true, // the four rounded corners
		'\u2500': true, '\u2502': true, // the horizontal and vertical rules
		'\u2588': true, '\u2592': true, // the gauge's full and empty cells
	}
	wide := os.Getenv(eastAsianChild) == "1"
	for _, set := range []Glyphs{UnicodeGlyphs, ASCIIGlyphs} {
		for _, r := range drawnRunes(set) {
			want := 1
			if wide && ambiguous[r] {
				want = 2
			}
			if got := ansi.StringWidth(string(r)); got != want {
				t.Errorf("%s draws %q (U+%04X) at %d columns, want %d (east-asian mode: %v)",
					set.Name, r, r, got, want, wide)
			}
		}
	}
	alsoInEastAsianMode(t)
}

// markerGlyphs is every glyph that is drawn INSIDE a measured column, which is
// the population the width rule is about.
func markerGlyphs(g Glyphs) []struct{ name, glyph string } {
	return []struct{ name, glyph string }{
		{"cursor", g.Cursor}, {"grabbed", g.Grabbed}, {"active", g.Active}, {"candidate", g.Candidate},
		{"exhausted", g.Exhausted}, {"empty", g.Empty}, {"quarantined", g.Quarantined},
		{"disabled", g.Disabled}, {"unknown", g.Unknown},
		{"more-above", g.MoreAbove}, {"more-below", g.MoreBelow},
	}
}

// drawnRunes is every rune a set can put on the page: the markers, the gauge,
// the cue, and the EIGHT border fields a bordered Style actually draws.
//
// The Middle fields of lipgloss.Border are deliberately not walked, and their
// absence is a fact about this renderer rather than an oversight. RoundedBorder
// carries U+252C, U+2534, U+251C, U+2524 and U+253C in them, none of which the
// closed set above accepts -- and none of which is ever drawn here, because the
// only bordered thing on the page is the outer frame, and the account table is
// built with an empty Border and every side switched off.
func drawnRunes(g Glyphs) []rune {
	var out []rune
	for _, s := range markerGlyphs(g) {
		out = append(out, []rune(s.glyph)...)
	}
	out = append(out, []rune(g.Cue)...)
	out = append(out, g.GaugeFull, g.GaugeEmpty)
	for _, s := range []string{
		g.Border.Top, g.Border.Bottom, g.Border.Left, g.Border.Right,
		g.Border.TopLeft, g.Border.TopRight, g.Border.BottomLeft, g.Border.BottomRight,
	} {
		out = append(out, []rune(s)...)
	}
	return out
}

// PickGlyphs' own environment read is the one place in this package where
// t.Setenv is both legal and sufficient, and the contrast with the two tests
// above is worth stating: those measure an engine chosen in package init, this
// one reads the variable at call time. A future rewrite that hoisted this read
// into an init or a package var would make this test pass while measuring
// nothing, exactly the way the naive width test does.
func TestGlyphsAutoFallsBackToAsciiWhenTheWidthEngineIsInEastAsianMode(t *testing.T) {
	for _, tc := range []struct {
		name        string
		configured  string
		env         string
		utf8Console bool
		want        string
	}{
		{"auto on a utf-8 console in the ordinary width mode", "auto", "", true, "unicode"},
		{"auto with the east-asian mode on", "auto", "1", true, "ascii"},
		{"auto with the east-asian mode spelled true", "auto", "true", true, "ascii"},
		{"auto with a value the width engine itself ignores", "auto", "yes", true, "unicode"},
		{"auto with the variable explicitly off", "auto", "0", true, "unicode"},
		{"auto on a console that cannot carry utf-8", "auto", "", false, "ascii"},
		{"an unset field is the same decision auto is", "", "", true, "unicode"},
		{"unicode is asked for and honoured on a console that cannot carry it", "unicode", "", false, "unicode"},
		{"unicode is asked for and honoured in east-asian mode", "unicode", "1", true, "unicode"},
		{"ascii is the escape hatch and always wins", "ascii", "", true, "ascii"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RUNEWIDTH_EASTASIAN", tc.env)
			if got := PickGlyphs(tc.configured, tc.utf8Console); got.Name != tc.want {
				t.Fatalf("PickGlyphs(%q, %v) with RUNEWIDTH_EASTASIAN=%q chose %q, want %q",
					tc.configured, tc.utf8Console, tc.env, got.Name, tc.want)
			}
		})
	}
}

// Every line of a bordered page measures the width the page was asked for, in a
// process whose width engine is in east-asian mode.
//
// READ WHAT THIS ASSERTS. It is not that the rounded frame measures correctly
// in that mode -- it does not, and cannot be made to from here. lipgloss sizes
// the vertical border with a width function that never reads the environment
// variable and draws the horizontal rules with one that does, so a frame asked
// for twenty columns renders its rules at twenty and its content rows at
// twenty-two, and its own GetHorizontalFrameSize answers two where the page
// spends four. This test asserts that the FALLBACK happened: that a page whose
// glyph set was left on auto in that mode was drawn with the ASCII frame, where
// every constant in the width ladder is already right.
//
// The negative control below is half the test. Without it, a fallback that had
// silently stopped being necessary and a fallback that had silently stopped
// happening would look identical from here.
func TestABorderedPageIsExactlyItsWidthInEastAsianMode(t *testing.T) {
	if os.Getenv(eastAsianChild) == "1" {
		// Through glyphsFor, and deliberately not through PickGlyphs directly.
		// A direct call proves the arm works and proves nothing at all about
		// whether anything reaches it. glyphsFor is the only path Render and
		// newApp take to a glyph set, so a glyphsFor that resolved "auto" for
		// itself -- or that pinned the console bit to a constant -- would leave
		// this arm green, correct, and unreachable in the shipping binary.
		g := glyphsFor(Options{GlyphSet: "auto", ConsoleUTF8: true})
		if g.Name != "ascii" {
			t.Fatalf("auto chose the %q set in east-asian width mode, where its frame is two columns wrong", g.Name)
		}
		m := fixtureModelGlyphs(80, 24, g)
		for i, line := range strings.Split(m.Body(), "\n") {
			if got := ansi.StringWidth(line); got != m.Width {
				t.Errorf("line %d of the fallback page is %d columns, want %d: %q", i, got, m.Width, line)
			}
		}
		var widest int
		for _, line := range strings.Split(fixtureModelGlyphs(80, 24, UnicodeGlyphs).Body(), "\n") {
			if got := ansi.StringWidth(line); got > widest {
				widest = got
			}
		}
		if widest <= 80 {
			t.Errorf("the unicode page measures %d columns in east-asian mode, so the frame no longer disagrees with itself and the auto fallback can be revisited", widest)
		}
		return
	}
	alsoInEastAsianMode(t)
}

// The escape hatch is honoured in the direction it was built for, and in no
// other. `glyphs = "unicode"` is a sentence somebody typed, so the frame, the
// cursor and the eight markers stay Unicode even in a process whose width
// engine is in its east-asian mode -- that is what PickGlyphs means by "a value
// that a probe could veto is not a setting".
//
// The ART is the one thing that cannot ride along, and the reason is a width
// fact rather than a taste. Measured on this tree: a 48-cell row of U+2580 is
// 96 columns in that mode. Art rows are cut in cell space, so ansi.Truncate
// never amputates them -- but the frame renders the whole page at a width it
// was told, and it measures every content row it is handed. A 96-column row
// inside a box asked for 78 breaks the box. The frame and the markers stay
// because their widths are still predictable; the drawing goes.
//
// Decided here rather than at the draw site so that Model.Body stays a function
// of its inputs: the environment is read once, where the vocabulary is chosen.
func TestAnExplicitUnicodeKeepsItsGlyphsAndLosesOnlyTheArtInEastAsianMode(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured string
		env        string
		wantName   string
		wantArt    bool
	}{
		{"unicode in the ordinary width mode draws art", "unicode", "", "unicode", true},
		{"unicode with the east-asian mode on keeps its glyphs", "unicode", "1", "unicode", false},
		{"unicode with the east-asian mode spelled true", "unicode", "true", "unicode", false},
		{"a value the width engine itself ignores leaves the art on", "unicode", "yes", "unicode", true},
		{"the variable explicitly off leaves the art on", "unicode", "0", "unicode", true},
		{"auto never reaches the unicode set in that mode", "auto", "1", "ascii", false},
		{"ascii never draws art", "ascii", "", "ascii", false},
		{"ascii asked for in that mode is still ascii", "ascii", "1", "ascii", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RUNEWIDTH_EASTASIAN", tc.env)
			g := PickGlyphs(tc.configured, true)
			if g.Name != tc.wantName {
				t.Errorf("PickGlyphs(%q) is the %s set, want %s", tc.configured, g.Name, tc.wantName)
			}
			if g.Art != tc.wantArt {
				t.Errorf("PickGlyphs(%q) draws art: %v, want %v", tc.configured, g.Art, tc.wantArt)
			}
			if tc.wantName == "unicode" && g.Cursor != UnicodeGlyphs.Cursor {
				t.Errorf("the escape hatch dropped the cursor along with the art: %q", g.Cursor)
			}
		})
	}
}

// The package's own set is a value nobody may quietly turn off.
//
// PickGlyphs clears Art on a COPY, and this is what holds it to that. Without
// it, an arm written as `UnicodeGlyphs.Art = false; return UnicodeGlyphs` would
// pass every case above and disable the art for the rest of the process, in a
// way that reproduces only when a test happens to run after that one.
func TestPickingGlyphsInEastAsianModeDoesNotDisarmThePackagesOwnSet(t *testing.T) {
	t.Setenv("RUNEWIDTH_EASTASIAN", "1")
	if g := PickGlyphs("unicode", true); g.Art {
		t.Fatalf("the east-asian arm did not clear Art, so the rest of this test is meaningless")
	}
	if !UnicodeGlyphs.Art {
		t.Error("PickGlyphs cleared Art on the package's UnicodeGlyphs instead of on its own copy")
	}
}

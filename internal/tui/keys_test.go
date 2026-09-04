package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/theme"
)

func TestTheKeybarWrapsWithoutDroppingAMainPageCommand(t *testing.T) {
	k := DefaultKeys()
	lines := keybar(newHelp(UnicodeGlyphs.Cue, theme.Palette{}), k, 35, UnicodeGlyphs.Cue)
	for _, binding := range k.ShortHelp() {
		help := binding.Help()
		if !strings.Contains(lines, help.Key+" "+help.Desc) {
			t.Errorf("35-column keybar dropped %q:\n%s", help.Key+" "+help.Desc, lines)
		}
	}
	if !strings.Contains(lines, "\n") {
		t.Fatalf("35-column keybar did not wrap:\n%s", lines)
	}
}

// help.SetWidth is not a bound. shouldAddItem returns "add it anyway" when the
// item overflows AND the ellipsis also does not fit, and the loop then keeps
// adding every remaining binding. Measured on this 53-wide keybar: SetWidth(30)
// gave 28, SetWidth(37) gave 53 and SetWidth(45) gave 53. Truncation is
// non-monotone and it overflows, so the two widths that were measured to fail
// are the two this test names.
//
// This one deliberately does NOT strip, and it is drawn from the palette that
// paints. ansi.StringWidth is ANSI-aware, so the escapes cost it nothing, and
// the painted bar is exactly the string this bound has to hold for -- stripping
// first would take it out of the one test that bounds it.
func TestTheKeybarNeverExceedsTheWidthItWasGiven(t *testing.T) {
	h, k := newHelp(UnicodeGlyphs.Cue, theme.Of(theme.Dark)), DefaultKeys()
	for _, w := range []int{20, 30, 37, 45, 53, 80, 113} {
		got := keybar(h, k, w, UnicodeGlyphs.Cue)
		for _, line := range strings.Split(got, "\n") {
			if n := ansi.StringWidth(line); n > w {
				t.Errorf("keybar line at width %d is %d columns wide: %q", w, n, line)
			}
		}
	}
}

// Wrapping keeps every command visible, including quit, instead of replacing
// the right side with a truncation cue.
func TestTheKeybarShowsEveryCommandAtEveryViableWidth(t *testing.T) {
	h, k := newHelp(UnicodeGlyphs.Cue, theme.Of(theme.Dark)), DefaultKeys()
	for _, w := range []int{20, 30, 37, 45, 53, 80, 113} {
		got := ansi.Strip(keybar(h, k, w, UnicodeGlyphs.Cue))
		for _, binding := range k.ShortHelp() {
			help := binding.Help()
			if !strings.Contains(got, help.Key+" "+help.Desc) {
				t.Errorf("keybar at width %d dropped %q: %q", w, help.Key+" "+help.Desc, got)
			}
		}
	}
}

// help's own separator is U+2022 and its ellipsis is U+2026. The keybar stays
// 7-bit ASCII even now that the page around it draws box characters and blocks
// on purpose, and the reason is local to this line: every character it emits
// that is not a binding's own name is either a separator or a cut cue, and both
// are ASCII in both glyph sets. A terminal on a code page that lacks either
// would render a replacement glyph in the middle of the one line telling a user
// how to leave.
//
// It strips first because the bar is painted now, and the escape bytes an SGR
// sequence adds are ASCII themselves: a sweep that left them in would go on
// passing while saying nothing about the characters a reader sees.
func TestTheKeybarIsSevenBitAscii(t *testing.T) {
	for _, w := range []int{37, 53, 113} {
		for _, r := range ansi.Strip(keybar(newHelp(UnicodeGlyphs.Cue, theme.Of(theme.Dark)), DefaultKeys(), w, UnicodeGlyphs.Cue)) {
			if r > 127 {
				t.Fatalf("keybar at width %d carries %q (U+%04X)", w, r, r)
			}
		}
	}
}

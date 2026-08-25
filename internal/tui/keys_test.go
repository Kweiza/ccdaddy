package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/theme"
)

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
		if n := ansi.StringWidth(got); n > w {
			t.Errorf("keybar at width %d is %d columns wide: %q", w, n, got)
		}
	}
}

// help truncates from the RIGHT, so the last binding is the first casualty.
// Quit must never be it: a user stranded in a full-screen program with no
// advertised way out is a worse failure than a missing list toggle, and the
// table already is the list.
func TestQuitOutlivesListInTheKeybar(t *testing.T) {
	order := DefaultKeys().ShortHelp()
	iq, il := -1, -1
	for i, b := range order {
		switch b.Help().Key {
		case "q":
			iq = i
		case "l":
			il = i
		}
	}
	if iq < 0 || il < 0 {
		t.Fatal("the keybar does not offer both q and l")
	}
	if iq > il {
		t.Fatalf("q is at %d and l at %d: help drops from the right, so this strands the user", iq, il)
	}
}

// TestQuitOutlivesListInTheKeybar checks ShortHelp's ORDER; this checks the
// actual RENDERED bar, because the order alone does not say at which widths q
// really survives -- that depends on how many columns the bindings ahead of
// it cost too. Measured directly against keybar()'s output: q is entirely
// absent below width 45 (at 37, for instance, the bar cuts off inside "c
// strategy" and never reaches q at all) and present at 45 and every width
// from there up to the full 53. Below 45, every truncated rendering must end
// in the cue -- the visual cue that something was cut, which an empty tail
// silently omitted at exactly the widths this task exists to fix.
//
// Both loops strip, and the bar is drawn from the palette that PAINTS rather
// than from a colourless one, which is the pairing that makes them mean
// anything. The assertions are about the characters a reader sees and the
// escapes sit exactly where they would break them: "q" is followed by an SGR
// reset before its space, so Contains(got, "q ") misses on a bar that is
// showing q perfectly well, and a cut bar ends in the cue followed by a reset,
// so HasSuffix misses on a bar that flagged its cut correctly. Asserting on the
// colourless bar instead would keep them green and stop them covering the bar
// this program actually draws.
func TestTheKeybarShowsQAsSoonAsItFitsAndFlagsWhenItCuts(t *testing.T) {
	h, k := newHelp(UnicodeGlyphs.Cue, theme.Of(theme.Dark)), DefaultKeys()
	for _, w := range []int{45, 53, 80, 113} {
		got := ansi.Strip(keybar(h, k, w, UnicodeGlyphs.Cue))
		if !strings.Contains(got, "q ") {
			t.Errorf("keybar at width %d does not show q: %q", w, got)
		}
	}
	for _, w := range []int{20, 30, 37} {
		got := ansi.Strip(keybar(h, k, w, UnicodeGlyphs.Cue))
		if !strings.HasSuffix(got, UnicodeGlyphs.Cue) {
			t.Errorf("keybar at width %d was cut but does not end in the cue: %q", w, got)
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

package theme

import (
	"image/color"
	"testing"

	"charm.land/lipgloss/v2"
)

// The contrast gate.
//
// It runs against a SET of grounds, not one ground, because the runtime signal
// is a single boolean: the terminal is asked whether its background is dark
// and answers yes or no. "Dark" is then every dark terminal anybody uses, and
// a palette that clears 7:1 on pure black and fails on Nord's #2e3440 is a
// palette that fails for a real reader. The worst ground in each set is what
// the numbers below are judged on.
//
// The ANSI theme is not here and cannot be: its sixteen slots are whatever the
// reader's terminal maps them to, so there is no luminance to measure. What
// gates that theme is the separation test, which is the only property this
// package can still promise once it has given the colours away.

var darkGrounds = []color.Color{
	lipgloss.Color("#000000"), // plain black
	lipgloss.Color("#1e1e1e"),
	lipgloss.Color("#282c34"),
	lipgloss.Color("#2e3440"), // the worst of the set at luminance 0.0341
	lipgloss.Color("#002b36"),
	lipgloss.Color("#272822"),
	lipgloss.Color("#333333"),
}

var lightGrounds = []color.Color{
	lipgloss.Color("#ffffff"),
	lipgloss.Color("#f5f5f5"),
	lipgloss.Color("#eeeeee"), // the worst of the set
	lipgloss.Color("#fdf6e3"),
	lipgloss.Color("#fafafa"),
}

// textBar is for roles a reader reads words in. markBar is for roles that
// paint a rule, a cursor, a marker or a bar -- shapes, not letters, which is
// the distinction WCAG itself draws between text and non-text contrast.
//
// The accent is a mark. It paints the frame, the cursor and the wordmark's
// block art, and it never spells a word a reader has to decode.
const (
	textBar = 7.0
	markBar = 4.5
)

func contrastBar(r Role) float64 {
	switch r {
	case RoleAccent, RoleGaugeCool, RoleGaugeOK, RoleGaugeWarn, RoleGaugeHigh, RoleGaugeOver:
		return markBar
	}
	return textBar
}

// trackCoverage is how much of its cell the unfilled-track glyph inks. The
// medium shade is a half-coverage character by construction, which is why the
// track gets measured blended rather than solid.
const trackCoverage = 0.5

func TestEveryRoleClearsItsContrastBarOnEveryGround(t *testing.T) {
	for _, tc := range []struct {
		name    Name
		grounds []color.Color
	}{
		{Dark, darkGrounds},
		{Light, lightGrounds},
	} {
		p := Of(tc.name)
		for r := Role(0); r < numRoles; r++ {
			if r == RoleDefault || r == RoleGaugeEmpty {
				// RoleDefault is the reader's own foreground and they own
				// its contrast. RoleGaugeEmpty has its own test, below,
				// because its bar is set by physics rather than by policy.
				continue
			}
			bar := contrastBar(r)
			worst, worstGround := 1e9, color.Color(nil)
			for _, g := range tc.grounds {
				if got := contrastRatio(p.Color(r), g); got < worst {
					worst, worstGround = got, g
				}
			}
			if worst < bar {
				gr, gg, gb := srgb(worstGround)
				t.Errorf("theme %s: %s reaches only %.2f:1 (bar %.1f) on ground #%02x%02x%02x",
					tc.name, roleNames[r], worst, bar,
					int(gr*255+0.5), int(gg*255+0.5), int(gb*255+0.5))
			}
			t.Logf("theme %s: %-15s worst %.2f:1 (bar %.1f)", tc.name, roleNames[r], worst, bar)
		}
	}
}

func TestTheDarkGaugeTrackClearsTheMarkBarBlended(t *testing.T) {
	// The track is ink at half coverage, so what the reader's eye integrates
	// is the mix, not the ink. On the worst dark ground #e4e4e4 lands at
	// #aaabac, which is where the number below comes from.
	p := Of(Dark)
	worst := 1e9
	for _, g := range darkGrounds {
		if got := blendedContrastRatio(p.Color(RoleGaugeEmpty), g, trackCoverage); got < worst {
			worst = got
		}
	}
	if worst < markBar {
		t.Errorf("dark gauge track reaches only %.2f:1 blended, bar %.1f", worst, markBar)
	}
	t.Logf("dark gauge track: worst %.2f:1 blended", worst)
}

func TestTheLightGaugeTrackSitsAtThePhysicalCeilingAndNotAtTheMarkBar(t *testing.T) {
	// This is the one role in the palette that does not reach 4.5:1, and no
	// colour could: at half coverage on a light ground the CEILING for any
	// ink whatsoever is about 1.90:1, because the darkest possible mix is
	// half the ground's own light. Measured across the ground set: 1.91 on
	// #ffffff, 1.90 on #f5f5f5, #eeeeee and #fdf6e3, 1.91 on #fafafa.
	//
	// So the requirement for this role is stated as what is achievable
	// instead of what is standard: distinguishable from the fill, and as far
	// from the ground as the glyph's coverage permits. The reading itself is
	// carried by the percentage printed beside the bar, which is not a
	// mitigation invented here -- it is why the percentage is outside the bar
	// rather than inside it.
	//
	// The floor below IS the ceiling, to within the rounding between 1.895
	// and 1.909 across the ground set. A light track that scores lower has
	// been lightened off the physical maximum; one that scores higher is
	// arithmetic nobody should believe without re-deriving it.
	const (
		ceiling = 1.90 // the physical maximum, and 1.909 before rounding
		floor   = 1.89 // measured worst: 1.895 on #eeeeee
	)
	p := Of(Light)
	worst, best := 1e9, 0.0
	for _, g := range lightGrounds {
		got := blendedContrastRatio(p.Color(RoleGaugeEmpty), g, trackCoverage)
		if got < worst {
			worst = got
		}
		if got > best {
			best = got
		}
	}
	if worst < floor {
		t.Errorf("light gauge track reaches only %.3f:1 blended, and %.2f is the ceiling", worst, ceiling)
	}
	if best > ceiling+0.05 {
		t.Errorf("light gauge track reaches %.3f:1 blended, which is above the physical ceiling %.2f -- "+
			"the blend is being computed in the wrong space", best, ceiling)
	}
	t.Logf("light gauge track: worst %.3f:1, best %.3f:1 blended", worst, best)
}

func TestTheGaugeTrackIsSeparableFromEveryFill(t *testing.T) {
	// Whatever the track's contrast against the GROUND is, it has to be
	// distinguishable from the fill beside it -- that is the comparison the
	// reader actually makes, and it is the one the ceiling above does not
	// speak to.
	for _, n := range []Name{Dark, Light} {
		p := Of(n)
		for _, r := range gaugeFills {
			if got := deltaE2000(p.Color(RoleGaugeEmpty), p.Color(r)); got < 10 {
				t.Errorf("theme %s: the gauge track and %s are %.2f dE00 apart", n, roleNames[r], got)
			}
		}
	}
}

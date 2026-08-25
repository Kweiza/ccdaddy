package theme

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The downsample gate, and it runs through the REAL conversion functions.
//
// A terminal that cannot take truecolor does not get the palette; it gets
// whatever the writer converts the palette into, and the writer's path is
// colorprofile's Convert, which is ansi.Convert256 followed for a 16-colour
// terminal by ansi.Convert16. Convert256 is a 6x6x6 cube quantisation with a
// grey tiebreak decided by HSLuv distance -- it is not nearest-neighbour by
// any perceptual metric, and a palette solved against a model of it rather
// than against it is a palette that ships an exhausted state rendered as
// mid-grey. That happened once already.
//
// The whole point is that these are the library's functions and not a copy of
// them: when the pinned version's tables move, this test is what says so.

// The 256-colour indices every role lands on. This is a contract and it is
// written out in full rather than computed, because a test that recomputes
// what it is checking checks nothing.
var want256 = map[Name][numRoles]int{
	Dark: {
		RoleAccent: 210, RoleActive: 156, RoleCandidate: 81, RoleExhausted: 217,
		RoleQuarantined: 184, RoleMuted: 188, RoleHeader: 231, RoleNotice: 229,
		RoleGaugeOK: 79, RoleGaugeWarn: 214, RoleGaugeOver: 209, RoleGaugeEmpty: 254,
	},
	Light: {
		RoleAccent: 95, RoleActive: 22, RoleCandidate: 18, RoleExhausted: 52,
		RoleQuarantined: 58, RoleMuted: 239, RoleHeader: 234, RoleNotice: 58,
		RoleGaugeOK: 23, RoleGaugeWarn: 94, RoleGaugeOver: 88, RoleGaugeEmpty: 16,
	},
}

// The indices that carry no hue: the 24-step grey ramp, and the six cells of
// the colour cube whose three channels are equal. A red that lands on one of
// these is not a dimmer red, it is grey, and the reader loses the distinction
// entirely rather than losing some of it.
func neutralIndex(i int) bool {
	if i >= 232 {
		return true
	}
	switch i {
	case 16, 59, 102, 145, 188, 231:
		return true
	}
	return false
}

func TestTheDownsampledIndicesAreTheOnesThePaletteWasSolvedFor(t *testing.T) {
	for _, n := range []Name{Dark, Light} {
		p := Of(n)
		for r := Role(0); r < numRoles; r++ {
			if r == RoleDefault {
				continue
			}
			got := int(ansi.Convert256(p.Color(r)))
			if w := want256[n][r]; got != w {
				t.Errorf("theme %s: %s downsamples to %d, want %d", n, roleNames[r], got, w)
			}
		}
	}
}

func TestTheStateRolesStaySeparableAllTheWayDownTo16Colours(t *testing.T) {
	// Five roles in one column. Two that convert to the same index are two
	// states a reader on a 256-colour or 16-colour terminal cannot tell
	// apart by colour at all.
	for _, n := range colouredThemes {
		p := Of(n)
		for i := 0; i < len(stateRoles); i++ {
			for j := i + 1; j < len(stateRoles); j++ {
				a, b := stateRoles[i], stateRoles[j]
				if x, y := ansi.Convert256(p.Color(a)), ansi.Convert256(p.Color(b)); x == y {
					t.Errorf("theme %s at 256 colours: %s and %s both land on %d",
						n, roleNames[a], roleNames[b], int(x))
				}
				if x, y := ansi.Convert16(p.Color(a)), ansi.Convert16(p.Color(b)); x == y {
					t.Errorf("theme %s at 16 colours: %s and %s both land on %d",
						n, roleNames[a], roleNames[b], int(x))
				}
			}
		}
	}
}

func TestTheGaugeFillsStaySeparableAt256(t *testing.T) {
	for _, n := range colouredThemes {
		p := Of(n)
		for i := 0; i < len(gaugeFills); i++ {
			for j := i + 1; j < len(gaugeFills); j++ {
				a, b := gaugeFills[i], gaugeFills[j]
				if x, y := ansi.Convert256(p.Color(a)), ansi.Convert256(p.Color(b)); x == y {
					t.Errorf("theme %s at 256 colours: %s and %s both land on %d",
						n, roleNames[a], roleNames[b], int(x))
				}
			}
		}
	}
}

func TestTheWarnAndOverFillsMergeAt16ColoursAndThatIsExpected(t *testing.T) {
	// EXPECTED, and asserted rather than left implicit so that it cannot
	// change without somebody deciding it should.
	//
	// A 16-colour terminal has one red and one bright red, and the warning
	// band and the over-threshold band are both warm; the conversion table
	// sends them to the same slot in both themes. The bar carries one colour
	// at a time, so the two never appear side by side to be compared -- and
	// the number printed beside the bar is what actually tells a reader which
	// band they are in. The three fills stay distinct at 256, which is what
	// the previous test pins; below that the percentage is the carrier.
	for _, n := range []Name{Dark, Light} {
		p := Of(n)
		warn := ansi.Convert16(p.Color(RoleGaugeWarn))
		over := ansi.Convert16(p.Color(RoleGaugeOver))
		if warn != over {
			t.Errorf("theme %s at 16 colours: warn=%d over=%d now differ; this is an improvement, "+
				"but it is a change to a documented alias -- update this test deliberately",
				n, int(warn), int(over))
		}
		if ok := ansi.Convert16(p.Color(RoleGaugeOK)); ok == warn {
			t.Errorf("theme %s at 16 colours: the ok fill collapsed onto the warn fill at %d", n, int(ok))
		}
	}
}

func TestQuarantinedAndNoticeShareOneLightIndexAndThatIsExpected(t *testing.T) {
	// EXPECTED. Both land on 58, the cube's dark olive.
	//
	// The alias is permitted because the two roles cannot be confused for
	// each other on the page: a notice is a whole line prefixed "note:" that
	// sits above the column header, and a quarantined state is one cell in
	// the STATE column beside its own glyph and its own word. They never
	// adjoin, and neither one's meaning is carried by its colour. The
	// separation gate in this package is scoped to the must-separable sets
	// for exactly this reason -- "no two roles alike" would be a stricter
	// rule than the page needs, and it would have cost a role somewhere that
	// does need its colour.
	p := Of(Light)
	q := ansi.Convert256(p.Color(RoleQuarantined))
	notice := ansi.Convert256(p.Color(RoleNotice))
	if q != notice || int(q) != 58 {
		t.Errorf("light: quarantined=%d notice=%d, want both 58; the documented alias moved",
			int(q), int(notice))
	}
}

func TestNoRedRoleLandsOnANeutralIndex(t *testing.T) {
	// The grey tiebreak in Convert256 compares HSLuv hue WITHOUT
	// wrap-around, so a red sitting just clockwise of 0 degrees scores badly
	// against its own cube cell and well against flat grey. Measured on the
	// pinned library: #dc1554 goes to 242 (#6c6c6c), #e01040 to 241
	// (#626262). Both are greys. Every red in this palette is far enough
	// round the wheel to stay chromatic, and this is the gate that keeps it
	// that way.
	//
	// The accent is in the set although it is the warmest role rather than a
	// red proper: it is the one whose index would move first if the accent is
	// ever retuned, so it is the one worth watching.
	for _, n := range []Name{Dark, Light} {
		p := Of(n)
		for _, r := range []Role{RoleAccent, RoleExhausted, RoleGaugeOver} {
			i := int(ansi.Convert256(p.Color(r)))
			if neutralIndex(i) {
				t.Errorf("theme %s: %s downsamples to %d, which carries no hue", n, roleNames[r], i)
			}
		}
	}
}

func TestTheAnsiThemeIsCarriedThroughUnconverted(t *testing.T) {
	// The sixteen-slot theme must survive the writer's conversion untouched,
	// because its whole contract is that the terminal resolves it. Convert16
	// returns a basic colour unchanged; if a role here were ever spelled as
	// hex instead, it would be re-quantised and the user's own theme would
	// stop owning it.
	p := Of(ANSI)
	for r := Role(0); r < numRoles; r++ {
		if r == RoleDefault {
			continue
		}
		basic, ok := p.Color(r).(ansi.BasicColor)
		if !ok {
			t.Errorf("ansi theme: %s is not one of the sixteen slots", roleNames[r])
			continue
		}
		if got := ansi.Convert16(p.Color(r)); got != basic {
			t.Errorf("ansi theme: %s converted from %d to %d", roleNames[r], int(basic), int(got))
		}
	}
	if p.Color(RoleExhausted) != lipgloss.BrightRed {
		t.Error("ansi theme: the exhausted state must be the terminal's own bright red")
	}
}

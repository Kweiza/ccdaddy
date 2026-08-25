package theme

import (
	"image/color"
	"testing"
)

// The colour-vision gate.
//
// There is no daltonised theme in this package and there is not going to be
// one. Claude Code ships two because colour is often its only carrier; this
// dashboard gave that up on purpose -- every distinction it draws also has a
// glyph, and wherever the STATE column survives the width ladder it also has
// its word. A theme that re-hues the palette for one kind of vision buys
// nothing a reader here did not already have.
//
// That argument is only worth anything if the numbers hold, so they are
// measured rather than asserted in prose. The five roles the STATE column
// paints stay ten dE00 apart under full protanopia, deuteranopia and
// tritanopia, in both themes.
//
// Ten is a deliberate floor and it is close to what the arithmetic allows.
// Under protanopia and deuteranopia green, amber and red collapse onto one
// axis, so four of the five roles have nothing left to separate on but
// lightness -- inside the roughly 22 L* that the 7:1 contrast floor leaves on
// a dark ground. That is what caps the answer near 10.7, not the choice of
// hues: every hue-family substitution was searched, and each one bought CVD
// distance only by desaturating some role into off-white or near-black, which
// costs the contrast gate what it gains here.

// cvdKinds is a slice and not a range over the matrix map, because a map
// range is randomised and a failing gate that names a different pair on every
// run is a gate nobody can bisect.
var cvdKinds = []string{"protan", "deutan", "tritan"}

// separationFloor is the dE00 two state roles must stay apart by. dE00 is on
// the 0..100 scale here; a library that returns hundredths would make every
// number below read as about 0.2 and every gate pass on nothing.
const separationFloor = 10.0

func TestTheStateRolesStayApartUnderEveryColourVisionDeficiency(t *testing.T) {
	for _, n := range []Name{Dark, Light} {
		p := Of(n)
		for _, kind := range cvdKinds {
			worst, worstPair := 1e9, ""
			for i := 0; i < len(stateRoles); i++ {
				for j := i + 1; j < len(stateRoles); j++ {
					a, b := stateRoles[i], stateRoles[j]
					got := deltaE2000(
						simulateCVD(p.Color(a), kind),
						simulateCVD(p.Color(b), kind),
					)
					if got < separationFloor {
						t.Errorf("theme %s under %s: %s and %s are %.2f dE00 apart, floor %.1f",
							n, kind, roleNames[a], roleNames[b], got, separationFloor)
					}
					if got < worst {
						worst, worstPair = got, roleNames[a]+"|"+roleNames[b]
					}
				}
			}
			t.Logf("theme %s under %s: worst pair %s at %.2f dE00", n, kind, worstPair, worst)
		}
	}
}

func TestTheStateRolesStayApartForAReaderWithNoDeficiencyAtAll(t *testing.T) {
	// The unsimulated case is not implied by the three simulated ones: a
	// palette could in principle be tuned until two roles that separate under
	// every deficiency converge for everybody else.
	for _, n := range []Name{Dark, Light} {
		p := Of(n)
		for i := 0; i < len(stateRoles); i++ {
			for j := i + 1; j < len(stateRoles); j++ {
				a, b := stateRoles[i], stateRoles[j]
				if got := deltaE2000(p.Color(a), p.Color(b)); got < separationFloor*2 {
					t.Errorf("theme %s: %s and %s are %.2f dE00 apart", n, roleNames[a], roleNames[b], got)
				}
			}
		}
	}
}

func TestActiveAndExhaustedAreTheBindingPairAndTheyClear(t *testing.T) {
	// The one comparison a reader makes most: the account being used against
	// the account that has nothing left. Deuteranopia is what binds it, on
	// both themes, and it clears without a non-colour cue -- the cues exist
	// anyway, a filled square and its word against a cross and its word.
	for _, n := range []Name{Dark, Light} {
		p := Of(n)
		active, exhausted := p.Color(RoleActive), p.Color(RoleExhausted)
		t.Logf("theme %s: active|exhausted normal %.2f", n, deltaE2000(active, exhausted))
		for _, kind := range cvdKinds {
			got := deltaE2000(simulateCVD(active, kind), simulateCVD(exhausted, kind))
			if got < separationFloor {
				t.Errorf("theme %s under %s: active|exhausted %.2f dE00, floor %.1f",
					n, kind, got, separationFloor)
			}
			t.Logf("theme %s: active|exhausted %s %.2f", n, kind, got)
		}
	}
}

func TestSimulationIsNotTheIdentity(t *testing.T) {
	// A gate that simulated nothing would pass on the unsimulated palette and
	// report nothing wrong. This is what says the matrices are being applied.
	red := color.RGBA{R: 0xff, G: 0x00, B: 0x00, A: 0xff}
	green := color.RGBA{R: 0x00, G: 0xff, B: 0x00, A: 0xff}
	for _, kind := range cvdKinds {
		normal := deltaE2000(red, green)
		got := deltaE2000(simulateCVD(red, kind), simulateCVD(green, kind))
		if got >= normal {
			t.Errorf("%s left red and green %.2f dE00 apart against %.2f unsimulated", kind, got, normal)
		}
	}
}

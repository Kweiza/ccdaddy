package theme

import (
	"image/color"
	"math"
	"testing"
)

// The colour arithmetic the three gate tests in this package are written
// against, implemented here rather than imported.
//
// Nothing here is a dependency. go-colorful is already in the module graph as
// an indirect requirement of lipgloss, so importing it would not download a
// new module -- but it would promote that line out of the indirect block the
// next time anybody runs `go mod tidy`, which makes a transitive pin this
// repository does not control into a direct one it does. The arithmetic below
// is eighty lines and every one of them is checked against published values by
// the first test in this file, which is a better trade than owning a
// dependency edge for a test helper.
//
// One number to know if you ever cross-check against that library: its
// DistanceCIEDE2000 returns dE00 divided by 100. It scales L, a and b up by
// 100 and divides the result back down. Compared straight against the floors
// here, every separation would read about 0.2 and every gate would pass on
// nothing.

// srgb pulls a colour apart into three 0..1 sRGB channels. It goes through
// RGBA() rather than through a type switch so that it answers for every
// color.Color a palette can hold, and it divides by 0xffff because RGBA is
// 16 bits per channel, not 8.
func srgb(c color.Color) (r, g, b float64) {
	ri, gi, bi, _ := c.RGBA()
	return float64(ri) / 0xffff, float64(gi) / 0xffff, float64(bi) / 0xffff
}

// linearize undoes the sRGB transfer function. Every blend and every luminance
// below happens in linear light, because light adds linearly and sRGB values
// do not -- averaging two sRGB numbers is not what a half-covered cell looks
// like.
func linearize(v float64) float64 {
	if v <= 0.04045 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}

func delinearize(v float64) float64 {
	if v <= 0.0031308 {
		return v * 12.92
	}
	return 1.055*math.Pow(v, 1.0/2.4) - 0.055
}

// relativeLuminance is WCAG 2.x's Y, and its coefficients are WCAG's own.
func relativeLuminance(c color.Color) float64 {
	r, g, b := srgb(c)
	return 0.2126*linearize(r) + 0.7152*linearize(g) + 0.0722*linearize(b)
}

// contrastRatio is WCAG 2.x's (L1+0.05)/(L2+0.05), lighter over darker.
func contrastRatio(a, b color.Color) float64 {
	la, lb := relativeLuminance(a), relativeLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// blendedContrastRatio is the contrast of ink laid over ground at a fractional
// coverage. It exists for one role: the gauge's unfilled track is a glyph that
// covers about half its cell, so what a reader sees is not the ink but the ink
// mixed into the ground, and mixing happens in linear light.
func blendedContrastRatio(ink, ground color.Color, coverage float64) float64 {
	ir, ig, ib := srgb(ink)
	gr, gg, gb := srgb(ground)
	mix := func(i, g float64) float64 {
		return coverage*linearize(i) + (1-coverage)*linearize(g)
	}
	l := 0.2126*mix(ir, gr) + 0.7152*mix(ig, gg) + 0.0722*mix(ib, gb)
	lg := relativeLuminance(ground)
	if l < lg {
		l, lg = lg, l
	}
	return (l + 0.05) / (lg + 0.05)
}

// lab is CIE L*a*b* under the D65 white point, by way of linear sRGB and XYZ.
func lab(c color.Color) (l, a, b float64) {
	sr, sg, sb := srgb(c)
	rl, gl, bl := linearize(sr), linearize(sg), linearize(sb)
	x := 0.4124564*rl + 0.3575761*gl + 0.1804375*bl
	y := 0.2126729*rl + 0.7151522*gl + 0.0721750*bl
	z := 0.0193339*rl + 0.1191920*gl + 0.9503041*bl
	return xyzToLab(x/0.95047, y/1.0, z/1.08883)
}

func xyzToLab(x, y, z float64) (l, a, b float64) {
	f := func(t float64) float64 {
		if t > 216.0/24389.0 {
			return math.Cbrt(t)
		}
		return (24389.0/27.0*t + 16) / 116
	}
	fx, fy, fz := f(x), f(y), f(z)
	return 116*fy - 16, 500 * (fx - fy), 200 * (fy - fz)
}

// deltaE2000 is CIEDE2000 on the conventional 0..100 scale, where 1 is about
// one just-noticeable difference. The gates in this package are written in
// that scale and in no other.
func deltaE2000(c1, c2 color.Color) float64 {
	l1, a1, b1 := lab(c1)
	l2, a2, b2 := lab(c2)
	return deltaE2000Lab(l1, a1, b1, l2, a2, b2)
}

func deltaE2000Lab(l1, a1, b1, l2, a2, b2 float64) float64 {
	const deg = math.Pi / 180
	cbar := (math.Hypot(a1, b1) + math.Hypot(a2, b2)) / 2
	c7 := math.Pow(cbar, 7)
	g := 0.5 * (1 - math.Sqrt(c7/(c7+math.Pow(25, 7))))
	a1p, a2p := (1+g)*a1, (1+g)*a2
	c1p, c2p := math.Hypot(a1p, b1), math.Hypot(a2p, b2)

	hue := func(ap, bp float64) float64 {
		if ap == 0 && bp == 0 {
			return 0
		}
		h := math.Atan2(bp, ap) / deg
		if h < 0 {
			h += 360
		}
		return h
	}
	h1p, h2p := hue(a1p, b1), hue(a2p, b2)

	dLp := l2 - l1
	dCp := c2p - c1p
	var dhp float64
	switch {
	case c1p*c2p == 0:
		dhp = 0
	case math.Abs(h2p-h1p) <= 180:
		dhp = h2p - h1p
	case h2p-h1p > 180:
		dhp = h2p - h1p - 360
	default:
		dhp = h2p - h1p + 360
	}
	dHp := 2 * math.Sqrt(c1p*c2p) * math.Sin(dhp/2*deg)

	lbar := (l1 + l2) / 2
	cbarp := (c1p + c2p) / 2
	var hbar float64
	switch {
	case c1p*c2p == 0:
		hbar = h1p + h2p
	case math.Abs(h1p-h2p) <= 180:
		hbar = (h1p + h2p) / 2
	case h1p+h2p < 360:
		hbar = (h1p + h2p + 360) / 2
	default:
		hbar = (h1p + h2p - 360) / 2
	}
	tt := 1 - 0.17*math.Cos((hbar-30)*deg) + 0.24*math.Cos(2*hbar*deg) +
		0.32*math.Cos((3*hbar+6)*deg) - 0.20*math.Cos((4*hbar-63)*deg)
	dTheta := 30 * math.Exp(-math.Pow((hbar-275)/25, 2))
	cbarp7 := math.Pow(cbarp, 7)
	rc := 2 * math.Sqrt(cbarp7/(cbarp7+math.Pow(25, 7)))
	sl := 1 + (0.015*math.Pow(lbar-50, 2))/math.Sqrt(20+math.Pow(lbar-50, 2))
	sc := 1 + 0.045*cbarp
	sh := 1 + 0.015*cbarp*tt
	rt := -math.Sin(2*dTheta*deg) * rc

	return math.Sqrt(math.Pow(dLp/sl, 2) + math.Pow(dCp/sc, 2) + math.Pow(dHp/sh, 2) +
		rt*(dCp/sc)*(dHp/sh))
}

// The Machado, Oliveira and Gomes 2009 simulation matrices at severity 1.0,
// applied in linear RGB, which is the space that paper derives them in. Each
// row sums to 1, so an achromatic colour comes back unchanged -- which is the
// property the helper test below checks, because a transposed or mistyped
// matrix loses it.
var cvdMatrices = map[string][9]float64{
	"protan": {
		0.152286, 1.052583, -0.204868,
		0.114503, 0.786281, 0.099216,
		-0.003882, -0.048116, 1.051998,
	},
	"deutan": {
		0.367322, 0.860646, -0.227968,
		0.280085, 0.672501, 0.047413,
		-0.011820, 0.042940, 0.968881,
	},
	"tritan": {
		1.255528, -0.076749, -0.178779,
		-0.078411, 0.930809, 0.147602,
		0.004733, 0.691367, 0.303900,
	},
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

// simulateCVD is what a colour looks like to a reader with the named
// deficiency, at full severity.
func simulateCVD(c color.Color, kind string) color.Color {
	m, ok := cvdMatrices[kind]
	if !ok {
		panic("theme: no simulation matrix named " + kind)
	}
	sr, sg, sb := srgb(c)
	rl, gl, bl := linearize(sr), linearize(sg), linearize(sb)
	// The result is kept at sixteen bits per channel. Rounding a simulated
	// colour back into eight bits moves the separations below by a few
	// hundredths, and this gate has under half a dE00 of headroom on its
	// worst pair -- a rounding artefact is not what should be spending it.
	out := func(i int) uint16 {
		v := clamp01(m[i*3]*rl + m[i*3+1]*gl + m[i*3+2]*bl)
		return uint16(math.Round(clamp01(delinearize(v)) * 0xffff))
	}
	return color.RGBA64{R: out(0), G: out(1), B: out(2), A: 0xffff}
}

func TestTheColourMathAgreesWithPublishedValues(t *testing.T) {
	white := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	black := color.RGBA{A: 0xff}
	if got := contrastRatio(white, black); math.Abs(got-21) > 0.001 {
		t.Errorf("contrast(white, black) = %.4f, want 21", got)
	}
	// The darkest grey that clears 4.5:1 on white, which is where WCAG's own
	// worked example lands.
	grey := color.RGBA{R: 0x76, G: 0x76, B: 0x76, A: 0xff}
	if got := contrastRatio(grey, white); math.Abs(got-4.54) > 0.01 {
		t.Errorf("contrast(#767676, white) = %.4f, want about 4.54", got)
	}

	// Pairs from the CIEDE2000 reference data published with the formula.
	// These are the ones that exercise the arcs the naive implementation gets
	// wrong: hue wrap-around, the blue rotation term, and near-zero chroma.
	for _, tc := range []struct {
		l1, a1, b1 float64
		l2, a2, b2 float64
		want       float64
	}{
		{50.0000, 2.6772, -79.7751, 50.0000, 0.0000, -82.7485, 2.0425},
		{50.0000, 3.1571, -77.2803, 50.0000, 0.0000, -82.7485, 2.8615},
		{50.0000, 2.8361, -74.0200, 50.0000, 0.0000, -82.7485, 3.4412},
		{50.0000, -1.3802, -84.2814, 50.0000, 0.0000, -82.7485, 1.0000},
		{50.0000, 2.5000, 0.0000, 50.0000, 0.0000, -2.5000, 4.3065},
		{50.0000, 2.5000, 0.0000, 73.0000, 25.0000, -18.0000, 27.1492},
		{50.0000, 2.5000, 0.0000, 61.0000, -5.0000, 29.0000, 22.8977},
		{50.0000, 2.5000, 0.0000, 56.0000, -27.0000, -3.0000, 31.9030},
		{50.0000, 2.5000, 0.0000, 58.0000, 24.0000, 15.0000, 19.4535},
		{60.2574, -34.0099, 36.2677, 60.4626, -34.1751, 39.4387, 1.2644},
		{63.0109, -31.0961, -5.8663, 62.8187, -29.7946, -4.0864, 1.2630},
		{2.0776, 0.0795, -1.1350, 0.9033, -0.0636, -0.5514, 0.9082},
	} {
		got := deltaE2000Lab(tc.l1, tc.a1, tc.b1, tc.l2, tc.a2, tc.b2)
		if math.Abs(got-tc.want) > 0.0002 {
			t.Errorf("dE00(%.4f,%.4f,%.4f | %.4f,%.4f,%.4f) = %.4f, want %.4f",
				tc.l1, tc.a1, tc.b1, tc.l2, tc.a2, tc.b2, got, tc.want)
		}
	}

	// Every simulation matrix leaves grey alone. A transposed or mistyped
	// matrix does not, and a wrong matrix would make the separation gate in
	// this package measure the wrong thing while still passing.
	for kind := range cvdMatrices {
		for _, v := range []uint8{0x00, 0x40, 0x80, 0xc0, 0xff} {
			in := color.RGBA{R: v, G: v, B: v, A: 0xff}
			out := simulateCVD(in, kind)
			if got := deltaE2000(in, out); got > 0.5 {
				t.Errorf("%s moves grey #%02x%02x%02x by %.2f dE00", kind, v, v, v, got)
			}
		}
	}
}

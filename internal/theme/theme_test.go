package theme

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// roleNames exists so a failure names the role rather than an integer, and it
// is a full-width array on purpose: adding a Role without adding its name here
// fails to compile at the literal or fails the emptiness check below, and
// either is louder than a test that reports "role 12".
var roleNames = [numRoles]string{
	RoleDefault:     "RoleDefault",
	RoleAccent:      "RoleAccent",
	RoleActive:      "RoleActive",
	RoleCandidate:   "RoleCandidate",
	RoleExhausted:   "RoleExhausted",
	RoleQuarantined: "RoleQuarantined",
	RoleMuted:       "RoleMuted",
	RoleHeader:      "RoleHeader",
	RoleNotice:      "RoleNotice",
	RoleGaugeOK:     "RoleGaugeOK",
	RoleGaugeWarn:   "RoleGaugeWarn",
	RoleGaugeOver:   "RoleGaugeOver",
	RoleGaugeEmpty:  "RoleGaugeEmpty",
}

// colouredThemes is every theme that is expected to paint. None is not here
// and that is the whole point of it.
var colouredThemes = []Name{Dark, Light, ANSI}

// stateRoles is the must-separable set: the five roles the STATE column paints
// in one column, where two that look alike is two states a reader cannot tell
// apart. RoleMuted belongs to it because disabled, unknown and the empty state
// all land on it.
var stateRoles = []Role{RoleActive, RoleCandidate, RoleExhausted, RoleQuarantined, RoleMuted}

// gaugeFills is the three colours the filled part of the bar takes. They never
// appear at once -- a bar has one colour -- but a reader who watches one
// account cross a band must see the crossing.
var gaugeFills = []Role{RoleGaugeOK, RoleGaugeWarn, RoleGaugeOver}

func TestEveryRoleIsNamed(t *testing.T) {
	for r := Role(0); r < numRoles; r++ {
		if roleNames[r] == "" {
			t.Errorf("role %d has no name in roleNames; add it", int(r))
		}
	}
}

func TestEveryRoleCarriesAColourInEveryColouredTheme(t *testing.T) {
	// RoleDefault is excepted BY NAME rather than by "whatever has no
	// colour": the exception is one role and naming it is what keeps a
	// second uncoloured role from joining it silently.
	for _, n := range colouredThemes {
		p := Of(n)
		for r := Role(0); r < numRoles; r++ {
			if r == RoleDefault {
				continue
			}
			// By type, never by RGBA. NoColor's RGBA is {0,0,0,0xFFFF},
			// which is black -- and lipgloss.Color returns NoColor on any
			// parse failure, so a typo'd hex would read as a legitimate
			// black in an RGBA comparison and this gate would pass over it.
			if _, none := p.Color(r).(lipgloss.NoColor); none {
				t.Errorf("theme %s: %s has no colour", n, roleNames[r])
			}
		}
		if _, none := p.Color(RoleDefault).(lipgloss.NoColor); !none {
			t.Errorf("theme %s: RoleDefault must stay the terminal's own foreground", n)
		}
	}
}

func TestTheNoneThemeCarriesNoColourForAnyRole(t *testing.T) {
	p := Of(None)
	for r := Role(0); r < numRoles; r++ {
		if _, none := p.Color(r).(lipgloss.NoColor); !none {
			t.Errorf("theme none: %s answered a colour", roleNames[r])
		}
	}
}

func TestTheNoneThemeEmitsNoEscapeByte(t *testing.T) {
	// The contract is stronger than "NoColor": a style whose foreground is
	// SET to NoColor is still a foreground the writer may spell out. Render
	// is what settles it.
	p := Of(None)
	for r := Role(0); r < numRoles; r++ {
		got := p.Style(r).Render("account")
		if strings.ContainsRune(got, 0x1b) {
			t.Errorf("theme none: %s rendered %q, which carries an escape byte", roleNames[r], got)
		}
	}
}

func TestTheZeroPaletteIsTheNoneTheme(t *testing.T) {
	// A Palette threaded through a struct literal written before the field
	// existed arrives here zero. It must render something, and the only safe
	// something is the terminal's own foreground.
	var p Palette
	if p.Name() != None {
		t.Errorf("zero Palette names itself %q, want %q", p.Name(), None)
	}
	for r := Role(0); r < numRoles; r++ {
		if _, none := p.Color(r).(lipgloss.NoColor); !none {
			t.Errorf("zero Palette: %s answered a colour", roleNames[r])
		}
		if got := p.Style(r).Render("account"); got != "account" {
			t.Errorf("zero Palette: %s rendered %q, want %q", roleNames[r], got, "account")
		}
	}
}

func TestARoleOutsideTheTableAnswersNoColour(t *testing.T) {
	// A newer build of another package can hand this one a Role this build
	// has never heard of, the same way the daemon's document format can carry
	// a state this binary has never heard of. Index out of range is not an
	// answer a renderer can use.
	for _, r := range []Role{-1, numRoles, numRoles + 7} {
		if _, none := Of(Dark).Color(r).(lipgloss.NoColor); !none {
			t.Errorf("role %d answered a colour", int(r))
		}
	}
}

func TestAutoResolvesToDarkOrLightAndNothingElse(t *testing.T) {
	if got := Pick(Auto, true); got != Of(Dark) {
		t.Errorf("Pick(auto, dark ground) = %s, want the dark palette", got.Name())
	}
	if got := Pick(Auto, false); got != Of(Light) {
		t.Errorf("Pick(auto, light ground) = %s, want the light palette", got.Name())
	}
	if Of(Auto) != Of(Dark) {
		t.Error("Of(auto) must answer the dark palette, which is what the runtime answers when nothing has told it otherwise")
	}
}

func TestAConfiguredThemeIgnoresTheBackgroundQuery(t *testing.T) {
	// The query is an escape sequence some multiplexers eat. The config is a
	// sentence somebody typed. When they disagree the sentence wins.
	for _, n := range []Name{Dark, Light, ANSI, None} {
		for _, isDark := range []bool{true, false} {
			if got := Pick(n, isDark); got != Of(n) {
				t.Errorf("Pick(%s, isDark=%v) = %s, want %s", n, isDark, got.Name(), n)
			}
		}
	}
}

func TestAnUnvalidatedNameAnswersTheDefaultPalette(t *testing.T) {
	// Package config validates against Names() first, so a name arriving
	// here unvalidated is in the position an unset key is in -- and the unset
	// key's answer is the default, not silence.
	if got := Of(Name("solarized")); got != Of(Dark) {
		t.Errorf("Of(unknown) = %s, want the dark palette", got.Name())
	}
}

func TestNamesIsTheClosedSetTheConfigValidatesAgainst(t *testing.T) {
	want := []Name{Auto, Dark, Light, ANSI, None}
	got := Names()
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Names()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// A caller must not be able to reach the package's own copy.
	Names()[0] = "clobbered"
	if Names()[0] != Auto {
		t.Error("Names() hands out a slice that aliases package state")
	}
}

func TestEveryNameExceptAutoNamesItselfBack(t *testing.T) {
	for _, n := range []Name{Dark, Light, ANSI, None} {
		if got := Of(n).Name(); got != n {
			t.Errorf("Of(%q).Name() = %q", n, got)
		}
	}
	if got := Of(Auto).Name(); got == Auto {
		t.Error("Of(auto).Name() must be a resolved theme; auto is a request, not a palette")
	}
}

func TestParseAnswersTheFiveSpellingsAndRefusesEverythingElse(t *testing.T) {
	for _, n := range Names() {
		got, ok := Parse(string(n))
		if !ok || got != n {
			t.Errorf("Parse(%q) = %q, %v; want %q, true", string(n), got, ok, n)
		}
	}
	// The refusals are the half that matters, and they are the half Of cannot
	// do. Of answers a Palette for every Name including one nobody spelled, so
	// a validator built on Of would accept "sloarized", write it into
	// config.toml and paint dark -- and the user would read their own typo back
	// out of `ccdad config list` as a theme they had chosen.
	//
	// Case and surrounding space are refused for the same reason: a spelling
	// this package accepts and `Names()` never prints is a second namespace,
	// and the next reader of that file matches neither half of it.
	for _, s := range []string{"", "Auto", "AUTO", " dark", "dark ", "solarized", "16", "true"} {
		if got, ok := Parse(s); ok {
			t.Errorf("Parse(%q) = %q, true; want a refusal", s, got)
		}
	}
}

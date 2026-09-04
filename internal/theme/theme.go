// Package theme is the palette every rendered surface reads its colours from.
//
// It is a leaf on purpose. Package cli imports package tui to register a
// command, so package tui can never import package cli, and a palette that
// lived in either would be reachable from only one of them. Package view
// imports nothing new: it stays colour-free, because internal/mcpsrv renders
// the same strings into a stdio protocol where an SGR byte is corruption.
//
// A palette stores color.Color and never lipgloss.Style. lipgloss.Style
// carries a slice field and a func field, so it has no == and no test can
// compare two of them; a colour table that cannot be compared is a colour
// table nothing can gate.
package theme

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// Role is what a colour is FOR, not what it looks like. Every surface names a
// role and the palette answers with a colour, so the same distinction reads
// the same way on the dashboard, in `ccdad status` and in the daemon screen --
// and a theme swap is one table, not a search for hex literals.
type Role int

const (
	// RoleDefault is the terminal's own foreground. It is NoColor in every
	// theme including the coloured ones: text that carries no distinction
	// must inherit whatever the reader chose, and painting it "white" is how
	// a dashboard ends up unreadable on a light terminal.
	RoleDefault Role = iota
	RoleAccent
	RoleActive
	RoleCandidate
	RoleExhausted
	RoleQuarantined
	// RoleMuted is a STATE role, not decoration. The STATE column paints it
	// for disabled, unknown, the empty state and the unrecognised
	// fallthrough, in the same column as quarantined and exhausted, so it
	// belongs to the must-separable set and is gated with the other four.
	RoleMuted
	RoleHeader
	RoleNotice
	RoleGaugeCool
	RoleGaugeOK
	RoleGaugeWarn
	RoleGaugeHigh
	RoleGaugeOver
	// RoleGaugeEmpty is the unfilled track, which is a half-coverage glyph
	// rather than solid ink. Its contrast is therefore the ink blended into
	// the ground and not the ink itself, which is why it carries its own bar.
	RoleGaugeEmpty

	numRoles
)

// UtilizationRole maps a used percentage to five equal visual bands. Every
// percentage remains printed as text; colour is emphasis, never the only copy
// of the value. Values outside the endpoint's ordinary 0..100 range are
// clamped by the comparisons naturally: negative is coolest and 100+ hottest.
func UtilizationRole(pct float64) Role {
	switch {
	case pct < 20:
		return RoleGaugeCool
	case pct < 40:
		return RoleGaugeOK
	case pct < 60:
		return RoleGaugeWarn
	case pct < 80:
		return RoleGaugeHigh
	default:
		return RoleGaugeOver
	}
}

// Name is a theme's name as the user spells it in the config file. It is a
// string rather than an int so that a config round-trip is the identity: the
// value read out of the file is the value compared here, with no table in
// between to disagree with the one in package config.
type Name string

const (
	// Auto is the default and it resolves to Dark or Light only. It does not
	// select ANSI on a low-colour terminal: downgrading truecolor to whatever
	// the terminal has is the writer's job, and the palette is solved against
	// the same conversion functions the writer uses, so the downgrade is safe
	// without a separate theme.
	Auto  Name = "auto"
	Dark  Name = "dark"
	Light Name = "light"
	// ANSI is the sixteen standard slots, symbolic. It carries no RGB at all,
	// so the user's own terminal theme owns every colour. It is opt-in for
	// exactly that reason -- it is the only way to ask for colours this
	// binary did not choose.
	ANSI Name = "ansi"
	// None is every role NoColor, which emits no SGR byte at all. That is not
	// a synonym for "how this binary rendered before colour": the glyph set
	// is an independent key, so None with the Unicode glyphs is a legal
	// combination that no previous release ever produced.
	None Name = "none"
)

// Palette is one theme's colours, one per Role.
//
// The zero value is usable and it is None: every Role answers NoColor. That
// matters because a Palette threaded through a struct literal that predates
// this field must render SOMETHING, and the only safe something is the
// terminal's own foreground.
type Palette struct {
	name Name
	c    [numRoles]color.Color
}

// Of answers the palette a name spells. Auto answers Dark, which is the same
// answer the runtime gives when nothing has told it the background is light.
//
// An unrecognised name answers Dark as well, and deliberately: package config
// validates against Names() before a string ever reaches here, so a name that
// arrives unvalidated is in exactly the position an unset key is in, and the
// unset key's answer is the default. Answering None instead would turn a
// caller's bug into colour silently switching itself off.
func Of(n Name) Palette {
	switch n {
	case Light:
		return light
	case ANSI:
		return ansi16
	case None:
		return Palette{name: None}
	}
	return dark
}

// Pick resolves the configured name against the runtime's background-darkness
// boolean. The boolean is consulted for Auto and for nothing else: a user who
// spelled `light` in the config wants light even on a terminal that reports
// itself dark, because the report is a query some multiplexers eat and the
// config is a sentence somebody typed.
func Pick(configured Name, isDark bool) Palette {
	if configured != Auto {
		return Of(configured)
	}
	if isDark {
		return Of(Dark)
	}
	return Of(Light)
}

// Name is the palette's own name, which is never Auto -- Auto is a request,
// not a palette, and by the time there is a Palette the request is resolved.
func (p Palette) Name() Name {
	if p.name == "" {
		return None
	}
	return p.name
}

// Color answers a role's colour, and answers NoColor for anything it has no
// colour for: the zero Palette, RoleDefault, and a Role value from a newer
// build of some other package. There is no error channel because there is
// nothing a renderer could do with one -- it has a cell to paint either way.
func (p Palette) Color(r Role) color.Color {
	if r < 0 || r >= numRoles || p.c[r] == nil {
		return lipgloss.NoColor{}
	}
	return p.c[r]
}

// Style is Color wrapped in the one thing every caller does with it.
//
// A NoColor role returns a style with no foreground SET, rather than a style
// whose foreground is NoColor. The two are not the same on the wire: the
// second is a colour the writer may still spell out, and the None theme's
// whole contract is that it emits zero escape bytes.
func (p Palette) Style(r Role) lipgloss.Style {
	c := p.Color(r)
	if _, ok := c.(lipgloss.NoColor); ok {
		return lipgloss.NewStyle()
	}
	return lipgloss.NewStyle().Foreground(c)
}

// Names is every name a user may spell, in the order the config help lists
// them. It returns a fresh slice each call so that a caller sorting or
// truncating it cannot reach the package's own copy.
func Names() []Name {
	return []Name{Auto, Dark, Light, ANSI, None}
}

// Parse is the string parser the config validator and the command wiring both
// need, and it is a separate function from Of because the two answer different
// questions.
//
// Of takes a Name and cannot refuse one: an unrecognised name is in the unset
// key's position, and the unset key's answer is the default. That is right for
// a renderer and wrong for a validator. A validator built on Of would accept
// every string ever typed, write "sloarized" into config.toml, and paint dark --
// and the user would read their own typo back out of `ccdad config list` as a
// theme they had chosen.
//
// The match is exact and case-sensitive, against the same five spellings
// Names() lists, because those are the five `ccdad config list` prints and the
// five every other reader in the tree matches. Accepting "Dark" here would put
// a spelling into the file that nothing else in the binary recognises.
func Parse(s string) (Name, bool) {
	switch Name(s) {
	case Auto, Dark, Light, ANSI, None:
		return Name(s), true
	}
	return "", false
}

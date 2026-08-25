package tui

import (
	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/theme"
)

// KeyMap is every binding the dashboard answers to. Add through List are the
// six the keybar advertises; Up through Enter are movement and dismissal,
// shown only in the long help; Start through Restart belong to the daemon
// screen and are not offered anywhere else.
type KeyMap struct {
	Add, Switch, Daemon, Strategy, Quit, List key.Binding
	Up, Down, Refresh, Help, Esc, Enter       key.Binding
	Start, Stop, Restart                      key.Binding
}

// DefaultKeys is the one KeyMap this program has.
func DefaultKeys() KeyMap {
	return KeyMap{
		Add:      key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add")),
		Switch:   key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "switch")),
		Daemon:   key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "daemon")),
		Strategy: key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "strategy")),
		Quit:     key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		List:     key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "list")),

		Up:      key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("up/k", "up")),
		Down:    key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("down/j", "down")),
		Refresh: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		Help:    key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Esc:     key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
		Enter:   key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "select")),

		// The daemon screen's three. Start is disabled under DaemonUnknown by
		// whoever holds that screen's state, not here. Capitalized because
		// lowercase s and r are already Switch and Refresh in this same
		// KeyMap, and FullHelp renders every one of these bindings together:
		// two entries advertising the same key for two different actions is
		// a bug the help view cannot show a user how to work around.
		Start:   key.NewBinding(key.WithKeys("S"), key.WithHelp("S", "start")),
		Stop:    key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "stop")),
		Restart: key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "restart")),
	}
}

// ShortHelp returns the keybar's six, in the order a, s, d, c, q, l. help
// truncates from the right, so this order is a safety property and not a
// preference: losing l costs nothing, because the table already is the list,
// and losing q strands a user in a full-screen program with no advertised way
// out. Being fifth of six rather than sixth is what buys q the room it has --
// it is not a guarantee q survives every width. Measured against the rendered
// bar (see keybar's own tests): q is entirely absent below width 45, and
// present at 45 and every width above it.
func (k KeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Add, k.Switch, k.Daemon, k.Strategy, k.Quit, k.List}
}

// FullHelp groups every binding for the long help view: the keybar's six,
// then movement and dismissal, then the daemon screen's three.
func (k KeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Add, k.Switch, k.Daemon, k.Strategy, k.Quit, k.List},
		{k.Up, k.Down, k.Refresh, k.Help, k.Esc, k.Enter},
		{k.Start, k.Stop, k.Restart},
	}
}

// newHelp is help.New with its two Unicode defaults overridden: ShortSeparator
// is U+2022 and Ellipsis is U+2026 out of the box, and neither is a character
// this line can afford. The separator is drawn between every pair of bindings
// on the one line that tells a user how to leave a full-screen program, and the
// ellipsis is the cut cue -- both land on measured column boundaries, and both
// are ambiguous-width, so on a machine in east-asian width mode they would each
// silently cost a column the footer had already spent.
//
// The cue is the page's own rather than a literal here, because the keybar and
// the header line are cut by the same page and a reader who learns what a cut
// looks like in one place should not have to learn it again in the other.
//
// help.New's default Styles carry truecolor foreground colours, which render as
// SGR escape bytes even though the only difference between a key and its
// description here is which characters they are. This repository's rendered
// output is compared as plain strings -- in the tests that read keybar() and in
// every golden page under testdata -- so the styles are zeroed rather than left
// at their default. A zero lipgloss.Style is exactly what NewStyle() returns,
// so this asks for no styling the same way the library would if asked for none.
//
// pal is taken and NOT read, and it is declared now rather than in the commit
// that reads it. Zeroing those Styles is a colour decision made here and
// nowhere else -- a key and its description are the two roles this line has --
// so the commit that gives the page a palette fills those two fields in and
// moves no caller. A parameter added later is a parameter every call site in
// this package moves for twice, and the reason the glyphs and the colours are
// two commits at all is that a glyph failure which reproduces on one operating
// system has to stay bisectable on its own.
func newHelp(cue string, pal theme.Palette) help.Model {
	h := help.New()
	h.ShortSeparator = "  "
	h.Ellipsis = cue
	h.Styles = help.Styles{}
	return h
}

// keybar renders the help line and then truncates it, because help.SetWidth is
// not a bound. Measured on this 53-column keybar: SetWidth(30) produced 28
// columns, SetWidth(37) produced 53 and SetWidth(45) produced 53. Its
// shouldAddItem returns "add it anyway" when an item overflows and the ellipsis
// also does not fit, and the loop then keeps adding every remaining binding, so
// truncation is non-monotone and overflows. The wrap is the bound.
//
// The tail passed to Truncate is the same argument h.Ellipsis was built from,
// which is what makes the two agree by construction rather than by two literals
// that matched when they were written: help's own ellipsis logic already adds
// the cue at some widths (20, 30) but not at 37 or 45, where shouldAddItem's
// overflow is what this function exists to catch -- an empty tail there cut a
// binding with no visual cue that anything had been cut. Truncating from the
// right means the ORDER of ShortHelp buys q one more rung before that
// truncation reaches it; it is not a guarantee q survives every width, only
// that it is never the first binding lost.
//
// help.Model has no exported Width field, only a private one reached through
// SetWidth, which is why this calls the setter rather than assigning to one.
func keybar(h help.Model, k KeyMap, width int, cue string) string {
	h.SetWidth(width)
	return ansi.Truncate(h.ShortHelpView(k.ShortHelp()), width, cue)
}

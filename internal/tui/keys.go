package tui

import (
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/theme"
)

// KeyMap is every binding the dashboard answers to. The main-page bindings are
// all advertised; Up through Enter are movement and dismissal,
// shown only in the long help; Start through Restart belong to the daemon
// screen and are not offered anywhere else.
type KeyMap struct {
	Add, Switch, Daemon, Strategy, Quit key.Binding
	Up, Down, Refresh, Help, Esc, Enter key.Binding
	Start, Stop, Restart                key.Binding
}

// DefaultKeys is the one KeyMap this program has.
func DefaultKeys() KeyMap {
	return KeyMap{
		Add:      key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add")),
		Switch:   key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "switch")),
		Daemon:   key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "daemon")),
		Strategy: key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "strategy")),
		Quit:     key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),

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

// ShortHelp is every action available on the main page. The renderer wraps the
// complete set; order no longer decides which commands disappear at a narrow
// width.
func (k KeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Add, k.Switch, k.Daemon, k.Strategy, k.Up, k.Down, k.Refresh, k.Help, k.Quit}
}

// FullHelp groups every binding for the long help view: the main-page set,
// then dismissal, then the daemon screen's three.
func (k KeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		k.ShortHelp(),
		{k.Esc, k.Enter},
		{k.Start, k.Stop, k.Restart},
	}
}

// newHelp is help.New with its two non-ASCII defaults overridden and its seven
// styles taken from the palette.
//
// ShortSeparator is U+2022 by default and Ellipsis is U+2026, and both are
// STRING fields on help.Model rather than part of help.Styles. They stay
// overridden and nothing below touches them: they are what keeps the keybar
// 7-bit, and a terminal on a code page that lacks either renders a replacement
// glyph in the middle of the one line telling a user how to leave a full-screen
// program. Both would also land on a measured column boundary while being
// ambiguous-width, so on a machine in east-asian width mode they would each
// silently cost a column the footer had already spent. The ellipsis is the
// page's own cue rather than a literal spelled a second time here, because the
// keybar and the header line are cut by the same page and a reader who learns
// what a cut looks like in one place should not have to learn it again in the
// other.
//
// help.Styles is SEVEN whole lipgloss.Style values and not a set of colour
// fields, so there is no way to tint one without replacing it -- and help.New's
// own defaults carry truecolor foregrounds this package never chose. Each of
// the seven therefore gets a style carrying a Foreground and nothing else -- no
// Width, no Padding, no Transform. help does its own layout: ShortHelpView
// measures each item with lipgloss.Width and decides from that whether the next
// binding fits, and FullHelpView joins its columns horizontally. A Width or a
// Padding set here would be a theme overruling that arithmetic, and the
// footer's own spacing is computed from the bar keybar returns.
//
// Under the None theme every one of the seven is a style with no foreground
// SET, which emits no SGR byte at all -- which is what lets the seven golden
// pages under testdata go on being compared as bytes.
//
// FullKey and FullDesc are rendered by the library over a MULTI-LINE join, and
// that is the one place the right-padding a multi-line Render performs is
// wanted rather than feared: it is what squares the long help's columns off
// before JoinHorizontal puts them side by side.
//
// Keys take the heading role and everything else the muted one. The key is what
// a reader is looking for and the description is what tells them they found the
// right one, so the contrast between the two is the whole content of the line;
// the separators and the ellipsis are punctuation and are quieter than both.
func newHelp(cue string, pal theme.Palette) help.Model {
	h := help.New()
	h.ShortSeparator = "  "
	h.Ellipsis = cue
	h.Styles = help.Styles{
		Ellipsis:       pal.Style(theme.RoleMuted),
		ShortKey:       pal.Style(theme.RoleHeader),
		ShortDesc:      pal.Style(theme.RoleMuted),
		ShortSeparator: pal.Style(theme.RoleMuted),
		FullKey:        pal.Style(theme.RoleHeader),
		FullDesc:       pal.Style(theme.RoleMuted),
		FullSeparator:  pal.Style(theme.RoleMuted),
	}
	return h
}

// keybarLines wraps complete key/description pairs. A binding is never split
// and no binding is dropped, so every available command remains visible at
// every viable dashboard width.
func keybarLines(h help.Model, bindings []key.Binding, width int) []string {
	if width <= 0 {
		return nil
	}
	sep := h.Styles.ShortSeparator.Render(h.ShortSeparator)
	var lines []string
	line := ""
	for _, binding := range bindings {
		if !binding.Enabled() {
			continue
		}
		help := binding.Help()
		item := h.Styles.ShortKey.Render(help.Key) + " " + h.Styles.ShortDesc.Render(help.Desc)
		if line == "" {
			line = item
			continue
		}
		if ansi.StringWidth(line)+ansi.StringWidth(sep)+ansi.StringWidth(item) <= width {
			line += sep + item
			continue
		}
		lines = append(lines, line)
		line = item
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

func keybar(h help.Model, k KeyMap, width int, _ string) string {
	return strings.Join(keybarLines(h, k.ShortHelp(), width), "\n")
}

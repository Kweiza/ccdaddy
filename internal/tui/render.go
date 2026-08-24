package tui

import (
	"fmt"
	"io"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/progress"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// Model is the whole page as data. Body is pure: no Program, no clock, no
// filesystem. Every field it will ever carry is declared here, so the event
// loop that arrives later adds methods to this type and no fields.
type Model struct {
	Snap   view.Snapshot
	Set    ColumnSet
	Width  int
	Height int
	Cursor int // index into Snap.Rows
	Top    int // first visible row, for the scrolling rung of the height ladder
	Gauge  progress.Model
	Help   help.Model
	Keys   KeyMap
}

// newModel is the one place a Model's three library values are built, so a
// second construction site cannot hand the page a differently-configured
// gauge or a keybar with the library's own colours back on.
func newModel(snap view.Snapshot, width, height int) Model {
	return Model{
		Snap:   snap,
		Set:    SetStatus,
		Width:  width,
		Height: height,
		Gauge:  newGauge(),
		Help:   newHelp(),
		Keys:   DefaultKeys(),
	}
}

// oneShotWidth and unboundedHeight are what the non-TTY render picks. A pipe
// has no columns, so no environment variable is consulted and no terminal is
// probed: 80 is the design target, and the height is large enough that the
// height ladder never drops a block. The one-shot output is therefore NOT the
// 80-column fixture below — that one is a page in a 24-row terminal, and this
// one carries the figure block the ladder took away from it.
const (
	oneShotWidth    = 80
	unboundedHeight = 1 << 20
)

// minKeybarWidth is how much room the footer keeps for the keybar before it
// starts shortening the daemon indicator instead.
//
// The rule the ladder states is that the daemon indicator is never truncated
// away and the keybar is what loses bindings — but a footer that spent every
// column on the daemon would advertise no way out of a full-screen program at
// all. Seven columns is "a add" plus the two-character cue that says more was
// cut, which is the least that still answers "what can I press".
const minKeybarWidth = 7

// refreshFailed prefixes the notice a failed reload leaves behind. It is a
// constant because AfterLoad below removes its own previous notice before
// adding a new one, and matching on a prefix is how a page that has failed to
// refresh for an hour still carries exactly one line about it.
const refreshFailed = "could not refresh; these are the last good numbers: "

// AfterLoad folds one Load result into the page.
//
// A failed load KEEPS the previous snapshot and adds one notice. The two
// alternatives are both worse and both silent: emptying the table hides
// accounts that are still there and reads as "you have none", and leaving the
// previous numbers unlabelled presents a reading from before the failure as
// the current one. The notice is the label, and it is what makes keeping the
// numbers honest rather than stale.
//
// A load that succeeds replaces the snapshot whole, which is what takes the
// notice away again — there is no separate clearing step to forget.
func (m Model) AfterLoad(snap view.Snapshot, err error) Model {
	if err == nil {
		m.Snap = snap
		return m
	}
	kept := make([]string, 0, len(m.Snap.Notices)+1)
	for _, n := range m.Snap.Notices {
		if !strings.HasPrefix(n, refreshFailed) {
			kept = append(kept, n)
		}
	}
	m.Snap.Notices = append(kept, refreshFailed+err.Error())
	return m
}

// Body is the whole page as one string.
//
// It reads nothing: the layout comes from Plan, which is a pure function of
// four numbers, and every cell comes from a Snapshot somebody else built. That
// is what makes the fixtures below comparable as whole strings.
func (m Model) Body() string {
	// The table always draws at least one row — an account, or the explicit
	// "no accounts" row below — so the height budget is asked about at least
	// one, never about zero. Budgeting for zero and then drawing one is how a
	// page ends up exactly one row taller than the terminal it was planned
	// for, with nothing reporting it.
	rows := len(m.Snap.Rows)
	if rows < 1 {
		rows = 1
	}
	l := Plan(m.Set, m.Width, m.Height, rows, len(m.Snap.Notices) > 0)
	if l.TooNarrow || l.TooShort {
		return m.floors(l)
	}

	// Width includes the border. A style with a border rendered at Width(20)
	// occupies 20 columns and gives its content 18, so a caller that passes
	// the terminal width to the frame AND subtracts the frame itself takes it
	// off twice — a page two columns narrower than the terminal, with nothing
	// reporting that either.
	inner := m.Width
	if l.Border {
		inner -= 2
	}

	var lines []string
	add := func(rows ...string) {
		for _, r := range rows {
			lines = append(lines, truncate(r, inner))
		}
	}

	switch {
	case l.Wordmark:
		add(wordmark[:len(wordmark)-1]...)
		// The version rides on the wordmark's own last row, which is where
		// the full page puts it and why the title rung costs nothing until
		// the wordmark is already gone.
		last := wordmark[len(wordmark)-1]
		if l.Title {
			last += titleLine(m.Snap.Version)
		}
		add(last)
	case l.Title:
		add(titleLine(m.Snap.Version))
	}
	if l.Blanks {
		add("")
	}
	if l.Tagline {
		add(tagline...)
		add("")
	}
	if l.Figures {
		add(figures...)
		add("")
	}
	if l.Header {
		add(m.headerLine(inner))
	}
	if l.Notice {
		add(noticeLine(m.Snap.Notices, inner))
	}
	lines = append(lines, m.tableBlock(l, inner)...)
	if l.Blanks {
		add("")
	}
	lines = append(lines, m.footer(inner))

	page := strings.Join(lines, "\n")
	if !l.Border {
		return page
	}
	return lipgloss.NewStyle().Border(lipgloss.ASCIIBorder()).Width(m.Width).Render(page)
}

// floors is the page below the minimum viable size: what it needs, and the
// footer, and nothing else. Both messages appear when both floors are under
// water, because a terminal that is too small in one dimension is usually too
// small in the other and naming only one sends the user back for a second try.
func (m Model) floors(l Layout) string {
	var lines []string
	if l.TooNarrow {
		lines = append(lines, truncate("ccdad needs 35 columns", m.Width))
	}
	if l.TooShort {
		lines = append(lines, truncate("ccdad needs 3 rows", m.Width))
	}
	return strings.Join(append(lines, m.footer(m.Width)), "\n")
}

// headerLine is who is live, what the engine is set to, and what it decided.
//
// The Mode clause is present only when the pass Decided. A zero Plan does not
// stringify to nothing — it stringifies to plausible values, and the zero Mode
// is "headroom" — so a line built from a pass that never ran would print a
// real answer nobody computed.
//
// It truncates with a visible cue rather than silently, for the reason the
// keybar does: a line cut mid-value leaves "Strategy: he", which reads as a
// strategy named "he" rather than as a line that did not fit.
func (m Model) headerLine(width int) string {
	line := "Active: " + m.Snap.ActiveLabel + "  |  Strategy: " + m.Snap.Strategy
	if m.Snap.HasMode {
		line += "  |  Mode: " + m.Snap.Mode.String()
	}
	return truncateCue(line, width)
}

// noticeLine is the one line the height ladder gives everything package cli
// would have written to stderr. The most consequential thing it ever carries
// is the hover-thresholds-unreadable warning, and dropping that silently would
// mean the rows are measured against thresholds other than the ones the page
// implies, with no way for a reader to tell.
//
// The count of what did not fit is reserved out of the width BEFORE the text
// is cut, rather than appended after and truncated off: a "(+3 more)" that the
// truncation eats is a promise the line stops keeping at exactly the width
// where it starts mattering.
func noticeLine(notices []string, width int) string {
	const prefix = "note: "
	suffix := ""
	if len(notices) > 1 {
		suffix = fmt.Sprintf("  (+%d more)", len(notices)-1)
	}
	room := width - len(prefix) - len(suffix)
	return truncateCue(prefix+truncateCue(notices[0], room)+suffix, width)
}

// footer is the keybar and the daemon indicator on one line, the indicator
// pushed to the right edge.
//
// The indicator wins every contest for the space: it shortens by dropping its
// own detail (see daemonFooter) and never by being cut off, because "is the
// engine running" is the one fact a dashboard exists to answer, while a
// keybinding a user cannot see is one they can still press.
func (m Model) footer(width int) string {
	if width <= 0 {
		return ""
	}
	foot := m.daemonFooter(width)
	room := width - ansi.StringWidth(foot)
	bar := ""
	if room > 0 {
		bar = keybar(m.Help, m.Keys, room)
	}
	gap := width - ansi.StringWidth(bar) - ansi.StringWidth(foot)
	return truncate(bar+spaces(gap)+foot, width)
}

// daemonFooter is the daemon indicator at the widest form that still leaves
// the keybar room to say something: the full wording first, then the same
// wording without the parenthesised detail, then the bare state with the
// label dropped.
func (m Model) daemonFooter(width int) string {
	for _, form := range []string{
		"Daemon: " + m.daemonPhrase(true),
		"Daemon: " + m.daemonPhrase(false),
		m.daemonPhrase(false),
	} {
		if width-ansi.StringWidth(form) >= minKeybarWidth {
			return form
		}
	}
	return m.daemonPhrase(false)
}

// daemonPhrase is the wording `ccdad daemon status` already prints, rather
// than a fourth one. Three exist in this binary already, and the reason for
// reusing one is the reason for having one: a fourth is a fourth thing to keep
// in step.
//
// The default arm is the unknown state and every future value. "Cannot tell"
// is never folded into "no": a supervisor gating on that folding respawns
// forever on a filesystem where locks do not work.
func (m Model) daemonPhrase(detailed bool) string {
	switch m.Snap.Report.State {
	case daemon.DaemonRunning:
		if detailed {
			return view.DescribeRunning(m.Snap.Report, m.Snap.Now)
		}
		return "running"
	case daemon.DaemonStopped:
		return "not running"
	default:
		return "unknown"
	}
}

// tableBlock is the column header and the account rows, as lines.
//
// It is lipgloss's table rather than the one in bubbles, for two measured
// reasons. This one does per-cell style through a func of (row, column) and
// accounts for width with escape sequences present, so a pre-coloured gauge
// drops into a cell without breaking the alignment of the columns beside it.
// The other has three style fields in total, and a pre-styled cell's own reset
// cancels the selected-row style mid-row, so per-column colour and row
// selection fight each other. What it would have bought is interaction, and
// the cursor this page needs is one integer.
func (m Model) tableBlock(l Layout, inner int) []string {
	shown, more := m.window(l)
	cols := l.Columns
	last := len(cols) - 1

	headers := make([]string, len(cols))
	for i, c := range cols {
		headers[i] = headerName(c)
	}

	data := make([][]string, 0, len(shown)+1)
	for _, r := range shown {
		cells := make([]string, len(cols))
		for i, c := range cols {
			cells[i] = m.cell(c, r, l)
		}
		data = append(data, cells)
	}
	// Zero accounts is a valid state — a fresh install, or every account
	// removed — and it renders as a header plus one explicit row. A bordered
	// box with nothing inside it reads as a dashboard that failed rather than
	// as a store that is empty.
	switch {
	case len(m.Snap.Rows) == 0:
		data = append(data, m.markerRow(cols, l, "no accounts"))
	case more > 0:
		data = append(data, m.markerRow(cols, l, fmt.Sprintf("+%d more  (j/k)", more)))
	}

	t := table.New().
		Border(lipgloss.Border{}).
		BorderTop(false).BorderBottom(false).
		BorderLeft(false).BorderRight(false).
		BorderHeader(false).BorderColumn(false).BorderRow(false).
		// Wrapping is what a bordered box does with content too wide for it,
		// and one wrapped cell costs a row that the height ladder already
		// spent. Every cell here is cut to size before it arrives; this says
		// so to the library as well.
		Wrap(false).
		StyleFunc(func(row, col int) lipgloss.Style {
			return cellStyle(shown, cols, row, col, last)
		}).
		Headers(headers...).
		Rows(data...)

	out := strings.Split(t.String(), "\n")
	for i, line := range out {
		out[i] = truncate(line, inner)
	}
	return out
}

// cellStyle is the table's per-cell style: the state column's own role style,
// and the column gaps.
//
// The gap after the index column is one rather than two, and that is the
// arithmetic the width ladder's own footprints were computed against — the
// index column is three columns of content and four of footprint. Every other
// column carries the standard two, and the last carries none, so the table's
// natural width is where the frame's padding starts rather than a column of
// trailing space the frame then pads again.
func cellStyle(shown []view.Row, cols []Column, row, col, last int) lipgloss.Style {
	st := lipgloss.NewStyle()
	switch {
	case row == table.HeaderRow:
		st = styleHeader
	case row < len(shown) && cols[col] == ColState:
		// The role style comes from the same call that produced the glyph and
		// the word, so a colour can never describe a different state than the
		// text beside it. Every one of these styles is unset today; colour is
		// a later commit's call and this is where it will land.
		_, _, st = stateCell(shown[row].Engine.State)
	}
	switch col {
	case last:
		return st
	case 0:
		return st.PaddingRight(1)
	default:
		return st.PaddingRight(2)
	}
}

// window is which account rows are drawn, and how many are not.
//
// At the scrolling rung the last visible line is spent naming what is off the
// page rather than on one more account: a table that silently stops at the
// bottom of the terminal is one a user reads as complete.
//
// With room for exactly ONE row, that trade inverts and the row wins. Two of
// the ladder's rules meet at that size and disagree: the scrolling rung says
// to show the height minus two and spend the last line on the count, which at
// three rows leaves no account on screen at all, while the never-dropped list
// says at least one account row survives every rung. The list wins, because a
// dashboard with a header, a count of four and no accounts has stopped being a
// dashboard — and j/k, which the count advertises, would have nothing to move
// through. The cost is real and is stated rather than hidden: at exactly three
// rows there is nowhere left to say that more exist.
func (m Model) window(l Layout) (rows []view.Row, more int) {
	all := m.Snap.Rows
	if l.VisibleRows >= len(all) {
		return all, 0
	}
	top := m.Top
	if top < 0 {
		top = 0
	}
	if top > len(all) {
		top = len(all)
	}
	if l.VisibleRows < 2 {
		n := l.VisibleRows
		if top+n > len(all) {
			n = len(all) - top
		}
		return all[top : top+n], 0
	}
	// One line off the visible count, spent on the count itself.
	n := l.VisibleRows - 1
	if top+n > len(all) {
		n = len(all) - top
	}
	return all[top : top+n], len(all) - n
}

// markerRow is a table row that is not an account: the empty-store line, or
// the count of the rows scrolling took away. It carries its text in the
// ACCOUNT column, padded exactly as an account label is, so the column keeps
// the width the layout gave it. Every other cell is empty rather than a dash —
// a dash in these tables means "there is a value here and it could not be
// read", and there is no account here at all.
func (m Model) markerRow(cols []Column, l Layout, text string) []string {
	cells := make([]string, len(cols))
	for i, c := range cols {
		if c == ColAccount {
			cells[i] = accountCell(text, l.AccountWide)
		}
	}
	return cells
}

// cell is one field of one row. Every cell this package draws comes from
// exactly one of these, and each of those is one line over a view.Row method,
// so no percentage, span or absence is spelled twice in this binary.
//
// ACCOUNT is `ccdad list`'s form — the address and the handle — rather than
// `ccdad status`'s alias alone. That is a deliberate divergence: this is the
// column a user reads immediately before pressing a hotkey that can move a
// credential, and an alias-only label leaves someone who has aliased two
// accounts unable to tell which address is which.
func (m Model) cell(c Column, r view.Row, l Layout) string {
	switch c {
	case ColIdx:
		return r.Marker() + " " + idxCell(r)
	case ColAccount:
		return accountCell(r.ListLabel(), l.AccountWide)
	case ColType:
		return typeCell(r)
	case ColUsed:
		if l.Collapsed {
			return usedCellCollapsed(r)
		}
		return usedCell(r, m.Gauge)
	case ColWindow:
		return windowCell(r)
	case ColResets:
		return resetsCell(r, m.Snap.Now)
	case ColState:
		glyph, text, _ := stateCell(r.Engine.State)
		if glyph == "" {
			return text
		}
		return glyph + " " + text
	case ColAuto:
		return autoCell(r)
	case ColTier:
		return tierCell(r)
	case ColLeft:
		return leftCell(r)
	}
	return ""
}

// headerName is the heading each column carries. USED and LEFT are separate
// columns with separate headings on purpose: `status` prints how much is
// spent and `list` prints how much is left, and one heading carrying two
// polarities is the drift the two tables have avoided since they were written.
func headerName(c Column) string {
	switch c {
	case ColIdx:
		return "IDX"
	case ColAccount:
		return "ACCOUNT"
	case ColType:
		return "TYPE"
	case ColUsed:
		return "USED"
	case ColWindow:
		return "WINDOW"
	case ColResets:
		return "RESETS IN"
	case ColState:
		return "STATE"
	case ColAuto:
		return "AUTO"
	case ColTier:
		return "TIER"
	case ColLeft:
		return "LEFT"
	}
	return ""
}

// truncateCue is truncate with a two-character cue that something was cut.
// The keybar already makes this argument for itself: an empty tail cuts a
// value with no sign that anything is missing, and a reader then takes the
// remainder for the whole. It is ASCII for the reason every other string in
// this package is — this binary emits no non-ASCII byte.
func truncateCue(s string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(s, width, "..")
}

// Render is the one-shot, non-TTY page: load, draw at the design target with
// no height ladder, and write it.
//
// A failed load here is fatal rather than a notice, and that is not a second
// rule: AfterLoad keeps the PREVIOUS snapshot, and a one-shot render has none.
// There is nothing to keep and nothing to label.
func Render(o Options) (string, error) {
	snap, err := o.Load(o.Now())
	if err != nil {
		return "", err
	}
	page := newModel(snap, oneShotWidth, unboundedHeight).Body()
	if o.Out != nil {
		if _, err := io.WriteString(o.Out, page+"\n"); err != nil {
			return page, err
		}
	}
	return page, nil
}

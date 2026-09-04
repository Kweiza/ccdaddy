package tui

import (
	"fmt"
	"image/color"
	"io"
	"os"
	"strings"
	"sync"

	"charm.land/bubbles/v2/help"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"

	"github.com/Kweiza/ccdaddy/internal/theme"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// Model is the whole page as data. Body is pure: no Program, no clock, no
// filesystem and no terminal -- everything it draws was read by somebody else
// and handed in.
//
// It was written with the claim that every field it would ever carry was
// already declared, and that claim is now spent: Pal and Glyphs arrived after
// it. They arrived for the reason the claim was made, though, which is the part
// worth keeping. A page cannot ask a terminal what it is able to draw -- asking
// means a console handle, an environment read, or a query written out and
// waited on -- so both answers travel in as data, resolved once by whoever
// built the Options, and the page stays a value that a test can compare as a
// string.
//
// The rule the old sentence was protecting is therefore restated rather than
// dropped: a field here is a FACT THE PAGE WAS TOLD, never one it goes and
// finds. The event loop adds methods to this type; anything that has to be read
// off a machine is read by whoever builds an Options and arrives already
// answered.
type Model struct {
	Snap view.Snapshot
	// Cols is the quota block this fleet needs: one column per window it
	// carries, and one countdown per rollover. It comes from view.ColumnsOf,
	// the same constructor the two CLI tables call, so no surface can grow a
	// column the others do not have.
	Cols   view.Columns
	Set    ColumnSet
	Width  int
	Height int
	Cursor int // index into Snap.Rows
	Top    int // first visible row, for the scrolling rung of the height ladder
	Help   help.Model
	Keys   KeyMap
	// Pal is the colour of every role on this page: the chrome, the header
	// line's labels, the table's cells through cellStyle, the gauge's fill and
	// track per row, and the frame's border.
	//
	// It is a FIELD and not a package-level lookup because lipgloss v2 removed
	// the global renderer -- a Style consults nothing about the terminal on its
	// own -- so the background-darkness answer has to be threaded in from
	// whoever asked the terminal for it. Two things ask, on two paths that
	// cannot share one call: the event loop asks through a Cmd and refines this
	// field when the reply arrives, and the one-shot render asks synchronously
	// through DarkBackground. Both end here.
	//
	// Its zero value is the None palette and that is load-bearing rather than
	// incidental: every role answers NoColor, Palette.Style hands back a style
	// with no foreground SET, and the page emits no SGR byte at all -- which is
	// what lets the seven golden pages under testdata be compared as bytes.
	Pal theme.Palette
	// Glyphs is the vocabulary this page draws with, chosen once per process.
	Glyphs Glyphs
}

// newModel is the one place a Model's library values are built, so a second
// construction site cannot hand the page a differently-configured gauge, a
// keybar with the library's own colours back on, or -- now that there are two
// vocabularies -- a gauge whose fill characters disagree with the frame drawn
// around them.
func newModel(snap view.Snapshot, width, height int, pal theme.Palette, g Glyphs) Model {
	return Model{
		Snap: snap,
		// The same constructor `ccdad status` calls, on the
		// same rows, so all three name the same windows in the same order under
		// the same headers. Computed once per model rather than per frame: it
		// is a function of the snapshot, and the snapshot does not change
		// under a frame.
		Cols:   view.ColumnsOf(snap.Rows),
		Set:    SetFull,
		Width:  width,
		Height: height,
		Pal:    pal,
		Glyphs: g,
		Help:   newHelp(g.Cue, pal),
		Keys:   DefaultKeys(),
	}
}

// paletteFor is Options.Theme as a Palette, with one substitution: the empty
// Name -- what an Options literal nobody filled in carries, and what every test
// in this package that does not care about colour builds -- answers the None
// palette rather than whatever a lookup makes of a name that is not a theme.
//
// Package cli sends the CONFIGURED name, and that name may be theme.Auto -- Of
// answers Dark for Auto, which is the defined default the interactive path then
// refines from the terminal's own reply and the one-shot path resolves for
// itself. What package cli never sends is the EMPTY name, so the substitution
// below is invisible in the shipping binary and load-bearing everywhere else in
// this package. probes() in the event loop builds an App from an Options with
// nothing in it but a clock; a resolver that defaulted the empty name to the
// dark theme would make that page paint, and every golden under testdata would
// redden for a reason that has nothing to do with the page. The empty name
// means "nobody said", and nobody-said renders plain.
func paletteFor(n theme.Name) theme.Palette {
	if n == "" {
		return theme.Of(theme.None)
	}
	return theme.Of(n)
}

// glyphsFor is Options as a glyph set, and it resolves nothing itself: it hands
// PickGlyphs the two facts and lets PickGlyphs decide.
//
// PickGlyphs asks two questions: can this console carry UTF-8, and was this
// process's width engine started in its east-asian mode. They are different
// questions with different answers, and each is answerable in exactly one
// place. The console question means a console handle and a syscall, which
// nothing in this package may make, so package cli asks it and its answer
// arrives as Options.ConsoleUTF8 -- as a FACT, not as a decision. The width
// question is about the engine THIS BINARY measures with, which package cli
// cannot answer for it, so PickGlyphs reads it at call time.
//
// This is why the console answer must not be folded into GlyphSet by the
// caller. Collapse "auto" to "unicode"/"ascii" over there and PickGlyphs's auto
// arm -- the only place the east-asian fallback lives -- never runs in a
// shipping binary: glyphs=auto on a CJK-configured terminal takes the Unicode
// set and draws a frame two columns wider than the page it encloses.
//
// An Options nobody filled in therefore answers the ASCII set, and that is
// right rather than unfortunate. The empty GlyphSet is "nobody said" and the
// false ConsoleUTF8 is "nobody checked", and a page drawn for a console nobody
// has vouched for is drawn in the vocabulary every console has.
func glyphsFor(o Options) Glyphs {
	return PickGlyphs(o.GlyphSet, o.ConsoleUTF8)
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

// noCursor is the value that says nobody is pointing at any row at all.
//
// The cursor CHARACTER is Glyphs.Cursor now, and the rule it used to carry is
// restated rather than lost: the mark lives in the two-column cell in front of
// the index, which is the only column with room and is already spent on the
// live account. Where the two meet, the LIVE ACCOUNT WINS. That mark answers
// "which login would a session get", which is a fact about the credential,
// while the cursor answers "where did I leave the highlight a moment ago",
// which the reader already knows. The cost is that the cursor is invisible
// while it sits on the live row, and it is stated rather than hidden.
//
// noCursor is out of range on purpose. This page is drawn by two callers with
// two meanings -- an event loop, where a cursor is what the switch key opens
// on, and the one-shot below, which writes once into a pipe where nothing is
// selecting anything -- and an index no row can have is how the second one says
// so without Model growing a field to hold the distinction.
const noCursor = -1

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
	// Asked for before the layout is planned, because the summary has one row
	// per fact and its exact size decides how many rows the page needs. The
	// width only moves each row's cut; it never changes the row count. Taking
	// the budget from the returned slice means the height plan and the page
	// below cannot carry separate ideas of how many summary rows exist.
	summary := m.summaryLines(m.Width)
	runway := m.runwayLines()
	footerWidth := m.Width - 2
	if footerWidth < 1 {
		footerWidth = m.Width
	}
	footerRows := len(m.footerLines(footerWidth))
	l := planWithRows(m.Set, m.Cols, m.Width, m.Height, rows,
		len(m.Snap.Notices) > 0, len(runway) > 0, footerRows, len(runway), len(summary))
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
	// The border spends one column at each edge. Re-cut the already-counted
	// rows to the content width so no line can be clipped a second time without
	// the visible cue summaryLines owns.
	summary = m.summaryLines(inner)

	var lines []string
	add := func(rows ...string) {
		for _, r := range rows {
			lines = append(lines, truncate(r, inner))
		}
	}
	// addRole is add with one role over every row it is given.
	//
	// It styles LINE BY LINE, and that is a correctness rule rather than a
	// preference. lipgloss right-pads every short line of a multi-line Render
	// out to the widest line in the block -- Render("a\nbb  \n") comes back as
	// "a   \nbb  \n    " -- so handing it the five-row wordmark in one piece
	// would add trailing spaces to four of its rows and push the frame's right
	// edge out with them. A row at a time cannot do that: there is nothing else
	// in the block for a short line to be padded out to.
	//
	// The cut happens BEFORE the style, so ansi.Truncate is measuring the
	// characters a reader sees and never an escape sequence.
	//
	// Anything whose role is the terminal's own foreground goes through add
	// instead, unstyled. A Style with no foreground set renders a string
	// unchanged, so the two agree -- but a blank spacer row has no role to name
	// and asking for one would be inventing a claim about it.
	addRole := func(role theme.Role, rows ...string) {
		st := m.Pal.Style(role)
		for _, r := range rows {
			lines = append(lines, st.Render(truncate(r, inner)))
		}
	}

	switch {
	case l.Wordmark && m.Glyphs.Art:
		for r := 0; r < wordArt.height()-1; r++ {
			lines = append(lines, m.artRow(wordArt, r, inner, theme.RoleAccent, ""))
		}
		// The version rides on the wordmark's own last row, which is where the
		// full page puts it and why the title rung costs nothing until the
		// wordmark is already gone. On the drawing it rides in CELLS after the
		// art rather than in bytes appended to it -- a pixel row cannot carry
		// concatenated text, but a row being assembled in cell space can.
		tail := ""
		if l.Title {
			tail = titleLine(m.Snap.Version)
		}
		lines = append(lines, m.artRow(wordArt, wordArt.height()-1, inner, theme.RoleAccent, tail))
	case l.Wordmark:
		addRole(theme.RoleAccent, wordmark[:len(wordmark)-1]...)
		// The same rule, in the vocabulary that has no drawing: see the arm
		// above for why the version is here rather than on a row of its own.
		last := wordmark[len(wordmark)-1]
		if l.Title {
			last += titleLine(m.Snap.Version)
		}
		addRole(theme.RoleAccent, last)
	case l.Title:
		addRole(theme.RoleAccent, titleLine(m.Snap.Version))
	}
	if l.Blanks {
		add("")
	}
	if l.Tagline {
		// The tagline is muted and the wordmark is not, and that split is what
		// the accent role is for: the wordmark is what the page IS, and the
		// tagline is a joke told underneath it. Two roles rather than one keeps
		// the eye on the name.
		addRole(theme.RoleMuted, tagline...)
		add("")
	}
	if l.Figures {
		if m.Glyphs.Art {
			for r := 0; r < figureArt.height(); r++ {
				lines = append(lines, m.artRow(figureArt, r, inner, theme.RoleAccent, ""))
			}
		} else {
			addRole(theme.RoleAccent, figures...)
		}
		add("")
	}
	if l.Header {
		add(summary...)
	}
	if l.Runway {
		for i, line := range runway {
			label := "         "
			if i == 0 {
				label = "Runway:  "
			}
			add(truncateCue(label+line, inner, m.Glyphs.Cue))
		}
	}
	if l.Notice {
		addRole(theme.RoleNotice, noticeLine(m.Snap.Notices, inner, m.Glyphs.Cue))
	}
	lines = append(lines, m.tableBlock(l, inner)...)
	if l.Blanks {
		add("")
	}
	lines = append(lines, m.footerLines(inner)...)

	page := strings.Join(lines, "\n")
	if !l.Border {
		return page
	}
	// The border is the one multi-line Render on this page and it is the one
	// place the padding is wanted: a bordered box exists to make every line the
	// same width. BorderForeground paints the frame characters alone and leaves
	// the content's own styling untouched, so the page inside arrives here
	// already correct and comes out still correct.
	//
	// The guard is not defensive tidiness. BorderForeground takes a color.Color
	// rather than a Style, so it cannot make the distinction Palette.Style
	// makes for every other surface here: a border with no foreground SET emits
	// nothing, while a border whose foreground IS NoColor is a colour the
	// writer may still spell out on the wire. The None theme's whole contract
	// is that it emits zero escape bytes, and this is the one call on the page
	// that could break it.
	frame := lipgloss.NewStyle().Border(m.Glyphs.Border).Width(m.Width)
	if c := m.Pal.Color(theme.RoleAccent); !isNoColor(c) {
		frame = frame.BorderForeground(c)
	}
	return frame.Render(page)
}

// floors is the page below the minimum viable size: what it needs, and nothing
// else. Both messages appear when both floors are under
// water, because a terminal that is too small in one dimension is usually too
// small in the other and naming only one sends the user back for a second try.
func (m Model) floors(l Layout) string {
	var lines []string
	if l.TooNarrow {
		lines = append(lines, truncate("ccdad needs 35 columns", m.Width))
	}
	if l.TooShort {
		lines = append(lines, truncate(fmt.Sprintf("ccdad needs %d rows", l.FooterRows+2), m.Width))
	}
	return strings.Join(lines, "\n")
}

// runwayLines is the measured-burn summary this page draws, one axis or
// supporting fact per row.
// nothing to say.
//
// Two gates, and neither subsumes the other. HasForecast says whether the
// caller produced a forecast at all; view.RunwayLine's empty string says
// whether the one it produced has anything worth a line, which it does not for
// a fleet with no basis. Dropping the flag would draw a line off a Fleet the
// Snapshot never claimed -- a value left in the struct by an earlier load --
// and dropping the string would draw the label with nothing after it.
//
// The zone comes off Snap.Now rather than from time.Local, and that is not a
// convenience. Nothing in this package reads the environment: that is what
// makes the whole page a pure function of a Snapshot somebody else built, and
// what lets the fixtures be compared as whole strings. Snap.Now was read by the
// caller with the clock nearest the reader, so its location is the reader's
// own. A page that resolved the zone itself would print one hour in the
// author's terminal and another in CI, where nothing sets TZ, and no fixture
// could pin either.
func (m Model) runwayLines() []string {
	if !m.Snap.HasForecast {
		return nil
	}
	return view.CompactRunwayLines(m.Snap.Forecast, m.Snap.Now, m.Snap.Now.Location())
}

// summaryLines is who is live, what the engine is set to, and what it decided.
//
// Every fact owns one line. In particular, Claude and Codex are separate
// active facts: a long account label may be cut, but it cannot consume either
// provider beside it or push Strategy and Current off the right edge.
//
// The Strategy line is Snap.StrategyLabel() and never Snap.Strategy: under
// hover the configured strategy has stopped being read, and naming it here made
// a page under a fully automatic mode look exactly like one that was not.
//
// The Current line is present only when the pass Decided. A zero Plan does not
// stringify to nothing — it stringifies to plausible values, and the zero Mode
// is "headroom" — so a line built from a pass that never ran would print a
// real answer nobody computed.
//
// Each line truncates with a visible cue rather than silently, for the reason
// the keybar does: a line cut mid-value leaves "Strategy: he", which reads as
// a strategy named "he" rather than as a line that did not fit.
func (m Model) summaryLines(width int) []string {
	lab := m.Pal.Style(theme.RoleHeader)
	lines := []string{
		lab.Render("Active (Claude): ") + m.Snap.ActiveLabel,
	}
	if m.Snap.CodexServingLabel != "" {
		lines = append(lines, lab.Render("Active (Codex): ")+m.Snap.CodexServingLabel)
	}
	lines = append(lines, lab.Render("Strategy: ")+m.Snap.StrategyLabel())
	if m.Snap.HasMode {
		lines = append(lines, lab.Render("Current: ")+m.Snap.Mode.String())
	}
	// The LABELS take the heading role and the answers take none, which is the
	// rule the table one block down already pays: the column headings carry
	// RoleHeader and the cells underneath carry their own role or the
	// terminal's foreground. Painting the whole line as one span would make the
	// fleet's three answers the loudest thing on the page and would make
	// "Active:" and the address it names read as one word.
	//
	// Every label is styled before its answer is appended and each completed
	// line is cut afterwards, so the cut is ANSI-aware and the cue truncateCue
	// leaves behind lands outside the style rather than inside it.
	for i := range lines {
		lines[i] = truncateCue(lines[i], width, m.Glyphs.Cue)
	}
	return lines
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
func noticeLine(notices []string, width int, cue string) string {
	const prefix = "note: "
	suffix := ""
	if len(notices) > 1 {
		suffix = fmt.Sprintf("  (+%d more)", len(notices)-1)
	}
	// Display columns and not bytes. The notice's own text is whatever the
	// caller wrote -- the runway wording alone already carries a multi-byte
	// separator -- so a reservation measured in bytes takes more room away than
	// the label occupies, and the "(+3 more)" it was protecting is cut off at
	// exactly the width where it starts mattering.
	//
	// It is also why nothing paints INSIDE this function: the caller styles the
	// line whole, after the reservation and both cuts, so no escape sequence is
	// ever in front of a measurement here.
	room := width - ansi.StringWidth(prefix) - ansi.StringWidth(suffix)
	return truncateCue(prefix+truncateCue(notices[0], room, cue)+suffix, width, cue)
}

// footerLines shows every main-page binding, wrapped to the available width.
// Daemon health remains one keystroke away on the daemon screen; keeping it on
// this line would consume a second row even when every command fits on one.
func (m Model) footerLines(width int) []string {
	if width <= 0 {
		return nil
	}
	return keybarLines(m.Help, m.Keys.ShortHelp(), width)
}

// moreLabel is the row that names what the window is not showing, and which way
// it lies.
//
// The direction is not decoration. window() slices the rows it was given, so
// what is off the page can be above the window, below it, or both at once --
// and the count alone reads as "below" to every reader, which is wrong for
// anyone who has pressed j to the bottom of a long list. The two marks are
// ASCII in both glyph sets for the reason the cut cue is: this string is drawn
// inside the ACCOUNT column at its measured width, and the two arrows that
// would read better here cost two columns on a machine in east-asian mode.
func moreLabel(g Glyphs, top, shown, total, more int) string {
	cue := g.MoreBelow
	switch {
	case top > 0 && top+shown < total:
		cue = g.MoreAbove + g.MoreBelow
	case top > 0:
		cue = g.MoreAbove
	}
	return fmt.Sprintf("%s +%d more  (j/k)", cue, more)
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
		headers[i] = headerName(c, m.Cols)
	}

	data := make([][]string, 0, len(shown)+1)
	for at, r := range shown {
		cells := make([]string, len(cols))
		for i, c := range cols {
			// The row's own index in Snap.Rows, not its index in the window:
			// the cursor is a position in the table and the window is a view
			// onto it, and the two differ by Top the moment scrolling starts.
			cells[i] = m.cell(c, r, l, m.Cols, m.Top+at)
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
		data = append(data, m.markerRow(cols, l, moreLabel(m.Glyphs, m.Top, len(shown), len(m.Snap.Rows), more)))
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
			return cellStyle(m.Glyphs, m.Pal, shown, cols, m.Cols, row, col, last)
		}).
		Headers(headers...).
		Rows(data...)

	out := strings.Split(t.String(), "\n")
	for i, line := range out {
		out[i] = truncate(line, inner)
	}
	return out
}

// cellStyle is the table's per-cell style: the state column's own role, the
// muted marker rows, the column headings, and the column gaps.
//
// Three kinds of row reach here and each answers differently. The heading row
// is not about any account. An account row takes the role of the state it
// prints, from the SAME call that produced the glyph and the word, so a colour
// can never describe a different state than the text beside it. And a marker
// row -- "no accounts", "+3 more  (j/k)" -- is this package's own sentence
// about the table rather than about anybody's account, so it takes RoleMuted
// across every column. It was previously excluded from the state arm by the
// `row < len(shown)` bound and then fell out of the switch with no style at
// all, which left the empty-store line wearing the terminal's default
// foreground in a table where everything around it is painted.
//
// The glyph set is passed through and only the third return is read. It is
// taken rather than defaulted because stateCell is one function with one
// signature and a second call site spelling its own vocabulary is exactly the
// drift the set exists to remove -- even here, where the vocabulary cannot
// reach the output.
//
// The style is built from the palette on every call and never cached. Palette
// stores colours, so Style hands back a fresh lipgloss.Style each time and the
// PaddingRight below cannot reach back into the theme and widen a role for
// every other caller.
//
// The gap after the index column is one rather than two, and that is the
// arithmetic the width ladder's own footprints were computed against — the
// index column is three columns of content and four of footprint. Every other
// column carries the standard two, and the last carries none, so the table's
// natural width is where the frame's padding starts rather than a column of
// trailing space the frame then pads again.
func cellStyle(g Glyphs, pal theme.Palette, shown []view.Row, cols []Column,
	block view.Columns, row, col, last int) lipgloss.Style {

	st := lipgloss.NewStyle()
	switch {
	case row == table.HeaderRow:
		st = pal.Style(theme.RoleHeader)
	case row >= len(shown):
		st = pal.Style(theme.RoleMuted)
	case cols[col].Kind == ColState:
		_, _, role := stateCell(g, shown[row].Engine.State)
		st = pal.Style(role)
	case cols[col].Kind == ColWindow:
		// The row of percentages IS the gauge, read across, and this is what
		// makes it one: each cell is coloured for its own window, against the
		// threshold that window was measured with.
		//
		// A single bar could not survive the change. It was seventeen columns
		// of one window, and which window was the derivation this table exists
		// to stop making -- three windows would be fifty-one columns of bar, and
		// one bar would be the derived window back again under a new name.
		//
		// The band is allowed HERE and refused on the two CLI tables, and the
		// difference is the STATE column beside it: colour is never the only
		// thing carrying a distinction, and on this page the account's verdict
		// has a word of its own. Neither CLI table has one at any width.
		st = pal.Style(cellRole(shown[row], block.Windows[cols[col].Win].Name))
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
		if c.Kind == ColAccount {
			cells[i] = accountCell(text, l.AccountWide, m.Glyphs.Cue)
		}
	}
	return cells
}

// cell is one field of one row. Every cell this package draws comes from
// exactly one of these, and each of those is one line over a view.Row method,
// so no percentage, span or absence is spelled twice in this binary.
//
// ACCOUNT is the address-and-handle form `ccdad status` also uses. This is the
// column a user reads immediately before pressing a hotkey that can move a
// credential, and an alias-only label leaves someone who has aliased two
// accounts unable to tell which address is which.
func (m Model) cell(c Column, r view.Row, l Layout, cols view.Columns, at int) string {
	switch c.Kind {
	case ColIdx:
		return m.markerCell(r, at) + " " + idxCell(r)
	case ColAccount:
		return accountCell(r.ListLabel(), l.AccountWide, m.Glyphs.Cue)
	case ColType:
		return typeCell(r)
	case ColWindow:
		return r.WindowCell(cols.Windows[c.Win].Name)
	case ColReset:
		return r.ResetCell(cols.Resets[c.Win], m.Snap.Now)
	case ColWorst:
		// The whole block in one cell, for a terminal too narrow to carry it.
		// It names the window as well as the number, because a percentage with
		// no window beside it is the thing this table stopped doing.
		return worstCell(r, cols)
	case ColState:
		glyph, text, _ := stateCell(m.Glyphs, r.Engine.State)
		if glyph == "" {
			return text
		}
		return glyph + " " + text
	case ColAuto:
		return autoCell(r)
	case ColTier:
		return tierCell(r)
	case ColAge:
		return r.AgeLabel(m.Snap.Now)
	}
	return ""
}

// markerCell is the one column in front of the index: the live account, the
// row the cursor is on, or neither.
//
// See noCursor above for why the live account wins where they meet, and why a
// page nobody is pointing at draws no cursor at all.
func (m Model) markerCell(r view.Row, at int) string {
	if !r.Active && at == m.Cursor {
		return m.Glyphs.Cursor
	}
	return r.Marker()
}

// headerName is the heading each dashboard column carries.
func headerName(c Column, cols view.Columns) string {
	switch c.Kind {
	case ColIdx:
		return "IDX"
	case ColAccount:
		return "ACCOUNT"
	case ColType:
		return "TYPE"
	case ColTier:
		return "TIER"
	case ColWindow:
		return cols.Windows[c.Win].Header
	case ColReset:
		return cols.Resets[c.Win].Header
	case ColWorst:
		return "WORST"
	case ColState:
		return "STATE"
	case ColAuto:
		return "AUTO"
	case ColAge:
		return "AGE"
	}
	return ""
}

func truncateCue(s string, width int, cue string) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(s, width, cue)
}

// isNoColor is whether a palette answered a role with "the terminal's own
// foreground". Palette.Style answers it for every caller that wants a Style;
// this is for the one caller that wants a colour.
func isNoColor(c color.Color) bool {
	_, ok := c.(lipgloss.NoColor)
	return ok
}

// DarkBackground answers whether this process's terminal has a dark
// background, for the one-shot page below and for nothing else. Every clause of
// it is a bound rather than a preference.
//
// It asks only when stdin AND stdout are terminals, and that guard has to be
// the CALLER'S. On Windows lipgloss opens CONIN$ and CONOUT$ explicitly when
// stdio is redirected, so the library will not decline a query into a pipe, and
// `ccdad status > file` from a scheduled task would sit in raw mode waiting for
// a reply from nobody.
//
// It asks at most once per process, because one ask is already the most
// expensive line in this binary. lipgloss's BackgroundColor loops over stdin
// AND stdout and runs both legs even where they are the same file -- there is
// no in == out guard to fall through -- at a two-second timeout each, so a
// single call puts stdin into raw mode with a deferred restore and blocks FOUR
// seconds on a terminal that answers neither OSC 11 nor DA1. Twice is eight.
//
// It answers dark when it did not ask, which is the same default the
// interactive half takes when a multiplexer eats the reply. A default that is
// DEFINED rather than awaited is what keeps the two surfaces from disagreeing
// on a terminal that never answers.
//
// It is a package var because no test has a terminal, and it is EXPORTED with
// no reader outside this package left, which is a state worth explaining
// instead of tidying. Package cli's one-shot tables did read it, on the
// argument that one cache beats two; the argument was wrong on its own numbers,
// because a sync.OnceValue is once per PROCESS and every one of those listings
// is its own process, so `ccdad status` paid the full four seconds under the
// default theme on a terminal that never answers. They take the same defined
// dark default this function returns when it declines to ask, and the thing
// that keeps them off this name is a syntax walk in that package which spells
// the name out. Unexporting would make that walk unreadable -- a ban on
// something the compiler already refuses teaches nobody what the cost was --
// and it would be a change to this page's own mechanism made for a reason that
// belongs to a different one. It is never reached from the daemon, which
// renders nothing.
var DarkBackground = sync.OnceValue(func() bool {
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return true
	}
	return lipgloss.HasDarkBackground(os.Stdin, os.Stdout)
})

// isTerminal is this package's own copy of the test package cli spells as isTTY
// in tty.go, and a copy rather than an import: package cli imports package tui
// to register the dashboard command, so the reverse import is a cycle and not a
// choice.
func isTerminal(f *os.File) bool {
	return f != nil && term.IsTerminal(int(f.Fd()))
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
	// theme=auto is resolved HERE and nowhere earlier. Package cli hands over
	// the configured word, and this is the branch the query is scoped to: a
	// redirected page has no Update to answer a BackgroundColorMsg, so this is
	// its only chance to ask, and resolving in tuiOptions would have moved the
	// FOUR-second wait onto the interactive branch instead -- the one path that
	// must not block. Four and not two, because the library runs the query
	// against stdin and then against stdout, two seconds a leg.
	//
	// In a shipping binary this line has never actually paid it, and that is
	// worth knowing before anyone reaches for the same call somewhere cheaper
	// looking. runTui reaches Render only when stdout or stdin is NOT a
	// terminal, and DarkBackground declines to ask unless both are, so the
	// guard has closed on every invocation that has ever got here. The ask
	// stays rather than being cut, because the guard belongs to DarkBackground
	// and the caller set is not this function's to freeze -- a second caller
	// with two terminals and no event loop is exactly the case it is written
	// for -- but nobody should read this as a cost the redirected page is
	// paying today.
	//
	// paletteFor stays the answer for every other name INCLUDING the empty one,
	// and the guard is why. theme.Pick resolves an unrecognised name -- the
	// empty one included -- to Dark, and the empty name is what an Options
	// nobody filled in carries; asking Pick unconditionally would make the
	// unthemed page paint and redden every golden under testdata.
	pal := paletteFor(o.Theme)
	if o.Theme == theme.Auto {
		pal = theme.Pick(theme.Auto, DarkBackground())
	}
	m := newModel(snap, oneShotWidth, unboundedHeight, pal, glyphsFor(o))
	// Nobody is pointing at anything in a pipe. Without this the first row of
	// every redirected render would carry a selection marker put there by a
	// reader who is not present.
	m.Cursor = noCursor
	page := m.Body()
	if o.Out != nil {
		if _, err := io.WriteString(o.Out, page+"\n"); err != nil {
			return page, err
		}
	}
	return page, nil
}

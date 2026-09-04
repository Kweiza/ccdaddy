package tui

import (
	"bytes"
	"image/color"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/theme"
)

// The role style comes from the same call that produced the glyph and the word,
// so a colour can never describe a different state than the text beside it.
// This asserts the pairing through cellStyle rather than through stateCell,
// because cellStyle is where the row index decides which of the three kinds of
// row is being painted.
func TestTheStateColumnTakesTheRoleOfTheStateItPrints(t *testing.T) {
	pal := theme.Of(theme.Dark)
	shown := fixtureRows()
	cols := []Column{col(ColIdx), col(ColAccount), col(ColState)}
	const state = 2

	for at, want := range map[int]theme.Role{
		0: theme.RoleActive,
		1: theme.RoleExhausted,
		2: theme.RoleCandidate,
		3: theme.RoleMuted,
	} {
		got := cellStyle(UnicodeGlyphs, pal, shown, cols, testCols(), at, state, len(cols)-1).GetForeground()
		if got != pal.Color(want) {
			t.Errorf("row %d's STATE cell takes %v, want %v (%v)", at, got, pal.Color(want), want)
		}
	}
}

// A marker row is not an account. "no accounts" and "+3 more  (j/k)" are this
// package's own sentences about the TABLE, and painting them in whatever role
// the row underneath happens to carry would make the empty-store line green on
// one machine and red on the next. Before this they were excluded from the
// state arm by a `row < len(shown)` bound and then fell through to no style at
// all -- the terminal's default foreground, in a table where everything around
// them is painted.
func TestAMarkerRowIsMutedAndNotWhateverTheRowsAroundItAre(t *testing.T) {
	pal := theme.Of(theme.Dark)
	cols := []Column{col(ColIdx), col(ColAccount), col(ColState)}

	// The scrolling marker: three rows shown, the marker at index 3.
	for col := range cols {
		got := cellStyle(UnicodeGlyphs, pal, fixtureRows()[:3], cols, testCols(), 3, col, len(cols)-1).GetForeground()
		if got != pal.Color(theme.RoleMuted) {
			t.Errorf("the +N-more marker's column %d takes %v, want RoleMuted", col, got)
		}
	}
	// The empty-store marker: no rows shown at all, the marker at index 0.
	for col := range cols {
		got := cellStyle(UnicodeGlyphs, pal, nil, cols, testCols(), 0, col, len(cols)-1).GetForeground()
		if got != pal.Color(theme.RoleMuted) {
			t.Errorf("the no-accounts marker's column %d takes %v, want RoleMuted", col, got)
		}
	}
}

// The column headings are the one row in the table that is not about any
// account, and they were already the one row cellStyle treated separately.
func TestTheColumnHeadingsTakeTheHeaderRole(t *testing.T) {
	pal := theme.Of(theme.Dark)
	cols := []Column{col(ColIdx), col(ColAccount), col(ColState)}
	got := cellStyle(UnicodeGlyphs, pal, fixtureRows(), cols, testCols(), table.HeaderRow, 0, len(cols)-1).GetForeground()
	if got != pal.Color(theme.RoleHeader) {
		t.Fatalf("the heading row takes %v, want RoleHeader", got)
	}
}

// The gaps are the width ladder's own arithmetic and a palette may not move
// them: one column after IDX, two after every other, none after the last.
func TestPaintingACellDoesNotMoveItsGap(t *testing.T) {
	cols := []Column{col(ColIdx), col(ColAccount), col(ColState)}
	for _, pal := range []theme.Palette{theme.Of(theme.None), theme.Of(theme.Dark)} {
		for col, want := range map[int]int{0: 1, 1: 2, 2: 0} {
			got := cellStyle(UnicodeGlyphs, pal, fixtureRows(), cols, testCols(), 0, col, len(cols)-1).GetPaddingRight()
			if got != want {
				t.Errorf("under %v, column %d pads %d, want %d", pal.Name(), col, got, want)
			}
		}
	}
}

// help.Styles is SEVEN whole lipgloss.Style values and not a set of colour
// fields, so any one left at its zero value is a surface that silently keeps
// the terminal's default foreground while everything beside it is painted. The
// test names all seven, because the way this goes wrong is somebody filling the
// four the short help uses and never opening the [?] screen.
//
// Each style carries a Foreground and NOTHING else. A Width, a Padding or a
// Transform on any of these would be help's own layout arithmetic overruled by
// a theme: ShortHelpView measures each item with lipgloss.Width and decides
// from that whether the next binding fits, FullHelpView joins its columns
// horizontally, and the footer's spacing is computed from the bar keybar
// returns.
func TestAllSevenHelpStylesArePaintedAndCarryNothingButAColour(t *testing.T) {
	h := newHelp(UnicodeGlyphs.Cue, theme.Of(theme.Dark))
	for name, st := range map[string]lipgloss.Style{
		"Ellipsis":       h.Styles.Ellipsis,
		"ShortKey":       h.Styles.ShortKey,
		"ShortDesc":      h.Styles.ShortDesc,
		"ShortSeparator": h.Styles.ShortSeparator,
		"FullKey":        h.Styles.FullKey,
		"FullDesc":       h.Styles.FullDesc,
		"FullSeparator":  h.Styles.FullSeparator,
	} {
		if st.GetForeground() == (lipgloss.NoColor{}) {
			t.Errorf("help.Styles.%s carries no colour under the Dark theme", name)
		}
		if w := st.GetWidth(); w != 0 {
			t.Errorf("help.Styles.%s sets Width(%d); help does its own layout", name, w)
		}
		if l, r := st.GetPaddingLeft(), st.GetPaddingRight(); l != 0 || r != 0 {
			t.Errorf("help.Styles.%s pads (%d,%d); help does its own spacing", name, l, r)
		}
	}
}

// The two string fields the keybar's 7-bit invariant rests on are NOT part of
// help.Styles and must survive it. ShortSeparator and Ellipsis live on
// help.Model, and help.New sets them to U+2022 and U+2026 -- so a Styles
// assignment that also reset the model would put a replacement glyph in the
// middle of the one line telling a user how to leave a full-screen program.
func TestPaintingTheKeybarDoesNotPutBackHelpsOwnNonAsciiStrings(t *testing.T) {
	h := newHelp(UnicodeGlyphs.Cue, theme.Of(theme.Dark))
	if h.ShortSeparator != "  " {
		t.Errorf("ShortSeparator is %q", h.ShortSeparator)
	}
	if h.Ellipsis != UnicodeGlyphs.Cue {
		t.Errorf("Ellipsis is %q, want the page's own cue %q", h.Ellipsis, UnicodeGlyphs.Cue)
	}
}

// Colour costs the keybar no columns and moves no binding. Measured at every
// width the ladder reaches: the painted bar strips back to the plain bar byte
// for byte and measures the same number of display columns.
func TestPaintingTheKeybarChangesNoWidthAndDropsNoBinding(t *testing.T) {
	k := DefaultKeys()
	plain := newHelp(UnicodeGlyphs.Cue, theme.Of(theme.None))
	dark := newHelp(UnicodeGlyphs.Cue, theme.Of(theme.Dark))
	for _, w := range []int{20, 30, 37, 45, 53, 80, 113} {
		a := keybar(plain, k, w, UnicodeGlyphs.Cue)
		b := keybar(dark, k, w, UnicodeGlyphs.Cue)
		if ansi.Strip(b) != a {
			t.Errorf("at width %d the painted keybar strips to %q, want %q", w, ansi.Strip(b), a)
		}
		if ansi.StringWidth(a) != ansi.StringWidth(b) {
			t.Errorf("at width %d the keybar is %d columns plain and %d painted",
				w, ansi.StringWidth(a), ansi.StringWidth(b))
		}
	}
}

// darkModel is the fixture page with a real palette on it.
//
// fixtureModel is deliberately the None theme, because that is what the seven
// golden files under testdata are written against and detection must never
// reach them, so every assertion about colour needs a second constructor rather
// than a mutated copy of the first. The keybar is rebuilt too: help.Model holds
// its seven styles BY VALUE, so a Model whose palette was swapped after
// construction would draw a coloured page under a colourless keybar.
func darkModel(width, height int) Model {
	m := fixtureModel(width, height)
	m.Pal = theme.Of(theme.Dark)
	m.Help = newHelp(m.Glyphs.Cue, m.Pal)
	return m
}

// sgr writes a rendered page through a NAMED colour profile.
//
// At TrueColor this is a straight pass-through in colorprofile's own Write, and
// that is the point rather than an oversight. lipgloss v2 removed the global
// renderer: Render emits truecolor unconditionally and downsampling is the
// WRITER's job, so the bytes below are a fixed answer that does not depend on
// the terminal go test happens to run in. Naming the profile is what makes that
// explicit, and changing which depth these bytes are asserted at is one word
// here rather than a rewrite of every assertion.
func sgr(t *testing.T, page string, p colorprofile.Profile) string {
	t.Helper()
	var buf bytes.Buffer
	w := &colorprofile.Writer{Forward: &buf, Profile: p}
	if _, err := io.WriteString(w, page); err != nil {
		t.Fatalf("writing the page through a %v writer: %v", p, err)
	}
	return buf.String()
}

// opener is the SGR sequence a role opens with: the bytes lipgloss writes in
// front of a string styled with it. It is taken FROM the palette rather than
// typed out as a hex triple, because a test that spelled #f09574 for itself
// would be asserting that somebody typed the same number twice, and would go on
// passing if the palette moved and the page did not.
func opener(pal theme.Palette, r theme.Role) string {
	open, _, _ := strings.Cut(pal.Style(r).Render("x"), "x")
	return open
}

// Once the ladder tests compare against a colourless fixture, NOTHING else in
// this package catches a role painted on the wrong cell: a page with every
// colour swapped renders to the same stripped bytes as a correct one. This is
// that test, and it names every pair it asserts so that a role which quietly
// stopped being applied shows up here as a named failure rather than as a page
// that looks fine.
func TestEachRoleLandsOnTheCellItNames(t *testing.T) {
	pal := theme.Of(theme.Dark)
	m := darkModel(113, 29)
	page := sgr(t, m.Body(), colorprofile.TrueColor)
	open := func(r theme.Role) string { return opener(pal, r) }

	for _, tc := range []struct{ name, want string }{
		{"the wordmark's art", open(theme.RoleAccent) + string(artUpper)},
		{"the version on the art's last row", open(theme.RoleAccent) + "ccdad "},
		{"the tagline", open(theme.RoleMuted) + tagline[0]},
		{"the summary's active label", open(theme.RoleHeader) + "Active (Claude): "},
		{"the column headings", open(theme.RoleHeader) + "IDX"},
		{"an active row's state", open(theme.RoleActive) + m.Glyphs.Active + " active"},
		{"an exhausted row's state", open(theme.RoleExhausted) + m.Glyphs.Exhausted + " exhausted"},
		{"a candidate row's state", open(theme.RoleCandidate) + m.Glyphs.Candidate + " candidate"},
		{"an unread row's state", open(theme.RoleMuted) + m.Glyphs.Unknown + " unknown"},
		// The gauge is gone: it was seventeen columns of ONE window, and which
		// window was the derivation this table stopped making. The row of
		// percentages is the gauge now, read across, and the same three roles
		// land on the cells instead of on a bar.
		{"a spent window's cell", open(theme.RoleGaugeOver)},
		{"a middle window's cell", open(theme.RoleGaugeWarn)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(page, tc.want) {
				t.Fatalf("the page does not carry %q:\n%s", tc.want, ansi.Strip(page))
			}
		})
	}

	// The summary's VALUES are not inside its labels' spans. The table pays
	// the same rule one block down -- the headings carry RoleHeader and the
	// cells underneath carry their own role or none -- and a summary line
	// painted as one span would make the fleet's answers louder than any row.
	if joined := open(theme.RoleHeader) + "Active (Claude): " + m.Snap.ActiveLabel; strings.Contains(page, joined) {
		t.Errorf("a summary line is painted as one span, so the label and the answer read alike")
	}

	// The frame is the page's outermost accent and the top border is the first
	// thing rendered, so it is the first line that has to carry it.
	if top := strings.Split(page, "\n")[0]; !strings.Contains(top, open(theme.RoleAccent)) {
		t.Errorf("the frame's top border is not painted: %q", top)
	}
}

// The note line takes a role of its own, and it takes it at the rung the height
// ladder actually keeps one: the notice is dropped SECOND, so 80x20 is the
// shortest fixture that renders it and 80x13 would pin its absence.
func TestTheNoticeLineIsPaintedAsANotice(t *testing.T) {
	pal := theme.Of(theme.Dark)
	m := darkModel(80, 21)
	m.Snap.Notices = []string{"hover thresholds could not be read"}
	page := sgr(t, m.Body(), colorprofile.TrueColor)
	if want := opener(pal, theme.RoleNotice) + "note: "; !strings.Contains(page, want) {
		t.Fatalf("the notice is not painted as one:\n%s", ansi.Strip(page))
	}
}

// The empty-store line is the table's own sentence and it is muted, whatever
// the accounts around it are doing -- there are none.
func TestTheMarkerRowIsPaintedMutedOnThePage(t *testing.T) {
	pal := theme.Of(theme.Dark)
	m := darkModel(80, 13)
	m.Snap.Rows = nil
	page := sgr(t, m.Body(), colorprofile.TrueColor)
	if want := opener(pal, theme.RoleMuted) + "no accounts"; !strings.Contains(page, want) {
		t.Fatalf("the empty-store line is not muted:\n%s", ansi.Strip(page))
	}
}

// Exit 3 is "the world is already how you asked for it" and is NOT a failure.
// A dashboard that painted every non-zero code red would tell a user something
// went wrong when the account they picked was simply the one already live --
// and that is the single most common way this panel is ever opened.
//
// It takes the muted role rather than the exhausted one, and rather than the
// active one: nothing happened, which is neither a success to celebrate nor a
// refusal to look at.
func TestExitThreeIsNotPaintedAsAFailure(t *testing.T) {
	pal := theme.Of(theme.Dark)
	for _, tc := range []struct {
		code int
		want theme.Role
	}{
		{exitOK, theme.RoleActive},
		{exitNothingToDo, theme.RoleMuted},
		{exitBlocked, theme.RoleExhausted},
		{7, theme.RoleExhausted},
	} {
		if got := (result{Code: tc.code}).verdictRole(); got != tc.want {
			t.Errorf("exit %d takes %v, want %v", tc.code, got, tc.want)
		}
	}
	if (result{Code: exitNothingToDo}).verdictRole() == (result{Code: exitBlocked}).verdictRole() {
		t.Error("exit 3 and exit 4 are painted alike, so the panel calls a no-op a refusal")
	}

	body := run(func([]string) (int, string, string) { return exitNothingToDo, "", "" },
		[]string{"switch", "uuid-a"}).Body(60, pal)
	if !strings.Contains(body, opener(pal, theme.RoleMuted)+"nothing to do") {
		t.Errorf("the exit-3 verdict is not muted: %q", body)
	}
	if strings.Contains(body, opener(pal, theme.RoleExhausted)) {
		t.Errorf("the exit-3 panel carries the exhausted role somewhere: %q", body)
	}
}

// The verdict is the ONLY painted line in the panel. The command line above it
// is what the user asked for and the output below it is somebody else's bytes;
// painting captured stderr from a vocabulary this package chose would be the
// panel rewriting a message, which is the one thing it exists not to do.
func TestThePanelPaintsItsVerdictAndNothingElse(t *testing.T) {
	pal := theme.Of(theme.Dark)
	r := run(func([]string) (int, string, string) {
		return exitBlocked, "some stdout\n", "some stderr\n"
	}, []string{"switch", "uuid-a"})
	lines := strings.Split(r.Body(80, pal), "\n")
	if len(lines) != 4 {
		t.Fatalf("the panel drew %d lines, want the command, the verdict and both streams: %q", len(lines), lines)
	}
	for i, line := range lines {
		if painted := strings.ContainsRune(line, 0x1b); painted != (i == 1) {
			t.Errorf("line %d painted=%v, want painted only on the verdict: %q", i, painted, line)
		}
	}
}

// A sentence this package wrote has no exit code, so it has no verdict to
// paint. The two occasions are the add key refusing a redirected stderr and how
// a login that held the terminal ended, and both are the package telling the
// user something rather than a command reporting how it went -- colouring them
// from a verdict vocabulary would invent a judgement nobody made.
func TestTheNoteTakesNoVerdictColour(t *testing.T) {
	pal := theme.Of(theme.Dark)
	a := newApp(fixtureOptions())
	a.m.Pal = pal
	a.m.Width, a.m.Height = 80, 24
	got := a.saying(addNeedsStderr).body()
	for _, r := range []theme.Role{theme.RoleActive, theme.RoleMuted, theme.RoleExhausted} {
		if strings.Contains(got, opener(pal, r)) {
			t.Fatalf("the note panel carries a verdict role: %q", got)
		}
	}
}

// The picker marks the value already in force, and that mark is the fact that
// makes the rest of the list mean something -- it is what the reader is
// comparing every other line against.
func TestThePickerPaintsTheValueAlreadyInForce(t *testing.T) {
	pal := theme.Of(theme.Dark)
	body := strategyPicker("headroom", UnicodeGlyphs).Body(60, pal)
	if !strings.Contains(body, opener(pal, theme.RoleAccent)) {
		t.Fatalf("the picker does not mark where the reader already is: %q", body)
	}
	if ansi.Strip(body) != strategyPicker("headroom", UnicodeGlyphs).Body(60, theme.Of(theme.None)) {
		t.Fatal("painting the picker changed its text")
	}
}

// The [D] screen reuses the main table's state cell so the two can never
// disagree about what a state is CALLED -- and it was throwing the third return
// away, so the two did disagree about what it looks like. It lays its columns
// out by hand and already measures with ansi.StringWidth, so painting the cell
// before the padding is computed costs the block no alignment.
//
// The zero-palette screen is what every fixture in daemonscreen_test.go builds,
// because a Palette's zero value is documented as None. This test is what makes
// that safe to rely on: it compares the painted screen against the zero-palette
// one, so the day the zero palette starts painting, this fails rather than
// thirty-five assertions in a file nobody was editing.
func TestTheDaemonScreensStateCellIsPaintedAndStillLinesUp(t *testing.T) {
	pal := theme.Of(theme.Dark)
	d := runningScreen(t)
	d.Pal = pal
	painted := d.Body(160, 40)

	if ansi.Strip(painted) != runningScreen(t).Body(160, 40) {
		t.Fatal("painting the daemon screen changed its text, or the zero palette is not None")
	}
	for i, line := range strings.Split(painted, "\n") {
		if got := ansi.StringWidth(line); got > 160 {
			t.Fatalf("line %d is %d columns wide once painted", i, got)
		}
	}
	for _, want := range []theme.Role{
		theme.RoleActive, theme.RoleExhausted, theme.RoleCandidate, theme.RoleMuted,
	} {
		if !strings.Contains(painted, opener(pal, want)) {
			t.Errorf("no state cell on the daemon screen carries %v", want)
		}
	}
}

// The screen is handed its palette by the page it was opened from, the same way
// it is handed the log and the credential home. A screen that resolved its own
// would be a second answer to a question the program has already asked once.
func TestTheDaemonScreenIsGivenThePagesPalette(t *testing.T) {
	a := appAt(t, fixtureOptions(), 113, 40)
	a.m.Pal = theme.Of(theme.Dark)
	if got := a.daemonScreen().Pal.Name(); got != theme.Dark {
		t.Fatalf("the daemon screen opened on %v while the page behind it was dark", got)
	}
}

// The interactive path asks the terminal what colour it is, and the answer is
// the only thing that can turn the page light. Both halves are asserted,
// because either alone is silent: a request nobody handles changes nothing, and
// an arm nobody feeds never runs.
//
// The request is a sentinel bubbletea turns into an OSC 11 write, and the
// message it produces is an unexported type no test can name. Its PRESENCE in
// the batch is therefore the assertion: under a stubbed clock the two tick
// commands produce nothing at all, so Init's batch drains to exactly the
// startup read and the colour request.
func TestTheProgramAsksTheTerminalWhatColourItIs(t *testing.T) {
	stubTick(t)
	msgs := drain(newApp(fixtureOptions()).Init())
	if len(msgs) != 2 {
		t.Fatalf("Init drained %d messages, want the startup read and the colour request: %#v", len(msgs), msgs)
	}
	var reads, others int
	for _, msg := range msgs {
		if _, ok := msg.(refreshMsg); ok {
			reads++
			continue
		}
		others++
	}
	if reads != 1 || others != 1 {
		t.Fatalf("Init batched %d reads and %d other messages, want one of each", reads, others)
	}
}

// A light answer resolves the Auto theme to Light; a dark one leaves it Dark.
// Styles are BUILT dark at construction and refined on arrival, never built
// inside the arm: bubbletea sends this message only in answer to a request and
// tracks no timeout, so a terminal that never answers -- one behind a
// multiplexer that eats OSC 11 -- would otherwise leave the page holding no
// palette at all, forever.
func TestABackgroundColourAnswerResolvesTheAutoTheme(t *testing.T) {
	stubTick(t)
	o := fixtureOptions()
	o.Theme = theme.Auto
	a := newApp(o)
	if got := a.m.Pal.Name(); got != theme.Dark {
		t.Fatalf("before any answer the page is %v, want the dark default", got)
	}

	next, _ := a.Update(tea.BackgroundColorMsg{Color: color.White})
	if got := next.(App).m.Pal.Name(); got != theme.Light {
		t.Errorf("a white background left the page on %v", got)
	}
	next, _ = a.Update(tea.BackgroundColorMsg{Color: color.Black})
	if got := next.(App).m.Pal.Name(); got != theme.Dark {
		t.Errorf("a black background left the page on %v", got)
	}
}

// The keybar moves with the palette. help.Model holds its seven styles BY
// VALUE, so an arm that swapped the palette and left the bar behind would draw
// a light page under a dark footer.
func TestTheKeybarIsRebuiltWhenTheThemeResolves(t *testing.T) {
	stubTick(t)
	o := fixtureOptions()
	o.Theme = theme.Auto
	next, _ := newApp(o).Update(tea.BackgroundColorMsg{Color: color.White})
	want := theme.Of(theme.Light).Style(theme.RoleHeader).GetForeground()
	if got := next.(App).m.Help.Styles.ShortKey.GetForeground(); got != want {
		t.Fatalf("the keybar stayed on %v while the page went light", got)
	}
}

// A user who named a theme is not overruled by their terminal. tui.theme=light
// is the escape hatch for exactly the user whose OSC 11 answer never arrives,
// and an arm that recomputed from the answer regardless would take it away from
// them the moment it did.
func TestANamedThemeSurvivesTheTerminalsAnswer(t *testing.T) {
	stubTick(t)
	o := fixtureOptions()
	o.Theme = theme.ANSI
	next, _ := newApp(o).Update(tea.BackgroundColorMsg{Color: color.White})
	if got := next.(App).m.Pal.Name(); got != theme.ANSI {
		t.Fatalf("a named theme was overruled by the terminal: %v", got)
	}
}

// An Options nobody filled in means "nobody said", and nobody-said renders
// plain. This is the shape probes() builds -- newApp(Options{Now: ...}) with no
// Theme at all -- and a resolver that treated the empty name as a request for
// the default would make that page paint and redden every golden under testdata.
func TestAnOptionsWithNoThemeStaysUnpaintedWhenTheTerminalAnswers(t *testing.T) {
	stubTick(t)
	a := newApp(Options{Now: func() time.Time { return fixtureNow }})
	if got := a.m.Pal.Name(); got != theme.None {
		t.Fatalf("an Options with no theme was built on %v", got)
	}
	next, _ := a.Update(tea.BackgroundColorMsg{Color: color.Black})
	if got := next.(App).m.Pal.Name(); got != theme.None {
		t.Fatalf("an Options with no theme painted itself %v once the terminal answered", got)
	}
}

// Two questions in one table. Does the one-shot page resolve theme.Auto against
// the terminal's background, and does it ask ONLY where the answer can change
// what is drawn?
//
// The seam is a package var because no test has a terminal, and what
// DarkBackground does without one is precisely what must never happen inside
// `go test` -- term.MakeRaw on stdin, and up to four seconds of waiting on a
// terminal that answers nothing, since lipgloss runs the query once per stdio
// end at two seconds each. The `asked` flag is half of every case: a Render
// that queried unconditionally would spend those four seconds resolving
// `tui.theme = none`, and a Render that queried on nothing would leave every
// light-terminal user on the dark palette with no way to find out.
func TestTheOneShotPageResolvesAutoThroughTheBackgroundQuery(t *testing.T) {
	saved := DarkBackground
	t.Cleanup(func() { DarkBackground = saved })

	for _, tc := range []struct {
		name       string
		configured theme.Name
		dark       bool
		want       theme.Name
		asks       bool
	}{
		{"auto on a light terminal", theme.Auto, false, theme.Light, true},
		{"auto on a dark terminal", theme.Auto, true, theme.Dark, true},
		{"a named theme never asks", theme.ANSI, false, theme.ANSI, false},
		{"an Options with no theme never asks", "", false, theme.None, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := false
			DarkBackground = func() bool { asked = true; return tc.dark }

			o := fixtureOptions()
			o.Theme = tc.configured
			o.Out = io.Discard
			got, err := Render(o)
			if err != nil {
				t.Fatal(err)
			}
			if asked != tc.asks {
				t.Fatalf("Render asked the terminal for its background: %v, want %v", asked, tc.asks)
			}
			page := sgr(t, got, colorprofile.TrueColor)
			if tc.want == theme.None {
				if strings.ContainsRune(page, 0x1b) {
					t.Fatalf("an Options with no theme painted the one-shot page:\n%q", page)
				}
				return
			}
			if open := opener(theme.Of(tc.want), theme.RoleHeader); !strings.Contains(page, open) {
				t.Fatalf("the one-shot page is not drawn in the %v palette:\n%q", tc.want, page)
			}
		})
	}
}

// Model.Body is the surface carrying the most paint, and until this test it was
// the only painted surface in the package with no strip-equality gate and no
// width gate of its own. The [D] screen has one, the keybar has one, and the
// panel and the picker have their width bounds; the main page had neither.
//
// Two assertions, and neither subsumes the other.
//
// The FIRST is the one the whole fixture strategy rests on: ansi.Strip of the
// painted page equals the colourless page byte for byte. Forty-one assertions
// in render_test.go and fourteen in model_test.go read raw rendered bytes with
// no strip anywhere, and every one of them is sound only because the None theme
// emits nothing -- but "emits nothing" is a claim about a theme, while "the
// paint changed no text" is a claim about the RENDERER, and it is the second
// that a misplaced Render breaks. A style applied to a joined multi-line block
// would right-pad four rows of the wordmark and this would catch it; a style
// applied around a truncation would move where the cut lands and this would
// catch that too.
//
// The SECOND is the width bound, and it is why this page in particular needs
// one. This is the only surface in the package where escapes now sit INSIDE a
// table cell: the gauge is built as "[" + ViewAs(...) + "] " + pct, and progress
// paints fill and track as two separate Render calls, so the SGR bytes are in
// the cell string before lipgloss/table ever measures it. Nothing else here
// exercises that library's own column arithmetic against a styled cell. It is
// asserted as EQUALITY wherever the page is framed, not merely as a bound: a
// bordered box makes every line the same width, so a framed page that is merely
// "not too wide" is a page whose frame has come apart on one side.
//
// Framing is read off the plain page with golden_test.go's own frameClass and
// linesCarrying rather than by asking the height ladder a second time, so this
// test and the ladder-profile test agree about what a framed page is by
// construction instead of by two copies of the same arithmetic.
//
// The table walks the width ladder at every rung that changes the COLUMN SET --
// full, WINDOW gone, TYPE gone, AUTO gone, STATE gone, the gauge collapsed, the
// floor -- and above the top rung as well, where ACCOUNT grows to its maximum.
// The heights are chosen so that each of the blocks the height ladder drops is
// dropped somewhere in the table, including the rung that takes the frame away,
// because an equality assertion that only ever ran on framed pages would never
// have exercised its other arm.
func TestPaintingThePageChangesNoByteOfItsTextAndNoColumnOfItsWidth(t *testing.T) {
	for _, tc := range []struct {
		name string
		w, h int
		prep func(*Model)
	}{
		{"every column and all the chrome", 113, 26, nil},
		{"ACCOUNT grown past the top rung", 160, 26, nil},
		{"WINDOW dropped", 91, 26, nil},
		{"TYPE dropped, figures gone", 77, 24, nil},
		{"AUTO dropped", 71, 20, nil},
		{"STATE dropped, the gauge still drawn", 56, 16, nil},
		{"the frame dropped", 56, 10, nil},
		{"the gauge collapsed", 43, 12, nil},
		{"the smallest page that still renders", 35, 3, nil},
		{"a notice, which is painted as a whole line", 80, 20, func(m *Model) {
			m.Snap.Notices = []string{"hover thresholds could not be read"}
		}},
		{"a runway line, whose bytes are not this package's", 113, 24, func(m *Model) {
			m.Snap.Forecast, m.Snap.HasForecast = fixtureFleet(), true
		}},
		{"zero accounts, so the marker row is the only row", 80, 13, func(m *Model) {
			m.Snap.Rows = nil
		}},
		{"more rows than fit, so the marker row sits under them", 113, 5, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plain, dark := fixtureModel(tc.w, tc.h), darkModel(tc.w, tc.h)
			if tc.prep != nil {
				tc.prep(&plain)
				tc.prep(&dark)
			}
			plainBody, darkBody := plain.Body(), dark.Body()

			if got := ansi.Strip(darkBody); got != plainBody {
				t.Fatalf("painting the page changed its text at %dx%d:\n painted+stripped:\n%s\n plain:\n%s",
					tc.w, tc.h, got, plainBody)
			}

			plainLines := strings.Split(plainBody, "\n")
			framed := linesCarrying(plainLines, frameClass(plain.Glyphs)) == len(plainLines)
			for i, line := range strings.Split(darkBody, "\n") {
				got := ansi.StringWidth(line)
				if got > tc.w {
					t.Errorf("at %dx%d the painted line %d is %d columns wide, %d more than the terminal has: %q",
						tc.w, tc.h, i, got, got-tc.w, line)
					continue
				}
				if framed && got != tc.w {
					t.Errorf("at %dx%d the painted line %d is %d columns inside a %d-column frame, so the frame is ragged: %q",
						tc.w, tc.h, i, got, tc.w, line)
				}
			}
		})
	}
}

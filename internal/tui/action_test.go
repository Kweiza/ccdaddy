package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/theme"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// Idx is a display ordinal and is recompacted when an account is removed; the
// root's own help says scripts must reference accounts by uuid or alias. A
// picker that passed "1" would move a credential belonging to whoever happened
// to occupy that slot -- and the shortest prefix the store resolves is eight
// characters, so an argv of "u-1" resolves to nothing at all and the user
// would see a usage error for a row they can see on screen.
func TestTheSwitchPickerPassesAFullUuidAndNeverTheDisplayOrdinal(t *testing.T) {
	rows := fixtureRows()
	for i := range rows {
		argv := switchPicker(rows, rows[i].Account.UUID, "", UnicodeGlyphs).Chosen()
		if len(argv) != 2 || argv[0] != "switch" {
			t.Fatalf("Chosen() = %v, want [switch <uuid>]", argv)
		}
		if argv[1] != rows[i].Account.UUID {
			t.Fatalf("Chosen() passed %q, want the whole uuid %q", argv[1], rows[i].Account.UUID)
		}
		if len(argv[1]) < 8 {
			t.Fatalf("Chosen() passed %q: the shortest resolvable prefix is 8 and Idx is not a key", argv[1])
		}
	}
}

// The picker names the target the way the table does, or a user who aliased two
// accounts cannot tell which address they are about to move to.
func TestThePickerNamesTargetsTheWayTheTableDoes(t *testing.T) {
	body := switchPicker(fixtureRows(), "", "", UnicodeGlyphs).Body(60, theme.Of(theme.None))
	if !strings.Contains(body, "work@example.com (work)") {
		t.Fatalf("the picker does not name the account as the table does:\n%s", body)
	}
}

// Disabled holds an account out of AUTOMATIC rotation and is not a lock: an
// explicit switch still activates one. A picker that hid it would make the
// dashboard refuse something the command underneath it allows.
func TestTheSwitchPickerStillOffersAnAccountHeldOutOfRotation(t *testing.T) {
	rows := fixtureRows()
	if !rows[1].Account.Disabled {
		t.Fatal("the fixture pool no longer has a disabled account, so this proves nothing")
	}
	if got := len(choosable(switchPicker(rows, "", "", UnicodeGlyphs))); got != len(rows) {
		t.Fatalf("the picker offers %d of %d accounts", got, len(rows))
	}
}

// choosable is every item of a picker a user can put the cursor on, which is
// every item that is not a section heading.
func choosable(p picker) []pickerItem {
	var out []pickerItem
	for _, it := range p.items {
		if !it.header {
			out = append(out, it)
		}
	}
	return out
}

// mixedPool is a fleet of both providers, built here rather than borrowed from
// the goldens' pool, and INTERLEAVED in store order on purpose: a Claude
// account, a Codex one, a Claude one, a Codex one. Sectioning has to reorder
// that, which is what makes a store index and a picker index disagree -- a pool
// the grouping left alone could not tell a uuid-based restore from an
// index-based one.
//
// The first Claude account is live. No codex account is served: which one the
// proxy points at is the caller's fact, and each test that needs it names it.
func mixedPool() []view.Row {
	row := func(uuid, email string, p provider.ID) view.Row {
		return view.Row{Account: store.Account{UUID: uuid, Email: email, Provider: p}}
	}
	rows := []view.Row{
		row("0c1a0000-0000-4000-8000-00000000c1a1", "one@claude.example", provider.Claude),
		row("0c0d0000-0000-4000-8000-00000000c0d1", "one@codex.example", provider.Codex),
		row("0c1a0000-0000-4000-8000-00000000c1a2", "two@claude.example", provider.Claude),
		row("0c0d0000-0000-4000-8000-00000000c0d2", "two@codex.example", provider.Codex),
	}
	rows[0].Active = true
	return rows
}

// A mixed fleet is offered under the same two headings the account table
// draws, in the table's own order: every Claude account under CLAUDE, then
// every Codex account under CODEX, whatever order the store held them in. The
// heading text is the table's constant and not a second spelling of it.
func TestTheSwitchPickerSectionsAMixedPoolUnderTheTablesHeadings(t *testing.T) {
	p := switchPicker(mixedPool(), "", "", UnicodeGlyphs)
	var got []string
	for _, it := range p.items {
		if it.header {
			got = append(got, "["+it.label+"]")
			continue
		}
		got = append(got, it.label)
	}
	want := []string{
		"[" + view.ClaudeSection + "]", "one@claude.example", "two@claude.example",
		"[" + view.CodexSection + "]", "one@codex.example", "two@codex.example",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the picker lists\n  %q\nwant\n  %q", got, want)
	}
}

// Every account of a mixed fleet is still choosable -- sectioning adds lines
// and removes none -- and what enter would run on each is that account's own
// uuid, in the order the table draws them.
func TestEveryAccountOfAMixedPoolIsStillChoosable(t *testing.T) {
	rows := mixedPool()
	var want []string
	for _, s := range view.Sections(rows) {
		for _, line := range s.Rows {
			want = append(want, line.Row.Account.UUID)
		}
	}
	var got []string
	for _, it := range choosable(switchPicker(rows, "", "", UnicodeGlyphs)) {
		if len(it.argv) != 2 || it.argv[0] != "switch" {
			t.Fatalf("a choosable item carries %v, want [switch <uuid>]", it.argv)
		}
		got = append(got, it.argv[1])
	}
	if len(got) != len(rows) || !slices.Equal(got, want) {
		t.Fatalf("the picker offers %q, want every account in the table's order %q", got, want)
	}
}

// The cursor never rests on a heading, in either direction. A heading is drawn
// and cannot be chosen, so a highlight left on one would be a cursor pointing
// at nothing on the one screen where enter moves a credential. Down off the
// last account of a section lands on the first account of the next, and up off
// that one lands back on it: the heading between them is stepped over both
// ways.
func TestThePickerCursorNeverRestsOnAHeading(t *testing.T) {
	rows := mixedPool()
	p := switchPicker(rows, "", "", UnicodeGlyphs)
	if it := p.items[p.cursor]; it.header {
		t.Fatalf("the picker opened on the heading %q", it.label)
	}
	for _, delta := range []int{1, -1} {
		for range len(p.items) {
			p = p.Move(delta)
			if it := p.items[p.cursor]; it.header {
				t.Fatalf("moving %d left the cursor on the heading %q", delta, it.label)
			}
		}
	}
	lastClaude, firstCodex := p.indexOf(rows[2].Account.UUID), p.indexOf(rows[1].Account.UUID)
	if !p.items[lastClaude+1].header {
		t.Fatal("no heading sits between the last Claude account and the first Codex one, so there is nothing to step over")
	}
	p.cursor = lastClaude
	if got := p.Move(1).cursor; got != firstCodex {
		t.Errorf("down off the last Claude account landed on %d (%q), want the first Codex one at %d", got, p.items[got].label, firstCodex)
	}
	p.cursor = firstCodex
	if got := p.Move(-1).cursor; got != lastClaude {
		t.Errorf("up off the first Codex account landed on %d (%q), want the last Claude one at %d", got, p.items[got].label, lastClaude)
	}
}

// Up from the first account stays on it. There is a heading above it and
// nothing above that, so a settle that only ever continued the way the cursor
// was travelling would come to rest on the heading; the fallback the other way
// is what puts it back on the account.
func TestUpFromTheFirstAccountStaysOnIt(t *testing.T) {
	p := switchPicker(mixedPool(), "", "", UnicodeGlyphs)
	if !p.items[0].header {
		t.Fatal("the first line is not a heading, so up from the first account has nothing to step over")
	}
	first := p.cursor
	if got := p.Move(-1).cursor; got != first {
		t.Fatalf("up from the first account moved the cursor from %d to %d (%q)", first, got, p.items[got].label)
	}
}

// Chosen refuses a heading on the type and not merely by construction. The
// constructor never gives a heading an argv, so a test over what it builds
// would be green under a Chosen that simply handed back the nil argv it found;
// this one hand-builds the item the type can express and the constructor does
// not -- a heading carrying a command -- and puts the cursor on it.
func TestChosenRefusesAHeadingEvenWhenItCarriesAnArgv(t *testing.T) {
	p := picker{items: []pickerItem{{
		label:  view.ClaudeSection,
		header: true,
		argv:   []string{"switch", mixedPool()[0].Account.UUID},
	}}}
	if got := p.Chosen(); got != nil {
		t.Fatalf("Chosen() on a heading = %v, want nil", got)
	}
}

// A one-provider fleet gets no headings. The account table draws both headings
// for such a fleet so a reader can see the other provider exists; the picker is
// a list of things to choose, a heading there labels nothing a reader has to
// tell apart, and it costs two of the rows the picker is drawn in.
func TestASingleProviderPickerDrawsNoHeadings(t *testing.T) {
	for _, only := range []provider.ID{provider.Claude, provider.Codex} {
		var rows []view.Row
		for _, r := range mixedPool() {
			if r.Account.Provider == only {
				rows = append(rows, r)
			}
		}
		if len(rows) < 2 {
			t.Fatalf("the pool has %d %s accounts, so a one-provider list here is not a list", len(rows), only)
		}
		p := switchPicker(rows, "", "", UnicodeGlyphs)
		for _, it := range p.items {
			if it.header {
				t.Fatalf("a %s-only picker drew the heading %q", only, it.label)
			}
		}
		if got := len(p.items); got != len(rows) {
			t.Fatalf("a %s-only picker has %d lines for %d accounts", only, got, len(rows))
		}
		body := p.Body(60, theme.Of(theme.None))
		if strings.Contains(body, view.ClaudeSection) || strings.Contains(body, view.CodexSection) {
			t.Fatalf("a %s-only picker drew a section heading:\n%s", only, body)
		}
	}
}

// Both providers' live accounts are marked. Claude's live credential and the
// account the codex proxy serves new threads from are two different facts about
// two different accounts, exactly one of each can hold, and a picker that could
// carry only one mark would have to choose which provider to be honest about.
func TestBothProvidersLiveAccountsAreMarked(t *testing.T) {
	rows := mixedPool()
	serving := rows[3].Account.UUID
	p := switchPicker(rows, "", serving, UnicodeGlyphs)
	var marked []string
	for _, it := range p.items {
		if it.inForce {
			marked = append(marked, it.argv[1])
		}
	}
	if want := []string{rows[0].Account.UUID, serving}; !slices.Equal(marked, want) {
		t.Fatalf("the picker marks %q as in force, want Claude's live account and the served codex one %q", marked, want)
	}
	if body := p.Body(60, theme.Of(theme.None)); strings.Count(body, "* ") != 2 {
		t.Fatalf("the picker drew %d in-force marks, want one per provider:\n%s", strings.Count(body, "* "), body)
	}
}

// The picker opens on the account it was handed, found by uuid, and never on
// that account's index in the store. Sectioning reorders: the pool's second
// account is a codex one and is drawn under the second heading, so its store
// index names a Claude account in the picker. An account no longer in the pool
// opens the picker at the top -- on the first account, not the heading over it.
func TestThePickerOpensOnTheAccountItWasHandedAndNotOnItsStoreIndex(t *testing.T) {
	rows := mixedPool()
	for _, r := range rows {
		if got := switchPicker(rows, r.Account.UUID, "", UnicodeGlyphs).Chosen(); len(got) != 2 || got[1] != r.Account.UUID {
			t.Errorf("handed %s, the picker opened on %v", r.Account.Email, got)
		}
	}
	gone := "00000000-0000-4000-8000-000000000000"
	if got := switchPicker(rows, gone, "", UnicodeGlyphs).Chosen(); len(got) != 2 || got[1] != rows[0].Account.UUID {
		t.Errorf("handed an account that is gone, the picker opened on %v, want the first account", got)
	}
}

// The picker is the same four-choice surface as `ccdad strategy`. Recovery is
// a current engine outcome, not a selectable policy.
func TestTheStrategyPickerOffersFourValuesAndMarksTheCurrentOne(t *testing.T) {
	p := strategyPicker("headroom", UnicodeGlyphs)
	body := p.Body(60, theme.Of(theme.None))
	for _, want := range []string{"hover", "manual", "headroom", "consume-first"} {
		if !strings.Contains(body, want) {
			t.Errorf("the picker does not offer %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"recovery"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("the picker offers %q, which is not a strategy:\n%s", unwanted, body)
		}
	}
	if !strings.Contains(body, "*") {
		t.Error("the picker does not mark the current value")
	}
}

// The picker writes through the one public strategy command.
func TestTheStrategyPickerWritesThroughTheStrategyCommand(t *testing.T) {
	p := strategyPicker("headroom", UnicodeGlyphs)
	got := strings.Join(p.Move(1).Chosen(), " ")
	if got != "strategy consume-first" {
		t.Fatalf("Chosen() = %q, want the strategy command that owns this choice", got)
	}
}

// A value the file does not hold yet marks nothing rather than marking the
// first item: "you are here" is a claim, and making it about an arbitrary row
// is worse than not making it.
func TestAnUnrecognisedCurrentStrategyMarksNothing(t *testing.T) {
	if body := strategyPicker("something-else", UnicodeGlyphs).Body(60, theme.Of(theme.None)); strings.Contains(body, "*") {
		t.Fatalf("an unknown current value still marked a row:\n%s", body)
	}
}

// The cursor stops at the ends. A held-down key that wrapped past the bottom
// would put the highlight on an account at the top that the user was not
// looking at when they let go -- on a list where enter moves a credential.
//
// The ends are the ends of the CHOOSABLE list, which is what the picker opens on
// and what the last item is: a section heading is drawn between them and is
// neither.
func TestTheCursorStopsAtTheEndsRatherThanWrapping(t *testing.T) {
	rows := fixtureRows()
	p := switchPicker(rows, "", "", UnicodeGlyphs)
	top := p.cursor
	if got := p.Move(-1).cursor; got != top {
		t.Errorf("up from the top landed on %d, want %d", got, top)
	}
	if got := p.Move(len(p.items) + 5).cursor; got != len(p.items)-1 {
		t.Errorf("down past the bottom landed on %d, want %d", got, len(p.items)-1)
	}
}

// An empty store is a real state -- a fresh install -- and it reaches the
// picker as a list with nothing in it rather than as a panic.
func TestAPickerOverAnEmptyStoreChoosesNothingAndSaysSo(t *testing.T) {
	p := switchPicker(nil, "", "", UnicodeGlyphs)
	if got := p.Chosen(); got != nil {
		t.Fatalf("Chosen() over an empty store = %v, want nil", got)
	}
	if body := p.Body(60, theme.Of(theme.None)); !strings.Contains(body, "nothing to choose from") {
		t.Fatalf("an empty picker rendered no explanation:\n%s", body)
	}
}

// Exit 3 is "already on that account" and it is not an error. A dashboard that
// painted every non-zero red would tell a user something went wrong when
// nothing did.
func TestExitThreeIsReportedAsNothingToDoAndNotAsAFailure(t *testing.T) {
	ex := func(argv []string) (int, string, string) { return 3, "", "already on that account\n" }
	body := run(ex, []string{"switch", "uuid-a"}).Body(60, theme.Of(theme.None))
	if !strings.Contains(body, "already on that account") {
		t.Fatalf("the command's own words did not reach the panel:\n%s", body)
	}
	if !strings.Contains(body, "nothing to do") {
		t.Fatalf("exit 3 was not reported in the contract's own words:\n%s", body)
	}
	if strings.Contains(strings.ToLower(body), "failed") || strings.Contains(strings.ToLower(body), "error") {
		t.Fatalf("exit 3 was reported as a failure:\n%s", body)
	}
}

// A switch that changed nothing must never be reported as a switch. The TUI
// renders what the command WROTE rather than deciding for itself, which is what
// makes this true without the TUI knowing what a displacement is.
func TestTheCommandsOwnWordsAreRenderedVerbatim(t *testing.T) {
	const note = "the environment's token wins; the login was not moved\n"
	ex := func([]string) (int, string, string) { return 0, "", note }
	if body := run(ex, []string{"switch", "uuid-a"}).Body(60, theme.Of(theme.None)); !strings.Contains(body, strings.TrimSpace(note)) {
		t.Fatalf("the displacement note did not survive to the panel:\n%s", body)
	}
}

// Exit 4 is blocked, and inside a `ccdad run` session that is exactly what the
// scoped-session refusal produces. The panel says so in the refusal's own
// words -- there is no second gate here and there must not be one.
func TestABlockedCommandIsReportedWithItsOwnRefusal(t *testing.T) {
	const refusal = "ccdad switch is refused inside a 'ccdad run' session\n"
	ex := func([]string) (int, string, string) { return 4, "", refusal }
	body := run(ex, []string{"switch", "uuid-a"}).Body(80, theme.Of(theme.None))
	if !strings.Contains(body, "blocked") {
		t.Fatalf("exit 4 was not reported as blocked:\n%s", body)
	}
	if !strings.Contains(body, strings.TrimSpace(refusal)) {
		t.Fatalf("the refusal's own words did not reach the panel:\n%s", body)
	}
}

// A code the contract does not name carries its own number rather than a word
// this package invented for it.
func TestAnUnnamedExitCodeCarriesItsNumber(t *testing.T) {
	ex := func([]string) (int, string, string) { return 7, "", "something else went wrong\n" }
	body := run(ex, []string{"switch", "uuid-a"}).Body(60, theme.Of(theme.None))
	if !strings.Contains(body, "exit 7") {
		t.Fatalf("exit 7 lost its code:\n%s", body)
	}
	if !strings.Contains(body, "something else went wrong") {
		t.Fatalf("exit 7 lost what the command said:\n%s", body)
	}
}

// A command that wrote nothing contributes nothing, rather than a blank line
// that reads as output nobody can see.
func TestACommandThatWroteNothingAddsNoBlankLine(t *testing.T) {
	ex := func([]string) (int, string, string) { return 0, "", "" }
	body := run(ex, []string{"daemon", "start"}).Body(60, theme.Of(theme.None))
	if got := len(strings.Split(body, "\n")); got != 2 {
		t.Fatalf("a silent command rendered %d lines, want the command and its verdict:\n%s", got, body)
	}
}

// The panel never runs wider than the frame it is drawn in. A command's stderr
// is arbitrary length and a bordered box soft-wraps rather than truncating, so
// one long refusal would cost as many rows as it needed.
//
// It draws under the palette that PAINTS, and that is what this bound is for.
// block measures with ansi.StringWidth and cuts with ansi.Truncate, so a
// painted block is the case where those two have work to do; a colourless one
// would leave the ANSI-aware half of block untested at every width here.
func TestThePanelNeverExceedsTheWidthItWasGiven(t *testing.T) {
	long := strings.Repeat("a refusal that keeps going ", 20)
	ex := func([]string) (int, string, string) { return 4, long + "\n", long + "\n" }
	for _, width := range []int{20, 40, 60, 80} {
		body := run(ex, []string{"switch", strings.Repeat("u", 60)}).Body(width, theme.Of(theme.Dark))
		for i, line := range strings.Split(body, "\n") {
			if got := ansi.StringWidth(line); got > width {
				t.Errorf("at width %d line %d is %d columns: %q", width, i, got, line)
			}
		}
	}
}

// The picker is drawn inside the same frame and obeys the same bound, and it is
// drawn painted for the same reason the panel above is.
func TestThePickerNeverExceedsTheWidthItWasGiven(t *testing.T) {
	rows := append(fixtureRows(), view.Row{})
	rows[len(rows)-1].Account.Email = strings.Repeat("long", 30) + "@example.com"
	for _, width := range []int{20, 40, 60} {
		for i, line := range strings.Split(switchPicker(rows, "", "", UnicodeGlyphs).Body(width, theme.Of(theme.Dark)), "\n") {
			if got := ansi.StringWidth(line); got > width {
				t.Errorf("at width %d line %d is %d columns: %q", width, i, got, line)
			}
		}
	}
}

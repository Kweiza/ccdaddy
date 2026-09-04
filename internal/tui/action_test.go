package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

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
		argv := switchPicker(rows, i, UnicodeGlyphs).Chosen()
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
	body := switchPicker(fixtureRows(), 0, UnicodeGlyphs).Body(60, theme.Of(theme.None))
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
	if got := len(switchPicker(rows, 0, UnicodeGlyphs).items); got != len(rows) {
		t.Fatalf("the picker offers %d of %d accounts", got, len(rows))
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
func TestTheCursorStopsAtTheEndsRatherThanWrapping(t *testing.T) {
	rows := fixtureRows()
	p := switchPicker(rows, 0, UnicodeGlyphs)
	if got := p.Move(-1).cursor; got != 0 {
		t.Errorf("up from the top landed on %d, want 0", got)
	}
	if got := p.Move(len(rows) + 5).cursor; got != len(rows)-1 {
		t.Errorf("down past the bottom landed on %d, want %d", got, len(rows)-1)
	}
}

// An empty store is a real state -- a fresh install -- and it reaches the
// picker as a list with nothing in it rather than as a panic.
func TestAPickerOverAnEmptyStoreChoosesNothingAndSaysSo(t *testing.T) {
	p := switchPicker(nil, 0, UnicodeGlyphs)
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
		for i, line := range strings.Split(switchPicker(rows, 0, UnicodeGlyphs).Body(width, theme.Of(theme.Dark)), "\n") {
			if got := ansi.StringWidth(line); got > width {
				t.Errorf("at width %d line %d is %d columns: %q", width, i, got, line)
			}
		}
	}
}

package cli

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/theme"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// lipgloss v2's Render() emits truecolor whatever the destination is: the v1
// global renderer that stripped it off a pipe is gone. One forgotten writer
// therefore puts escape bytes into every redirected invocation and every CI
// log, and nothing else in this repository would notice. That was easy to see
// when there was not one ESC byte in the tree; it is the harder case now that
// list, status, doctor, the daemon summary and every screen of the dashboard
// emit them by design, because a leak no longer stands out against a tree that
// emits none -- it looks exactly like the surfaces that are working.
func TestAColouredRenderReachesABufferWithNoEscapeBytes(t *testing.T) {
	isolate(t)
	var buf bytes.Buffer
	w := colorWriter(&buf)
	if _, err := w.Write([]byte("\x1b[38;2;255;0;0mred\x1b[0m")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); strings.ContainsRune(got, 0x1b) {
		t.Fatalf("the writer passed an escape byte through to a buffer: %q", got)
	}
	if !strings.Contains(buf.String(), "red") {
		t.Fatalf("the writer ate the text along with the colour: %q", buf.String())
	}
}

// NO_COLOR is a user's explicit instruction and it is honoured on a terminal
// too, not only off one. TTY_FORCE simulates a terminal destination
// (colorprofile's own escape hatch for exactly this, since bytes.Buffer can
// never satisfy term.File) so the profile this resolves to would otherwise be
// colour-capable, and NO_COLOR must still floor it to ASCII.
//
// The control case exists because "profile ends up <= ASCII" is ambiguous by
// itself: it is equally what NO_COLOR flooring a colour-capable profile looks
// like AND what TTY_FORCE silently failing and falling back to NoTTY looks
// like. Running the identical TTY_FORCE+TERM setup with NO_COLOR genuinely
// unset first, and requiring a colour-capable profile there, is what makes the
// NO_COLOR=1 assertion below mean something rather than passing either way.
func TestNoColorStripsEvenWhereColourWouldOtherwiseBeAllowed(t *testing.T) {
	isolate(t)
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TTY_FORCE", "1")
	t.Setenv("CLICOLOR_FORCE", "")
	// COLORTERM=truecolor (exported by most modern terminal emulators, tmux
	// with Tc/RGB, and some IDE-integrated shells) would push the control
	// case's profile to TrueColor instead of ANSI256, so it is pinned
	// explicitly rather than left to whatever machine happens to run this.
	t.Setenv("COLORTERM", "")

	// Control: the same destination, with NO_COLOR genuinely unset (empty,
	// which colorprofile's strconv.ParseBool treats as false rather than as
	// NO_COLOR's own "present regardless of value" rule -- t.Setenv has no
	// unset, and empty is the value that actually reads as "not set" to this
	// library). Measured: this resolves to ANSI256 with COLORTERM pinned
	// above, but the property that actually matters is "colour-capable at
	// all", so the assertion checks that rather than the specific profile.
	t.Setenv("NO_COLOR", "")
	var control bytes.Buffer
	wc, ok := colorWriter(&control).(*colorprofile.Writer)
	if !ok {
		t.Fatalf("colorWriter did not return a *colorprofile.Writer: %T", colorWriter(&control))
	}
	if wc.Profile <= colorprofile.ASCII {
		t.Fatalf("control case (NO_COLOR unset): profile is %v, which carries no colour -- "+
			"TTY_FORCE did not force a colour-capable profile, so the NO_COLOR=1 "+
			"assertion below would pass even if NO_COLOR did nothing at all", wc.Profile)
	}

	// The same destination, with NO_COLOR=1, must be floored to ASCII.
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	w, ok := colorWriter(&buf).(*colorprofile.Writer)
	if !ok {
		t.Fatalf("colorWriter did not return a *colorprofile.Writer: %T", colorWriter(&buf))
	}
	if w.Profile > colorprofile.ASCII {
		t.Fatalf("NO_COLOR=1 left the profile at %v, which still carries colour", w.Profile)
	}
}

// `tui.theme = auto` is answered from a DEFINITION and never from the terminal.
// That is the rule that keeps a one-shot listing off a query which costs four
// seconds on an emulator that ignores it -- two per stdio end, both legs run,
// once per process, and every one of these commands is its own process.
//
// The whole vocabulary is in the table and not auto alone, because the defect
// this replaces was not "auto is wrong", it was "one spelling of five reached
// for a runtime answer while the other four did not". A fix that special-cased
// auto and left `dark` resolving through a terminal question would block just
// the same, and the row that would catch it is the row that looks redundant.
//
// Dark is the answer because dark is what the query itself returns when it
// declines to ask, which the redirected dashboard page and an interactive one
// inside a multiplexer both already take. One default, defined once, is what
// stops three surfaces on the same terminal from disagreeing about what nobody
// answered.
//
// This says what gets PRINTED. The syntax walk in appearance_test.go says the
// query is not reached, and neither test subsumes the other: a package that
// resolved auto to Light would pass the walk, and a package that asked the
// terminal and was told dark would pass this.
func TestTheConfiguredThemeResolvesWithoutAskingTheTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want theme.Name
	}{
		{"no [tui] table at all, which is the default nobody changes", "", theme.Dark},
		{"auto, spelled out", "[tui]\ntheme = \"auto\"\n", theme.Dark},
		{"dark", "[tui]\ntheme = \"dark\"\n", theme.Dark},
		{"light", "[tui]\ntheme = \"light\"\n", theme.Light},
		{"ansi", "[tui]\ntheme = \"ansi\"\n", theme.ANSI},
		{"none", "[tui]\ntheme = \"none\"\n", theme.None},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			if tc.body != "" {
				writeConfig(t, tc.body)
			}
			if got := resolvePalette().Name(); got != tc.want {
				t.Fatalf("the configured theme resolved to the %v palette, want %v: a listing "+
					"answers auto with the defined dark default and asks nothing", got, tc.want)
			}
		})
	}
}

// The third probe, beside the two that already exist. It is a package var for
// the same reason they are: a real terminal is not something a test can
// arrange, and the decision hanging on this one is a REFUSAL -- the class that
// has to be exercised.
func TestStderrIsAProbeATestCanReplace(t *testing.T) {
	isolate(t)
	saved := stderrIsTTY
	t.Cleanup(func() { stderrIsTTY = saved })
	stderrIsTTY = func() bool { return true }
	if !stderrIsTTY() {
		t.Fatal("stderrIsTTY is not swappable, so the [A]dd refusal cannot be tested")
	}
}

// The whole reason text/tabwriter had to go, as an assertion.
//
// tabwriter measured a cell by rune count, so one coloured cell padded its
// column for the escape bytes too and the header ended up padded for cells the
// rows were not. What this requires is that stripping every escape byte out of
// a painted table gives back exactly the table an unpainted build prints: same
// widths, same gaps, same bytes.
//
// THE REFERENCE IS THE None THEME, AND THAT IS THE ENTIRE POINT OF THIS TEST.
// The obvious version -- run it once with CLICOLOR_FORCE and once without, and
// compare the stripped output to the unforced output -- is VACUOUS, and it was
// written that way first and measured passing against a deliberately
// reintroduced tabwriter whose table was visibly destroyed. It passes because
// lipgloss.Style.Render emits its escapes unconditionally, long before
// colorprofile sees anything: the layout is computed over escaped cells in BOTH
// runs, and the unforced run then has those escapes stripped downstream by the
// profile writer. Two identically wrecked tables compare equal. The forcing
// variable moves which bytes survive and never moves the arithmetic, so it
// cannot be the axis this is measured on.
//
// tui.theme = "none" is the axis that works, because a None palette sets no
// Foreground at all and Render on a style with nothing set returns the string
// unchanged. That run is the only one in this package where the widths were
// computed over genuinely bare cells, which makes it the only honest reference
// for what the columns are supposed to be.
func TestAColouredTableIsThePlainTableWithColourAddedAndNothingMoved(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedBurningFleet(t)

	// The unpainted reference: no style in the palette carries a colour, so
	// nothing calls for an escape and the widths are measured over the text
	// alone.
	writeConfig(t, "[tui]\ntheme = \"none\"\n")
	_, plain, _, _ := runRoot(t, "status")
	if strings.ContainsRune(plain, 0x1b) {
		t.Fatalf("the None theme emitted an escape byte, so it is not the bare "+
			"reference this comparison needs:\n%q", plain)
	}

	// TTY_FORCE is colorprofile's own escape hatch for a destination that can
	// never satisfy term.File, and CLICOLOR_FORCE is what raises the profile
	// once it believes there is a terminal. Together they are the only way to
	// get real SGR bytes into a bytes.Buffer.
	writeConfig(t, "[tui]\ntheme = \"dark\"\n")
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TTY_FORCE", "1")
	t.Setenv("CLICOLOR_FORCE", "1")
	_, coloured, _, _ := runRoot(t, "status")

	if !strings.ContainsRune(coloured, 0x1b) {
		t.Fatalf("the forced profile produced no escape byte at all, so this test "+
			"would pass with the colour removed entirely:\n%q", coloured)
	}
	if got := ansi.Strip(coloured); got != plain {
		t.Fatalf("colour moved the columns:\n stripped %q\n plain    %q", got, plain)
	}
}

// The document paths do not go through the colour writer, and they are asserted
// to rather than left to the fact that nothing styles them today.
func TestTheJSONPathsCarryNoEscapeByteEvenWithColourForced(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedBurningFleet(t)
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TTY_FORCE", "1")
	t.Setenv("CLICOLOR_FORCE", "1")

	for _, argv := range [][]string{
		{"status", "--json"}, {"list", "--json"},
		{"doctor", "--json"}, {"daemon", "status", "--json"},
	} {
		_, out, _, _ := runRoot(t, argv...)
		if strings.ContainsRune(out, 0x1b) {
			t.Errorf("`ccdad %s` put an escape byte in its document: %q", strings.Join(argv, " "), out)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Errorf("`ccdad %s` did not emit one JSON object: %v", strings.Join(argv, " "), err)
		}
	}
}

// The root must NOT be wrapped, and this is the assertion that says so out
// loud. cmd.SetOut(colorWriter(...)) at the root would double-wrap runTui's
// Out, and colorprofile's Detect would then be looking at a *colorprofile.Writer
// rather than at a terminal, resolve NoTTY and silently kill colour on the
// one-shot dashboard render -- a failure with no error, no test and no symptom
// except a page that came out grey.
//
// THE ROOT IS READ EXACTLY AS NewRootCmd LEFT IT, AND THAT IS THE ENTIRE POINT.
// The obvious version of this test calls root.SetOut(&buf) first, the way every
// other test in this package does, and it is BLIND: SetOut replaces whatever
// the constructor installed, so a NewRootCmd that really did wrap its own
// writer has the wrap thrown away one line before the assertion reads it. The
// first arm then passes on the buffer, tuiOptions wraps that same buffer rather
// than a wrapped writer, and the "doubled" arm below is unreachable as well --
// both arms green against the exact defect they name. Measured: with
// root.SetOut(colorWriter(os.Stdout)) added to NewRootCmd, the SetOut shape
// passed and this shape fails on the first arm.
//
// So nothing is set here. OutOrStdout() falls back to os.Stdout on an untouched
// root, which is precisely the production value this is asking about, and the
// only thing either arm does with a writer is ask its type -- no byte is
// written to the developer's terminal by this test.
func TestTheRootIsNotWrappedAndTheDashboardWriterIsNotDoubled(t *testing.T) {
	isolate(t)
	root := NewRootCmd()
	if _, wrapped := root.OutOrStdout().(*colorprofile.Writer); wrapped {
		t.Fatal("the root's own writer is a *colorprofile.Writer; every renderer under " +
			"it now wraps a wrapped writer")
	}
	w, ok := tuiOptions(root).Out.(*colorprofile.Writer)
	if !ok {
		t.Fatalf("the dashboard's Out is not a *colorprofile.Writer: %T", tuiOptions(root).Out)
	}
	if _, doubled := w.Forward.(*colorprofile.Writer); doubled {
		t.Fatal("the dashboard's writer forwards to another *colorprofile.Writer")
	}
}

// styledRun matches one lipgloss-styled cell: an SGR sequence, the text it
// opened over, and the reset that closes it. lipgloss emits exactly this shape
// because every style in this package sets a Foreground and nothing else, and
// it closes the cell BEFORE the column padding, so the captured text is the
// cell and not the gap after it.
var styledRun = regexp.MustCompile("\x1b\\[[0-9;]*m([^\x1b]*)\x1b\\[[0-9;]*m")

// markerCell is the IDX cell of the live row, and it needs its own shape
// because that cell is the one thing in either table painted WHOLE rather than
// as a label. `list` and `status` both build it as
// fmt.Sprintf("%s %d", r.Marker(), idx) and quotaCellStyle paints column 0 of
// the active row, so the styled run is "* 1" and there is no styled run
// anywhere that is a bare "*".
//
// Matched by shape rather than listed by value, so this stays a test about the
// rule and not about how many accounts the fixture happened to seed. The "* "
// is still the glyph carrying the distinction: strip every escape and "* 1"
// says live exactly as loudly as it did painted.
var markerCell = regexp.MustCompile(`^\* [0-9]+$`)

// Colour is never the only thing carrying a distinction, and neither one-shot
// table has a STATE column at any width to fall back on -- `ccdad list` has
// never had one and `ccdad status` has never had one either. So the rule this
// package can actually hold is stronger and simpler than "keep a word
// somewhere": EVERY painted run must itself be a glyph or a word that states
// what the paint states. Strip every escape byte and nothing is lost.
//
// The allowed set is therefore an allow-list and not a deny-list, and that is
// the point: a future arm that paints a bare percentage, a reset duration or an
// email address fails here by construction, named in the failure message,
// without anybody having to think of that arm in advance. This is the test that
// would have caught painting the LEFT cell on Row.Empty() -- a red "75%" with
// no glyph and no word anywhere on the row, on a verdict read off a different
// window from the one that number came from.
func TestColourIsNeverTheSoleCarrierInAOneShotTable(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	// Three accounts and three shapes of number, because the allow-list only
	// bites on rows that exist: one polled with room, one never polled at all
	// so LEFT and USED both read "?", and one polled with NOTHING left.
	//
	// The spent account is the load-bearing one. It is the row an exhausted arm
	// in quotaCellStyle would paint -- a bare "0%" with no glyph and no word
	// beside it -- and without it in the fixture this whole test passes just as
	// happily against a build that HAS that arm, which was measured: adding the
	// arm and running a two-account fixture came back green. A gate that cannot
	// see the thing it forbids is not a gate.
	//
	// `switch` is what makes the first one live. seedLiveAs writes an access
	// token the stored blob does not carry, so attribution does not match it
	// and no row gets the marker, which would leave the RoleActive arm
	// unexercised and the sawMarker check below failing for the wrong reason.
	seedAccount(t, "uuid-a", "a@example.com")
	seedUsage(t, "uuid-a", 4)
	seedAccount(t, "uuid-b", "b@example.com")
	seedAccount(t, "uuid-c", "c@example.com")
	seedUsage(t, "uuid-c", 0)
	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("switch = %d (%s)", code, top)
	}

	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TTY_FORCE", "1")
	t.Setenv("CLICOLOR_FORCE", "1")

	// Every header label of both tables, and the unreadable sign. The active
	// marker is not here because it is not painted alone -- it arrives as the
	// whole IDX cell, and markerCell above is its shape. Nothing else in either
	// table may carry a colour.
	allowed := map[string]bool{
		"IDX": true, "ACCOUNT": true, "TYPE": true, "TIER": true, "LEFT": true,
		"RESETS IN": true, "USED": true, "WINDOW": true, "PACE": true, "AGE": true,
		view.Unreadable: true,
	}

	for _, argv := range []string{"list", "status"} {
		_, out, _, top := runRoot(t, argv)
		runs := styledRun.FindAllStringSubmatch(out, -1)
		if len(runs) == 0 {
			t.Fatalf("`ccdad %s` painted nothing at all, so this test proves nothing (%s):\n%q",
				argv, top, out)
		}
		sawMarker, sawUnreadable := false, false
		for _, m := range runs {
			text := strings.TrimSpace(m[1])
			// The IDX cell is matched by shape and allowed here rather than in
			// the map, because the map is keyed on exact text and this cell
			// carries an index that changes with the fixture.
			if markerCell.MatchString(text) {
				sawMarker = true
				continue
			}
			if text == view.Unreadable {
				sawUnreadable = true
			}
			if !allowed[text] {
				t.Errorf("`ccdad %s` painted %q, which is neither a glyph nor a word that "+
					"states what the colour states: strip the escapes and a reader has "+
					"lost the distinction entirely", argv, text)
			}
		}
		// Without these the allow-list above is satisfied by a build that
		// paints the header and nothing else, which is not the property.
		if !sawMarker {
			t.Errorf("`ccdad %s` never painted the active marker, so the RoleActive arm is "+
				"unexercised and the allow-list is passing on the header alone:\n%q", argv, out)
		}
		if !sawUnreadable {
			t.Errorf("`ccdad %s` never painted an unreadable cell, so the RoleMuted arm is "+
				"unexercised:\n%q", argv, out)
		}
	}
}

package tui

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os/exec"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// A hand-rolled goroutine ticker piles up across an [A]dd -- the event loop is
// blocked for the whole span the login holds the terminal -- and then floods
// the loop the moment it ends. Rescheduling from Update means at most one of
// each clock is ever outstanding, and this is what says so.
func TestATickSchedulesExactlyOneSuccessor(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  tea.Msg
		want time.Duration
	}{
		{"the content clock", contentMsg(fixtureNow), contentCadence},
		{"the reload clock", reloadMsg(fixtureNow), reloadCadence},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheduled := stubTick(t)
			_, _ = newApp(fixtureOptions()).Update(tc.msg)
			if len(*scheduled) != 1 {
				t.Fatalf("one %s tick scheduled %d successors, want exactly 1: %v", tc.name, len(*scheduled), *scheduled)
			}
			if (*scheduled)[0].after != tc.want {
				t.Errorf("%s rescheduled at %s, want %s", tc.name, (*scheduled)[0].after, tc.want)
			}
			// The successor has to be the SAME clock. Swap the two messages
			// and every duration in this file stays right while the store gets
			// read every second and a half.
			if got, want := reflect.TypeOf((*scheduled)[0].msg), reflect.TypeOf(tc.msg); got != want {
				t.Errorf("%s rescheduled itself as a %v, want %v", tc.name, got, want)
			}
		})
	}
}

// The two cadences are the whole subject of this task, so they are pinned
// against the design's own bands rather than against themselves: the cheap
// redraw between one and two seconds, and the daemon probe between five and
// ten. An assertion that compared each constant to itself would hold for any
// value either of them was ever changed to.
func TestBothCadencesAreInsideTheBandsTheyWereBudgetedFor(t *testing.T) {
	if contentCadence < time.Second || contentCadence > 2*time.Second {
		t.Errorf("the content cadence is %s, outside the one-to-two-second band", contentCadence)
	}
	if reloadCadence < 5*time.Second || reloadCadence > 10*time.Second {
		t.Errorf("the reload cadence is %s, outside the five-to-ten-second band", reloadCadence)
	}
	if contentCadence >= reloadCadence {
		t.Errorf("the cheap clock (%s) is not faster than the expensive one (%s)", contentCadence, reloadCadence)
	}
}

// Both clocks are armed once, at the start, and neither is armed twice.
func TestBothClocksAreStartedExactlyOnce(t *testing.T) {
	scheduled := stubTick(t)
	newApp(fixtureOptions()).Init()
	if len(*scheduled) != 2 {
		t.Fatalf("Init armed %d timers, want 2 (content and reload): %v", len(*scheduled), *scheduled)
	}
	seen := map[time.Duration]bool{}
	for _, a := range *scheduled {
		seen[a.after] = true
	}
	if !seen[contentCadence] || !seen[reloadCadence] {
		t.Errorf("Init armed %v, want one of each cadence", *scheduled)
	}
}

// Three costs, one clock each. store.Open does a MkdirAll and two Chmod(0700)
// on EVERY call and the daemon probe is a file read plus a shared flock with
// up to three attempts a hundred milliseconds apart -- and the seam that
// carries all of it is one call, so the only way not to pay for it every
// second and a half is not to make that call every second and a half.
//
// The content clock reads NOTHING. It advances the moment the page describes,
// which is what every age and countdown on it is measured against.
func TestTheExpensiveReadsDoNotRunAtTheContentCadence(t *testing.T) {
	stubTick(t)
	reads := 0
	o := fixtureOptions()
	snap := fixtureSnapshot(fixtureReport(113, 26))
	o.Load = func(time.Time) (view.Snapshot, error) { reads++; return snap, nil }

	var m tea.Model = newApp(o)
	for i := 0; i < 100; i++ {
		var cmd tea.Cmd
		m, cmd = m.Update(contentMsg(fixtureNow))
		for _, out := range drain(cmd) {
			if _, ok := out.(loadedMsg); ok {
				t.Fatal("a content tick issued a load")
			}
		}
	}
	if reads != 0 {
		t.Fatalf("a hundred content ticks read the documents %d times, want 0", reads)
	}

	_, cmd := m.Update(reloadMsg(fixtureNow))
	loads := 0
	for _, out := range drain(cmd) {
		if _, ok := out.(loadedMsg); ok {
			loads++
		}
	}
	if loads != 1 {
		t.Fatalf("one reload tick issued %d loads, want exactly 1", loads)
	}
}

// The content clock is not merely cheap, it is the thing that makes the page
// move at all: with no read of its own, the only thing it can change is the
// moment every span on the page is measured from.
func TestTheContentTickAdvancesTheMomentThePageDescribes(t *testing.T) {
	stubTick(t)
	later := fixtureNow.Add(37 * time.Second)
	o := fixtureOptions()
	o.Now = func() time.Time { return later }

	a := appAt(t, o, 113, 26)
	// The message carries a DIFFERENT instant from the one Options hands out,
	// so a loop that read the tick's own wall clock instead of the injected one
	// fails here rather than agreeing by coincidence.
	next, _ := a.Update(contentMsg(fixtureNow.Add(-time.Hour)))
	if got := next.(App).m.Snap.Now; !got.Equal(later) {
		t.Fatalf("after a content tick the page describes %s, want %s", got, later)
	}
}

// Never. The usage endpoint allows roughly 28-30 requests per identity per
// rolling hour on a sliding window, and quota only moves every minute to ten
// anyway, so one dashboard left open would saturate an account for an hour and
// learn nothing for it.
//
// The rule is enforced by there being nothing to call rather than by anybody
// remembering it: the loop reaches the world through the injected seam, one
// bounded log read, and the login child. This walks the syntax of both files
// to say that no fourth way got in -- a package named here that is not on the
// pure list has to be listed call by call.
func TestNothingInTheLoopEverFetches(t *testing.T) {
	// The pure list is packages with no way to reach the world at all, so no
	// call into one of them can be a fetch and none has to be listed by name.
	//
	// internal/theme is on it because it is a leaf that imports image/color and
	// lipgloss and nothing else: every exported name is either a constant or a
	// total function over a Name and a bool. In particular theme.Pick is the
	// arm that resolves theme.Auto once the terminal has ANSWERED -- the
	// question was asked by the library, off a Cmd, and arrived here as a
	// message. Resolving an answer somebody else obtained is not a read, and
	// the thing this test exists to forbid, a query issued from inside the
	// loop, is exactly what routing it through Init's Cmd avoids.
	pure := map[string]bool{
		"errors":                    true,
		"fmt":                       true,
		"strings":                   true,
		"time":                      true,
		"charm.land/bubbles/v2/key": true,
		"charm.land/bubbletea/v2":   true,
		"github.com/Kweiza/ccdaddy/internal/theme": true,
		"github.com/Kweiza/ccdaddy/internal/view":  true,
	}
	// Every name this package reaches for outside the pure list, one at a
	// time. TailLog is the bounded log read -- the whole of this package's own
	// I/O that does not arrive through Options, and it derives no figure: the
	// daemon's lines are shown as lines. DaemonRunning is a constant, and
	// naming a constant reads nothing; it is on the list because the walk
	// below deliberately does not distinguish a call from a mention, so that a
	// fetch parked in a package-level value and called through the name
	// cannot slip past it.
	allowed := map[string]bool{
		"daemon.TailLog":       true,
		"daemon.DaemonRunning": true,
	}

	for _, name := range []string{"model.go", "run.go"} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		path := map[string]string{}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			local := p[strings.LastIndex(p, "/")+1:]
			if imp.Name != nil {
				local = imp.Name.Name
			}
			path[local] = p
		}
		ast.Inspect(f, func(n ast.Node) bool {
			pkg, sel, ok := qualifies(n)
			if !ok {
				return true
			}
			p, ok := path[pkg]
			if !ok || pure[p] {
				return true
			}
			if !allowed[pkg+"."+sel] {
				t.Errorf("%s reaches %s.%s; the loop's whole I/O is the injected seam, the bounded log read and the login child",
					name, pkg, sel)
			}
			return true
		})
	}
}

// The keybar must not advertise something the model ignores. This is the
// property the deleted footer test held, against the switch that replaced it.
func TestEveryAdvertisedKeyIsHandled(t *testing.T) {
	bindings := DefaultKeys().ShortHelp()
	if len(bindings) == 0 {
		t.Fatal("the key map is empty, so this test and the keybar both assert nothing")
	}
	for _, b := range bindings {
		if !Handles(b) {
			t.Errorf("the keybar offers %q and the model does nothing with it", b.Help().Key)
		}
	}
	for _, group := range DefaultKeys().FullHelp() {
		for _, b := range group {
			if !Handles(b) {
				t.Errorf("the help view offers %q and the model does nothing with it", b.Help().Key)
			}
		}
	}
}

// A key nothing binds is not handled, which is what stops the predicate above
// from being vacuously true.
func TestAKeyNobodyBoundIsNotReportedAsHandled(t *testing.T) {
	if Handles(unboundKey()) {
		t.Fatal("Handles said yes to a key this program never bound")
	}
}

// One keystroke never moves a credential: s opens a picker, and only enter on
// the picker runs anything.
func TestTheSwitchKeyOpensAPickerAndRunsNothing(t *testing.T) {
	var ran [][]string
	o := fixtureOptions()
	o.Exec = recorder(&ran)

	a := appAt(t, o, 113, 26)
	next, cmd, ok := a.key(keyPress("s"))
	if !ok {
		t.Fatal("the switch key was not handled")
	}
	drain(cmd)
	if len(ran) != 0 {
		t.Fatalf("pressing s ran %v", ran)
	}
	if next.scr != screenPicker {
		t.Fatal("pressing s did not open a picker")
	}

	after, cmd, _ := next.key(keyPress("enter"))
	drain(cmd)
	if len(ran) != 1 {
		t.Fatalf("enter on the picker ran %d commands, want 1", len(ran))
	}
	if ran[0][0] != "switch" {
		t.Errorf("enter on the switch picker ran %v", ran[0])
	}
	// The WHOLE uuid of the row the cursor was on, never the display ordinal
	// and never a prefix: the ordinal is recompacted when an account is
	// removed, so an argv built from the number on the screen moves whichever
	// credential now occupies that slot.
	if want := a.m.Snap.Rows[a.m.Cursor].Account.UUID; len(ran[0]) < 2 || ran[0][1] != want {
		t.Errorf("enter on the switch picker ran %v, want the uuid %q", ran[0], want)
	}
	if after.scr == screenPicker {
		t.Error("the picker is still up after choosing")
	}
}

// Esc leaves the picker without running anything, which is the other half of
// the two-keystroke confirm.
func TestEscapeLeavesThePickerWithoutRunningAnything(t *testing.T) {
	var ran [][]string
	o := fixtureOptions()
	o.Exec = recorder(&ran)

	a := appAt(t, o, 113, 26)
	a, _, _ = a.key(keyPress("s"))
	a, cmd, _ := a.key(keyPress("esc"))
	drain(cmd)
	if len(ran) != 0 {
		t.Fatalf("esc on the picker ran %v", ran)
	}
	if a.scr != screenPage {
		t.Error("esc did not dismiss the picker")
	}
}

// An open picker eats every key. A keystroke that fell through would act on a
// page the user cannot see.
func TestAnOpenPickerEatsThePagesKeys(t *testing.T) {
	var ran [][]string
	o := fixtureOptions()
	o.Exec = recorder(&ran)

	a := appAt(t, o, 113, 26)
	a, _, _ = a.key(keyPress("s"))
	// The daemon key, which the PAGE binds and the picker does not: a key
	// nothing binds anywhere would leave every assertion below true for the
	// wrong reason. Reaching the page would open the daemon screen, so the
	// screen this ends on is what says the keystroke was eaten.
	next, cmd, ok := a.key(keyPress("d"))
	drain(cmd)
	if ok {
		t.Error("the daemon key was answered while a picker was open")
	}
	if next.scr != screenPicker {
		t.Error("a key handled by the page reached it through an open picker")
	}
}

// The command's own words reach the panel unedited, and a command that ran is
// a command that may have changed something -- so the page is re-read at once
// rather than at the next tick.
func TestACommandsOutcomeIsShownVerbatimAndTriggersAReread(t *testing.T) {
	stubTick(t)
	o := fixtureOptions()
	o.Exec = func([]string) (int, string, string) {
		return 3, "", "that account is already the live one\n"
	}

	a := appAt(t, o, 113, 26)
	a, _, _ = a.key(keyPress("s"))
	_, cmd, _ := a.key(keyPress("enter"))

	var res result
	for _, out := range drain(cmd) {
		if m, ok := out.(ranMsg); ok {
			res = m.res
		}
	}
	next, cmd := a.Update(ranMsg{res: res})
	body := next.(App).body()
	if !strings.Contains(body, "that account is already the live one") {
		t.Fatalf("the command's own words did not reach the panel:\n%s", body)
	}
	if !strings.Contains(body, "nothing to do") {
		t.Errorf("exit 3 was not reported in the contract's own words:\n%s", body)
	}
	loads := 0
	for _, out := range drain(cmd) {
		if _, ok := out.(loadedMsg); ok {
			loads++
		}
	}
	if loads != 1 {
		t.Fatalf("a command that ran triggered %d re-reads, want 1", loads)
	}
}

// There are two providers and neither of them is what `a` has always meant, so
// the key opens a choice instead of a login. Nothing is released until somebody
// has said which one.
func TestTheAddKeyOffersAProviderChoiceRatherThanStartingALogin(t *testing.T) {
	a := appAt(t, fixtureOptions(), 113, 26)
	next, cmd, ok := a.key(keyPress("a"))
	if !ok {
		t.Fatal("the add key was not handled")
	}
	if cmd != nil {
		t.Fatal("the add key released the terminal before anybody had chosen a provider")
	}
	if next.scr != screenAddProvider {
		t.Fatalf("the add key opened screen %d, want the provider choice", next.scr)
	}
	body := next.body()
	for _, want := range []string{"Claude", "Codex"} {
		if !strings.Contains(body, want) {
			t.Errorf("the choice does not offer %s:\n%s", want, body)
		}
	}
}

// The cursor opens on the first choice with nothing marked, and it opens there
// again next time. There is no provider "in force" for an add — the mark on the
// other two pickers answers "which value is already live", and an add has no
// such value — and a remembered position would make the same keystrokes add a
// different provider on the second press than on the first.
func TestTheProviderChoiceOpensOnTheFirstChoiceWithNothingMarkedAndForgetsIt(t *testing.T) {
	a := appAt(t, fixtureOptions(), 113, 26)

	a, _, _ = a.key(keyPress("a"))
	if a.pick.cursor != 0 {
		t.Fatalf("the choice opened on item %d, want the first", a.pick.cursor)
	}
	if a.pick.current != -1 {
		t.Fatalf("item %d is marked as already in force, and no provider is in force for an add", a.pick.current)
	}
	if strings.Contains(a.body(), "* ") {
		t.Errorf("the choice drew an in-force mark:\n%s", a.body())
	}

	a, _, _ = a.key(keyPress("down"))
	if a.pick.cursor != 1 {
		t.Fatalf("down left the cursor on item %d", a.pick.cursor)
	}
	a, _, _ = a.key(keyPress("esc"))
	a, _, _ = a.key(keyPress("a"))
	if a.pick.cursor != 0 {
		t.Fatalf("the choice reopened on item %d: the provider was remembered between invocations", a.pick.cursor)
	}
}

// The three motion keys mean one thing across both lists. They are one arm
// called from both handlers rather than two copies, so up, down and esc cannot
// come to mean two things on two lists that a user moves through identically.
func TestTheProviderChoiceMovesAndCancelsLikeEveryOtherPicker(t *testing.T) {
	a := appAt(t, fixtureOptions(), 113, 26)
	a, _, _ = a.key(keyPress("a"))

	a, _, _ = a.key(keyPress("down"))
	a, _, _ = a.key(keyPress("up"))
	if a.pick.cursor != 0 {
		t.Fatalf("down then up left the cursor on item %d", a.pick.cursor)
	}
	// The ends hold rather than wrapping, for the reason the switch list's do:
	// a held key that wrapped would leave the highlight on a provider the user
	// was not looking at when they let go.
	a, _, _ = a.key(keyPress("up"))
	if a.pick.cursor != 0 {
		t.Fatalf("up past the top wrapped to item %d", a.pick.cursor)
	}

	next, cmd, ok := a.key(keyPress("esc"))
	if !ok {
		t.Fatal("esc was not handled on the provider choice")
	}
	if cmd != nil {
		t.Fatal("esc started something")
	}
	if next.scr != screenPage {
		t.Fatalf("esc left screen %d showing, want the page", next.scr)
	}
}

// Enter here RELEASES the terminal to a login, and that is why this screen has
// its own handler rather than another arm of pickerKey. Every other picker's
// enter runs its argv through the executor, which captures the command's bytes
// into a panel; a login run that way would have its code, its URL and its paste
// prompt swallowed, and the dashboard would sit blank waiting for something the
// user was never shown.
func TestEnterOnTheProviderChoiceReleasesTheTerminalRatherThanCapturingBytes(t *testing.T) {
	var ran [][]string
	o := fixtureOptions()
	o.Exec = recorder(&ran)

	a := appAt(t, o, 113, 26)
	a, _, _ = a.key(keyPress("a"))
	next, cmd, ok := a.key(keyPress("enter"))
	if !ok {
		t.Fatal("enter was not handled on the provider choice")
	}
	if cmd == nil {
		t.Fatal("enter on the provider choice started nothing")
	}
	for _, msg := range drain(cmd) {
		if _, isRan := msg.(ranMsg); isRan {
			t.Fatal("the login was put through the executor, which captures its output into a panel")
		}
	}
	if len(ran) != 0 {
		t.Fatalf("the executor ran %v", ran)
	}
	if next.scr != screenPage {
		t.Fatalf("the choice stayed on screen %d behind the released login", next.scr)
	}
}

// The row the cursor is standing on is the provider that would be added. Every
// choice is reachable from where the screen opens, and each one hands back its
// own command line rather than the list's first.
func TestTheProviderTheCursorIsOnIsTheOneThatWouldBeAdded(t *testing.T) {
	choices := addChoices()
	if len(choices) < 2 {
		t.Fatal("there is only one provider, so moving the cursor cannot change the answer")
	}
	for i, want := range choices {
		a := appAt(t, fixtureOptions(), 113, 26)
		a, _, _ = a.key(keyPress("a"))
		for range i {
			a, _, _ = a.key(keyPress("down"))
		}
		if got := a.pick.Chosen(); !slices.Equal(got, want.argv) {
			t.Errorf("%d down from the top the choice is %v, want %s's %v", i, got, want.label, want.argv)
		}
	}
}

// The login that opens is the one the cursor was standing on when enter was
// pressed, and this reads the command line that is ACTUALLY released rather
// than the one the picker would have handed over.
//
// Those are two different claims, and only this one is the user's: a handler
// that consulted the list instead of the cursor would leave every assertion
// about the picker green while enter on Codex opened the Claude login. The
// library wraps the child in a message it does not export, so the release goes
// through a package var to be readable at all.
func TestTheLoginThatIsReleasedIsTheProviderTheCursorWasOn(t *testing.T) {
	var released []string
	old := execProcess
	execProcess = func(c *exec.Cmd, fn tea.ExecCallback) tea.Cmd {
		released = slices.Clone(c.Args[1:])
		return func() tea.Msg { return addFinishedMsg{} }
	}
	t.Cleanup(func() { execProcess = old })

	choices := addChoices()
	if len(choices) < 2 {
		t.Fatal("there is only one provider, so moving the cursor cannot change the login")
	}
	for i, want := range choices {
		released = nil
		a := appAt(t, fixtureOptions(), 113, 26)
		a, _, _ = a.key(keyPress("a"))
		for range i {
			a, _, _ = a.key(keyPress("down"))
		}
		_, cmd, ok := a.key(keyPress("enter"))
		if !ok || cmd == nil {
			t.Fatalf("enter on %s was not handled or released nothing", want.label)
		}
		if !slices.Equal(released, want.argv) {
			t.Errorf("enter on %s released %v, want %v", want.label, released, want.argv)
		}
	}
}

// Every screen a keypress can arrive in is probed, or Handles answers for a
// dashboard smaller than the one that ships and a keybar can advertise a key
// nothing on the reachable screens does anything with.
func TestEveryScreenAKeypressCanArriveInIsProbed(t *testing.T) {
	seen := map[screen]bool{}
	for _, a := range probes() {
		if seen[a.scr] {
			t.Errorf("two probes sit on screen %d, so some other screen has none", a.scr)
		}
		seen[a.scr] = true
	}
	for s := screenPage; s <= screenAddProvider; s++ {
		if !seen[s] {
			t.Errorf("screen %d is not probed: Handles cannot see the keys that live there", s)
		}
	}
}

// The stderr gate is asked BEFORE any choice is offered, and that order is the
// point rather than an accident of layout: it is a question about the terminal
// every login will need, not about which login it is, and offering a choice
// first would make a user pick a provider on the way to being refused.
func TestTheAddKeyRefusesARedirectedStderrWithoutSpawningAnything(t *testing.T) {
	o := fixtureOptions()
	o.StderrTTY = false

	a := appAt(t, o, 113, 26)
	next, cmd, ok := a.key(keyPress("a"))
	if !ok {
		t.Fatal("the add key was not handled")
	}
	if cmd != nil {
		t.Fatal("the add key produced a command with stderr redirected")
	}
	if !strings.Contains(next.body(), "stderr") {
		t.Fatalf("the refusal does not name the redirect:\n%s", next.body())
	}
	if next.scr != screenPanel {
		t.Errorf("the refusal was not put on screen; screen %d is showing, and a provider "+
			"choice here would ask the user to pick on the way to being refused", next.scr)
	}
}

// A sentence this package wrote stays until it is dismissed. Put on the notice
// rung it would belong to the snapshot instead, and the next successful reload
// replaces the snapshot whole -- so it would be taken away on a timer whether
// or not anybody had read it.
func TestASentenceThisPackageWroteSurvivesAReload(t *testing.T) {
	stubTick(t)
	o := fixtureOptions()
	o.StderrTTY = false

	a := appAt(t, o, 113, 26)
	a, _, _ = a.key(keyPress("a"))
	next, _ := a.Update(loadedMsg{snap: fixtureSnapshot(fixtureReport(113, 26))})
	if !strings.Contains(next.(App).body(), "stderr") {
		t.Fatal("a reload took away a sentence the loop had just written")
	}
}

// A finished login says how it ended -- the login's own prose is already gone
// with the terminal it was holding -- and re-reads the store, because an
// account may have been added.
func TestAFinishedLoginSaysHowItEndedAndRereadsTheStore(t *testing.T) {
	stubTick(t)
	a := appAt(t, fixtureOptions(), 113, 26)
	next, cmd := a.Update(addFinishedMsg{})
	if !strings.Contains(next.(App).body(), addOutcome(nil)) {
		t.Fatalf("the login's outcome is not on screen:\n%s", next.(App).body())
	}
	loads := 0
	for _, out := range drain(cmd) {
		if _, ok := out.(loadedMsg); ok {
			loads++
		}
	}
	if loads != 1 {
		t.Fatalf("a finished login triggered %d re-reads, want 1", loads)
	}
}

// A failed refresh keeps the LAST GOOD reading on screen and labels it, rather
// than emptying the table -- which is the unknown-is-zero bug in another
// costume -- or going quietly stale, which is the drift the change-stamp rule
// forbids elsewhere. The rule itself belongs to Model.AfterLoad; this is the
// loop calling it rather than inventing a second one.
func TestAFailedRefreshKeepsTheLastGoodSnapshotAndSaysSo(t *testing.T) {
	stubTick(t)
	a := appAt(t, fixtureOptions(), 113, 26)
	before := a.m.Snap

	// Through the seam rather than around it: a loadedMsg built by hand says
	// nothing about the read carrying Load's error in the first place, and
	// dropping that error is exactly the bug this test is named for.
	a.opts.Load = func(time.Time) (view.Snapshot, error) {
		return view.Snapshot{}, errors.New("store is locked")
	}
	asked, cmd := a.reloading()
	var next tea.Model = asked
	for _, msg := range drain(cmd) {
		next, _ = next.Update(msg)
	}
	got := next.(App).m.Snap
	if len(got.Rows) != len(before.Rows) {
		t.Fatalf("a failed refresh changed the row count from %d to %d; it must keep the last good snapshot",
			len(before.Rows), len(got.Rows))
	}
	found := false
	for _, n := range got.Notices {
		if strings.Contains(n, refreshFailed) && strings.Contains(n, "store is locked") {
			found = true
		}
	}
	if !found {
		t.Errorf("Notices = %v, want one line naming the failure", got.Notices)
	}
}

// Widening and narrowing must not panic, and must not leave the cursor off the
// end of the window that is actually drawn.
//
// The sizes are chosen BETWEEN the height ladder's rungs rather than on them.
// A fixture set built for the rungs cannot exercise the scrolling rung at all,
// because scrolling only starts where those fixtures stop.
func TestAResizeNeverLeavesTheCursorOutsideTheVisibleRows(t *testing.T) {
	sizes := []struct{ w, h int }{
		{113, 26}, {80, 24}, {80, 20}, {80, 13}, {80, 9}, {80, 7}, {80, 5},
		{80, 4}, {80, 3}, {56, 9}, {43, 8}, {35, 6}, {35, 4}, {35, 3},
	}
	a := appAt(t, fixtureOptions(), 113, 26)
	rows := len(a.m.Snap.Rows)
	if rows < 2 {
		t.Fatalf("the fixture pool has %d rows, which cannot exercise scrolling", rows)
	}

	for i := 0; i < rows*2; i++ {
		a, _, _ = a.key(keyPress("down"))
	}
	for _, s := range sizes {
		next, _ := a.Update(tea.WindowSizeMsg{Width: s.w, Height: s.h})
		a = next.(App)
		assertCursorIsDrawn(t, a.m, s.w, s.h)
		a.body()
	}
	for i := 0; i < rows*2; i++ {
		a, _, _ = a.key(keyPress("up"))
		assertCursorIsDrawn(t, a.m, a.m.Width, a.m.Height)
	}
}

// The scrolling rung is where Top starts moving, and it fires at sizes no
// fixture visits: the window has to be smaller than the row count before there
// is anything to scroll.
func TestTheCursorPushesTheWindowDownOnceScrollingStarts(t *testing.T) {
	a := appAt(t, fixtureOptions(), 80, 5)
	if a.m.Top != 0 {
		t.Fatalf("Top started at %d", a.m.Top)
	}
	for i := 0; i < len(a.m.Snap.Rows); i++ {
		a, _, _ = a.key(keyPress("down"))
	}
	if a.m.Top == 0 {
		t.Fatal("the cursor walked past the bottom of the window and the window never moved")
	}
	assertCursorIsDrawn(t, a.m, 80, 5)
}

// The [D] screen's keys replace the page's while it is showing, so x is stop
// there and nothing at all on the page.
func TestTheDaemonScreensKeysOnlyExistOnIt(t *testing.T) {
	var ran [][]string
	o := fixtureOptions()
	o.Exec = recorder(&ran)

	a := appAt(t, o, 113, 26)
	if _, _, ok := a.key(keyPress("x")); ok {
		t.Error("the stop key answered on the page, where it is not offered")
	}

	a, cmd, ok := a.key(keyPress("d"))
	if !ok || a.scr != screenDaemon {
		t.Fatal("the daemon key did not open the daemon screen")
	}
	drain(cmd)

	_, cmd, ok = a.key(keyPress("x"))
	if !ok {
		t.Fatal("the stop key was not handled on the daemon screen")
	}
	drain(cmd)
	if len(ran) != 1 || ran[0][0] != "daemon" || ran[0][1] != "stop" {
		t.Fatalf("the stop key ran %v", ran)
	}
}

// A lock that could not be probed is not an invitation. The binding is
// disabled, which also takes it out of the help view -- and a disabled binding
// matches nothing, so the keypress runs nothing either.
func TestTheStartKeyRunsNothingWhenTheLockCouldNotBeProbed(t *testing.T) {
	var ran [][]string
	o := unknownDaemonOptions()
	o.Exec = recorder(&ran)

	a := appAt(t, o, 113, 26)
	a, cmd, _ := a.key(keyPress("d"))
	drain(cmd)
	_, cmd, ok := a.key(keyPress("S"))
	drain(cmd)
	if ok {
		t.Error("the start key answered while the lock could not be probed")
	}
	if len(ran) != 0 {
		t.Fatalf("the start key ran %v under an unprobed lock", ran)
	}
}

// ctrl+c is checked before any screen gets a look. It is an ordinary key here
// -- nothing is released, so there is no scoped trap to fight -- but it is the
// key every terminal user tries first, and a screen that ate it would strand
// somebody inside a full-screen program.
func TestCtrlCLeavesFromEveryScreen(t *testing.T) {
	base := appAt(t, fixtureOptions(), 113, 26)
	for _, tc := range []struct {
		name string
		open string
	}{
		{"the page", ""},
		{"a picker", "s"},
		{"the daemon screen", "d"},
		{"the help view", "?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := base
			if tc.open != "" {
				a, _, _ = a.key(keyPress(tc.open))
			}
			_, cmd, ok := a.key(keyPress("ctrl+c"))
			if !ok || cmd == nil {
				t.Fatal("ctrl+c produced nothing")
			}
			if _, quit := cmd().(tea.QuitMsg); !quit {
				t.Fatal("ctrl+c did not quit")
			}
		})
	}
}

// The one headless program in this package. It exists to prove the
// construction order and that the terminal is given back, not to assert a
// frame: the renderer coalesces frames, so only the last one lands anywhere a
// test can read it.
func TestAHeadlessProgramStartsAndQuitsCleanly(t *testing.T) {
	p := tea.NewProgram(newApp(fixtureOptions()),
		tea.WithInput(nil),
		tea.WithOutput(io.Discard),
		tea.WithWindowSize(80, 24))

	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()

	// Send blocks until the loop takes the message, so this cannot outrun the
	// program's own start.
	p.Send(keyPress("q"))
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the program returned %v", err)
		}
	case <-time.After(10 * time.Second):
		p.Kill()
		t.Fatal("the program did not quit")
	}
}

// The two lines that are the whole point of keeping the page pure: the view is
// the page, and the terminal mode is a field on it rather than an option
// passed to the program.
func TestTheViewIsThePageInTheAlternateScreen(t *testing.T) {
	a := appAt(t, fixtureOptions(), 80, 24)
	v := a.View()
	if !v.AltScreen {
		t.Error("the dashboard did not ask for the alternate screen")
	}
	if v.Content != a.m.Body() {
		t.Error("the view is not the page")
	}
}

// Every key name this program binds has to survive the round trip through the
// message a terminal would send, or the predicate above answers about a key
// nobody pressed.
func TestEveryBoundKeyNameSurvivesBeingBuiltAsAMessage(t *testing.T) {
	seen := map[string]bool{}
	for _, group := range DefaultKeys().FullHelp() {
		for _, b := range group {
			for _, name := range b.Keys() {
				if seen[name] {
					continue
				}
				seen[name] = true
				if got := keyPress(name).String(); got != name {
					t.Errorf("keyPress(%q) came back as %q", name, got)
				}
			}
		}
	}
}

// armed is one scheduled wake-up: when, and what it will send when it fires.
// The message matters as much as the cadence -- two clocks whose messages were
// swapped would keep every duration in this file correct and read the store
// every second and a half.
type armed struct {
	after time.Duration
	msg   tea.Msg
}

// stubTick swaps the rescheduling seam for one that records what was asked for
// and hands back a command that does nothing. Without it a test that wanted to
// know whether a successor had been scheduled would have to wait for it.
func stubTick(t *testing.T) *[]armed {
	t.Helper()
	var got []armed
	old := tick
	tick = func(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
		got = append(got, armed{after: d, msg: fn(fixtureNow)})
		return func() tea.Msg { return nil }
	}
	t.Cleanup(func() { tick = old })
	return &got
}

// stubProgram swaps the construction seam, so a test can see what Run asked
// the library for without opening a program against a terminal go test does
// not have.
func stubProgram(t *testing.T, fn func(tea.Model, ...tea.ProgramOption) error) {
	t.Helper()
	old := program
	program = fn
	t.Cleanup(func() { program = old })
}

// appAt is an App that has been given a terminal and one good read, which is
// the state every key test wants to start from.
func appAt(t *testing.T, o Options, width, height int) App {
	t.Helper()
	snap, err := o.Load(o.Now())
	if err != nil {
		t.Fatal(err)
	}
	m, _ := newApp(o).Update(tea.WindowSizeMsg{Width: width, Height: height})
	m, _ = m.Update(loadedMsg{snap: snap})
	return m.(App)
}

// drain runs a command and every command a batch of them contains, and
// collects what they produced. Nothing here is concurrent: the point is to see
// what a keypress asked for, not to reproduce how the program runs it.
func drain(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, drain(c)...)
		}
		return out
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

// unboundKey is a binding for a key this program does not bind, which is what
// stops the predicate over the real ones from being vacuously true.
func unboundKey() key.Binding {
	return key.NewBinding(key.WithKeys("z"), key.WithHelp("z", "nothing"))
}

func recorder(into *[][]string) view.Exec {
	return func(argv []string) (int, string, string) {
		*into = append(*into, argv)
		return 0, "", ""
	}
}

// unknownDaemonOptions is the fixture pool with a lock nobody could probe.
func unknownDaemonOptions() Options {
	snap := fixtureSnapshot(daemon.Report{State: daemon.DaemonUnknown})
	o := fixtureOptions()
	o.Load = func(time.Time) (view.Snapshot, error) { return snap, nil }
	return o
}

// assertCursorIsDrawn asks the renderer's own window which rows it is about to
// draw, rather than recomputing the arithmetic, and requires the cursor to be
// among them.
func assertCursorIsDrawn(t *testing.T, m Model, width, height int) {
	t.Helper()
	n := len(m.Snap.Rows)
	if n == 0 {
		return
	}
	if m.Cursor < 0 || m.Cursor >= n {
		t.Fatalf("%dx%d: the cursor is at %d with %d rows", width, height, m.Cursor, n)
	}
	runway := m.runwayLines()
	footerWidth := m.Width - 2
	if footerWidth < 1 {
		footerWidth = m.Width
	}
	l := planWithRows(testCols(), m.Width, m.Height, n, len(m.Snap.Notices) > 0,
		len(runway) > 0, len(m.footerLines(footerWidth)), len(runway), len(m.summaryLines(m.Width)))
	if l.TooNarrow || l.TooShort {
		return
	}
	shown, _ := m.window(l)
	if m.Cursor < m.Top || m.Cursor >= m.Top+len(shown) {
		t.Fatalf("%dx%d: the cursor is at %d and the drawn window is [%d,%d)",
			width, height, m.Cursor, m.Top, m.Top+len(shown))
	}
}

// A daemon key runs its command from the daemon screen, so dismissing what it
// said goes back to the screen the reader was reading rather than to the page.
func TestDismissingAPanelGoesBackToTheScreenTheCommandRanFrom(t *testing.T) {
	stubTick(t)
	var ran [][]string
	o := fixtureOptions()
	o.Exec = recorder(&ran)

	a := appAt(t, o, 113, 26)
	a, cmd, _ := a.key(keyPress("d"))
	drain(cmd)
	_, cmd, _ = a.key(keyPress("x"))

	var res result
	for _, out := range drain(cmd) {
		if m, ok := out.(ranMsg); ok {
			res = m.res
		}
	}
	next, _ := a.Update(ranMsg{res: res})
	shown := next.(App)
	if shown.scr != screenPanel {
		t.Fatal("the daemon command's outcome was not put on screen")
	}
	back, _, _ := shown.key(keyPress("esc"))
	if back.scr != screenDaemon {
		t.Fatalf("esc from the panel landed on screen %d, want the daemon screen", back.scr)
	}
}

// A picker and the help view are only ever opened from the page, so esc on
// either goes there.
func TestEscapeFromTheHelpViewGoesBackToThePage(t *testing.T) {
	a := appAt(t, fixtureOptions(), 113, 26)
	a, _, ok := a.key(keyPress("?"))
	if !ok || a.scr != screenHelp {
		t.Fatal("the help key did not open the help view")
	}
	// The same key closes it again, which is what a reader tries first.
	a, _, _ = a.key(keyPress("?"))
	if a.scr != screenPage {
		t.Fatal("the help key did not close the help view")
	}
	a, _, _ = a.key(keyPress("?"))
	a, _, _ = a.key(keyPress("esc"))
	if a.scr != screenPage {
		t.Fatal("esc did not close the help view")
	}
}

// Quit means quit wherever it is pressed, and not "dismiss this". A key that
// meant two things depending on what was showing is one a user cannot rely on
// in a full-screen program.
func TestTheQuitKeyQuitsFromEveryScreenThatIsNotAConfirm(t *testing.T) {
	base := appAt(t, fixtureOptions(), 113, 26)
	for _, tc := range []struct{ name, open string }{
		{"the page", ""},
		{"the daemon screen", "d"},
		{"the help view", "?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := base
			if tc.open != "" {
				a, _, _ = a.key(keyPress(tc.open))
			}
			_, cmd, ok := a.key(keyPress("q"))
			if !ok || cmd == nil {
				t.Fatal("q produced nothing")
			}
			if _, quit := cmd().(tea.QuitMsg); !quit {
				t.Fatal("q did not quit")
			}
		})
	}

	// An open picker is the exception, and it is the deliberate one: it eats
	// every key it does not bind, so a keystroke cannot act on a page the user
	// is no longer looking at.
	a, _, _ := base.key(keyPress("s"))
	if _, _, ok := a.key(keyPress("q")); ok {
		t.Error("q was answered while a picker was open")
	}
}

// The key switch is reached through Update in the running program, not through
// the unexported helper every other test here calls. Nothing else says that
// Update keeps the model the switch handed back.
func TestUpdateAppliesWhatTheKeySwitchReturned(t *testing.T) {
	a := appAt(t, fixtureOptions(), 113, 26)
	next, _ := a.Update(keyPress("?"))
	if next.(App).scr != screenHelp {
		t.Fatal("a key that went through Update changed nothing")
	}
}

// The strategy key writes a setting; it must never be able to move a
// credential. Nothing but the argv says which of the two it is.
func TestTheStrategyKeyWritesASettingAndNeverMovesACredential(t *testing.T) {
	var ran [][]string
	o := fixtureOptions()
	o.Exec = recorder(&ran)

	a := appAt(t, o, 113, 26)
	a, cmd, ok := a.key(keyPress("c"))
	if !ok || a.scr != screenPicker {
		t.Fatal("the strategy key did not open a picker")
	}
	drain(cmd)
	if len(ran) != 0 {
		t.Fatalf("the strategy key ran %v before anything was chosen", ran)
	}
	_, cmd, _ = a.key(keyPress("enter"))
	drain(cmd)
	if len(ran) != 1 {
		t.Fatalf("enter on the strategy picker ran %d commands, want 1", len(ran))
	}
	got := strings.Join(ran[0], " ")
	if !strings.HasPrefix(got, "strategy ") {
		t.Fatalf("the strategy picker ran %q, want a strategy change", got)
	}
	if strings.Contains(got, "switch") {
		t.Fatalf("the strategy key ran %q, which moves a credential", got)
	}
}

// Init reads once. Without it the dashboard opens on an empty page and stays
// there for a whole reload cadence, with nothing saying why.
func TestInitReadsOnceAtStartup(t *testing.T) {
	stubTick(t)
	reads := 0
	o := fixtureOptions()
	snap := fixtureSnapshot(fixtureReport(113, 26))
	o.Load = func(time.Time) (view.Snapshot, error) { reads++; return snap, nil }

	a := newApp(o)
	var m tea.Model = a
	for _, msg := range drain(a.Init()) {
		var cmd tea.Cmd
		m, cmd = m.Update(msg)
		drain(cmd)
	}
	if reads != 1 {
		t.Fatalf("starting up read the documents %d times, want exactly 1", reads)
	}
}

// The clock comes from Options, because nothing in this package may call the
// wall clock itself -- and the read is the place that would.
func TestTheReadIsGivenTheClockFromOptions(t *testing.T) {
	stubTick(t)
	pinned := fixtureNow.Add(11 * time.Hour)
	var saw time.Time
	o := fixtureOptions()
	o.Now = func() time.Time { return pinned }
	o.Load = func(now time.Time) (view.Snapshot, error) {
		saw = now
		return fixtureSnapshot(fixtureReport(113, 26)), nil
	}
	_, cmd := newApp(o).reloading()
	drain(cmd)
	if !saw.Equal(pinned) {
		t.Fatalf("the read was given %s, want the clock Options handed out (%s)", saw, pinned)
	}
}

// A held refresh key repeats. Without a gate it reproduces exactly the cost
// profile the split cadence exists to avoid -- and it quietly requires
// Options.Load to be safe to call from twenty goroutines at once, which
// nothing has ever promised.
func TestOnlyOneReadIsEverInFlight(t *testing.T) {
	stubTick(t)
	reads := 0
	snap := fixtureSnapshot(fixtureReport(113, 26))
	o := fixtureOptions()
	o.Load = func(time.Time) (view.Snapshot, error) { reads++; return snap, nil }

	a := appAt(t, o, 113, 26)
	reads = 0 // appAt does its own read to build the page; count from here
	for i := 0; i < 20; i++ {
		var cmd tea.Cmd
		a, cmd, _ = a.key(keyPress("r"))
		drain(cmd)
	}
	if reads != 1 {
		t.Fatalf("twenty presses of the refresh key issued %d reads, want 1", reads)
	}

	// The nineteen that were refused are ONE remembered read, not nineteen:
	// whoever pressed usually pressed because something changed, and the read
	// already running may have started before it did.
	next, cmd := a.Update(loadedMsg{snap: snap})
	drain(cmd)
	if reads != 2 {
		t.Fatalf("after the read came back %d had run, want the one remembered re-read", reads)
	}
	last, cmd := next.(App).Update(loadedMsg{snap: snap})
	drain(cmd)
	if reads != 2 {
		t.Fatalf("%d reads ran; the remembered one was remembered twice", reads)
	}
	if last.(App).loading {
		t.Error("the loop still thinks a read is in flight")
	}
}

// A terminal repeats a held key, and these are the keys that change something.
func TestAHeldMutatingKeyRunsItsCommandOnce(t *testing.T) {
	stubTick(t)
	var ran [][]string
	o := fixtureOptions()
	o.Exec = recorder(&ran)

	a := appAt(t, o, 113, 26)
	a, cmd, _ := a.key(keyPress("d"))
	drain(cmd)
	for i := 0; i < 8; i++ {
		var c tea.Cmd
		a, c, _ = a.key(keyPress("x"))
		drain(c)
	}
	if len(ran) != 1 {
		t.Fatalf("eight presses of the stop key ran %d commands: %v", len(ran), ran)
	}

	// And the gate opens again once the command comes back.
	next, _ := a.Update(ranMsg{res: result{Argv: []string{"daemon", "stop"}}})
	back, _, _ := next.(App).key(keyPress("esc"))
	_, cmd, _ = back.key(keyPress("R"))
	drain(cmd)
	if len(ran) != 2 {
		t.Fatalf("the gate never reopened: %v", ran)
	}
}

// The daemon screen's own three keys, each pinned to the argv it runs. A key
// that ran the wrong verb would stop a daemon somebody asked to restart.
func TestEachDaemonKeyRunsItsOwnVerb(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want string
	}{
		{"S", "daemon start"},
		{"x", "daemon stop"},
		{"R", "daemon restart"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			stubTick(t)
			var ran [][]string
			o := fixtureOptions()
			o.Exec = recorder(&ran)

			a := appAt(t, o, 113, 26)
			a, cmd, _ := a.key(keyPress("d"))
			drain(cmd)
			_, cmd, ok := a.key(keyPress(tc.key))
			if !ok {
				t.Fatalf("%q was not handled on the daemon screen", tc.key)
			}
			drain(cmd)
			if len(ran) != 1 || strings.Join(ran[0], " ") != tc.want {
				t.Fatalf("%q ran %v, want %q", tc.key, ran, tc.want)
			}
		})
	}
}

// Esc leaves the daemon screen, and the page is underneath it unchanged.
func TestEscapeLeavesTheDaemonScreen(t *testing.T) {
	a := appAt(t, fixtureOptions(), 113, 26)
	a, cmd, _ := a.key(keyPress("d"))
	drain(cmd)
	a, _, _ = a.key(keyPress("esc"))
	if a.scr != screenPage {
		t.Fatal("esc did not leave the daemon screen")
	}
	if a.body() != a.m.Body() {
		t.Fatal("the page underneath is not the page")
	}
}

// The log is read when the screen opens, refreshed on the reload clock while
// it is up, and refreshed again after one of its keys has run -- and it is not
// read at all while the page is showing, because nothing there displays it.
func TestTheLogIsReadForTheScreenThatShowsItAndNotOtherwise(t *testing.T) {
	stubTick(t)
	a := appAt(t, fixtureOptions(), 113, 26)

	if countLogReads(a.Update(reloadMsg(fixtureNow))) != 0 {
		t.Error("the log was read while the page was showing")
	}

	a, cmd, _ := a.key(keyPress("d"))
	if len(drain(cmd)) != 1 {
		t.Fatal("opening the daemon screen did not read the log")
	}
	if countLogReads(a.Update(reloadMsg(fixtureNow))) != 1 {
		t.Error("the reload clock did not refresh the log while the screen was up")
	}

	a.back = screenDaemon
	if countLogReads(a.Update(ranMsg{res: result{Argv: []string{"daemon", "stop"}}})) != 1 {
		t.Error("a daemon command did not refresh the log it may have changed")
	}
}

// The lines the loop read are the lines the screen draws.
func TestTheLinesTheLoopReadAreTheLinesTheScreenDraws(t *testing.T) {
	a := appAt(t, fixtureOptions(), 113, 40)
	a, _, _ = a.key(keyPress("d"))
	next, _ := a.Update(logMsg{lines: []string{"a line the daemon wrote"}})
	if !strings.Contains(next.(App).body(), "a line the daemon wrote") {
		t.Fatalf("the log did not reach the screen:\n%s", next.(App).body())
	}
}

// The [D] screen may produce NEITHER half of the credential-home warning for
// itself: this process's own resolution is an environment read and the
// comparison is a filesystem one. Both come down through Options and this file
// is the only thing that carries them across, so a dropped field here switches
// the warning off with nothing else in the tree failing.
func TestTheDaemonScreenIsHandedBothHalvesOfTheCredentialHomeWarning(t *testing.T) {
	snap := fixtureSnapshot(daemon.Report{
		State:     daemon.DaemonRunning,
		HasStatus: true,
		Status: daemon.Status{
			PID:            8123,
			StartedAt:      fixtureNow.Add(-2 * time.Hour),
			CredentialHome: "/home/u/.claude",
		},
	})
	open := func(t *testing.T, fill func(*Options)) string {
		t.Helper()
		o := fixtureOptions()
		o.Load = func(time.Time) (view.Snapshot, error) { return snap, nil }
		fill(&o)
		a := appAt(t, o, 113, 40)
		a, _, _ = a.key(keyPress("d"))
		return a.body()
	}

	drifted := open(t, func(o *Options) {
		o.CredentialHome = "/home/somebody/.config/claude"
		o.SamePath = func(a, b string) bool { return a == b }
	})
	if !strings.Contains(drifted, "different logins") {
		t.Fatalf("Options.CredentialHome never reached the screen, so the warning it exists for is off:\n%s", drifted)
	}

	// The same two homes, and a comparison that knows they are one directory.
	// This is the half a passed-through CredentialHome alone would get wrong.
	spelled := open(t, func(o *Options) {
		o.CredentialHome = "/home/u/.claude/"
		o.SamePath = func(a, b string) bool { return strings.TrimSuffix(a, "/") == strings.TrimSuffix(b, "/") }
	})
	if strings.Contains(spelled, "different logins") {
		t.Fatalf("Options.SamePath never reached the screen, so two spellings of one directory read as a disagreement:\n%s", spelled)
	}
}

// A lock that could not be probed disables the start key, and that is a fact
// about the lock rather than about which screen is showing -- so the one full
// help view in this program must not offer it either.
func TestTheHelpViewDropsTheKeyTheLockDisabled(t *testing.T) {
	want := DefaultKeys().Start.Help().Key + " " + DefaultKeys().Start.Help().Desc

	live := appAt(t, fixtureOptions(), 113, 40)
	live, _, _ = live.key(keyPress("?"))
	if !strings.Contains(live.body(), want) {
		t.Fatalf("the help view does not offer %q with a running daemon:\n%s", want, live.body())
	}

	dark := appAt(t, unknownDaemonOptions(), 113, 40)
	dark, _, _ = dark.key(keyPress("?"))
	if strings.Contains(dark.body(), want) {
		t.Fatalf("the help view offers %q while the lock could not be probed:\n%s", want, dark.body())
	}
}

// The first read failing is not the same state as a refresh failing, and the
// wording has to say so: there are no last good numbers behind it, and an
// empty table under a notice promising them reads as "you have no accounts".
func TestAFirstReadThatFailsDoesNotPromiseNumbersItDoesNotHave(t *testing.T) {
	stubTick(t)
	o := fixtureOptions()
	o.Load = func(time.Time) (view.Snapshot, error) {
		return view.Snapshot{}, errors.New("store is locked")
	}

	a := newApp(o)
	var m tea.Model = a
	for _, msg := range drain(a.Init()) {
		var cmd tea.Cmd
		m, cmd = m.Update(msg)
		for _, out := range drain(cmd) {
			m, _ = m.Update(out)
		}
	}
	notices := m.(App).m.Snap.Notices
	if len(notices) != 1 {
		t.Fatalf("a first failed read left %d notices: %v", len(notices), notices)
	}
	if strings.Contains(notices[0], refreshFailed) {
		t.Errorf("a first failed read claims last good numbers that do not exist: %q", notices[0])
	}
	if !strings.Contains(notices[0], "store is locked") {
		t.Errorf("the notice does not name the failure: %q", notices[0])
	}

	// And a second failure replaces it rather than stacking.
	next, cmd := m.Update(refreshMsg{})
	for _, out := range drain(cmd) {
		next, _ = next.Update(out)
	}
	if got := next.(App).m.Snap.Notices; len(got) != 1 {
		t.Fatalf("two failed reads left %d notices: %v", len(got), got)
	}
}

// A second outcome can land while a panel is already showing -- a login
// finishing behind a command's result -- and a panel whose way out is the
// panel is one esc can never leave.
func TestASecondOutcomeCannotMakeThePanelItsOwnWayOut(t *testing.T) {
	stubTick(t)
	a := appAt(t, fixtureOptions(), 113, 26)
	a, cmd, _ := a.key(keyPress("d"))
	drain(cmd)

	first, _ := a.Update(ranMsg{res: result{Argv: []string{"daemon", "stop"}}})
	second, _ := first.(App).Update(addFinishedMsg{})
	got := second.(App)
	if got.back == screenPanel {
		t.Fatal("the panel's way out is the panel itself")
	}
	out, _, _ := got.key(keyPress("esc"))
	if out.scr == screenPanel {
		t.Fatal("esc cannot leave the panel")
	}
	if out.scr != screenDaemon {
		t.Fatalf("esc landed on screen %d, want the screen the first command ran from", out.scr)
	}
}

// Every screen drawn instead of the page is cut to the terminal it is drawn
// in. A picker or a panel that overflowed would push the rest off the bottom
// with nothing saying it had.
func TestEveryOverlayIsCutToTheTerminal(t *testing.T) {
	const w, h = 38, 5
	a := appAt(t, fixtureOptions(), w, h)

	long := strings.Repeat("a very wide line indeed ", 8)
	for _, tc := range []struct {
		name string
		app  App
	}{
		{"a picker", press(a, "s")},
		{"the help view", press(a, "?")},
		{"the daemon screen", press(a, "d")},
		{"a command's own output", a.showing(result{Argv: []string{"switch", "x"}, Stdout: strings.Repeat(long+"\n", 6)})},
		{"a sentence this package wrote", a.saying(addNeedsStderr)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.app.body()
			lines := strings.Split(body, "\n")
			if len(lines) > h {
				t.Errorf("%d rows in a %d-row terminal", len(lines), h)
			}
			for i, line := range lines {
				if ansi.StringWidth(line) > w {
					t.Errorf("row %d is %d columns wide in a %d-column terminal: %q",
						i, ansi.StringWidth(line), w, line)
				}
			}
		})
	}
}

// A read that comes back with fewer rows than the one before it must not leave
// the cursor pointing past the end of the table.
func TestAReadThatShrinksTheTableRepositionsTheCursor(t *testing.T) {
	stubTick(t)
	a := appAt(t, fixtureOptions(), 80, 20)
	for i := 0; i < len(a.m.Snap.Rows); i++ {
		a, _, _ = a.key(keyPress("down"))
	}
	if a.m.Cursor == 0 {
		t.Fatal("the cursor never moved")
	}

	thin := fixtureSnapshot(fixtureReport(80, 20))
	thin.Rows = thin.Rows[:1]
	next, _ := a.Update(loadedMsg{snap: thin})
	assertCursorIsDrawn(t, next.(App).m, 80, 20)

	// And all the way to nothing, which is a real state.
	empty := fixtureSnapshot(fixtureReport(80, 20))
	empty.Rows = nil
	last, _ := next.Update(loadedMsg{snap: empty})
	if got := last.(App).m.Cursor; got != 0 {
		t.Fatalf("an emptied table left the cursor at %d", got)
	}
	last.(App).body()
}

// The option list Run hands the library is empty, and that emptiness is the
// shutdown property: the two options NOT passed are the ones that would turn a
// SIGTERM in raw mode into a kill that leaves the user's next shell with no
// echo and no Ctrl-C.
func TestRunAsksTheLibraryForNoOptionsAtAll(t *testing.T) {
	var count int
	var model tea.Model
	stubProgram(t, func(m tea.Model, opts ...tea.ProgramOption) error {
		model, count = m, len(opts)
		return nil
	})
	if err := Run(fixtureOptions()); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	if count != 0 {
		t.Fatalf("Run passed %d program options; the empty list is what keeps the signal handler", count)
	}
	if _, ok := model.(App); !ok {
		t.Fatalf("Run handed the library a %T", model)
	}
}

// An interrupt is the user asking the program to stop, which is not a failure.
// The library reports SIGTERM as an ordinary quit and SIGINT as an error even
// though both take the same shutdown path, and passing that straight on would
// make two ways of asking for the same thing exit differently.
func TestAnInterruptIsNotReportedAsAFailure(t *testing.T) {
	stubProgram(t, func(tea.Model, ...tea.ProgramOption) error { return tea.ErrInterrupted })
	if err := Run(fixtureOptions()); err != nil {
		t.Fatalf("an interrupt came back as %v", err)
	}

	// Everything else still does come back. A program that swallowed its own
	// panic would leave a dead dashboard reporting success.
	boom := errors.New("the renderer fell over")
	stubProgram(t, func(tea.Model, ...tea.ProgramOption) error { return boom })
	if err := Run(fixtureOptions()); !errors.Is(err, boom) {
		t.Fatalf("Run returned %v, want the failure it was given", err)
	}
}

// press is one keystroke against a copy, for tables that want several screens
// from one starting point.
func press(a App, name string) App {
	next, _, _ := a.key(keyPress(name))
	return next
}

// countLogReads runs whatever a message asked for and counts the log reads
// among it. The read itself is real -- there is no daemon log on a test
// machine, so it comes back empty and that is the answer it should give.
func countLogReads(_ tea.Model, cmd tea.Cmd) int {
	n := 0
	for _, out := range drain(cmd) {
		if _, ok := out.(logMsg); ok {
			n++
		}
	}
	return n
}

// qualifies is the package and the identifier a node reaches for, following a
// chain of selectors down to whatever it started from. Without the chain the
// walk sees only the naive form and lets a fetch through a package-level value
// -- http.DefaultClient.Get -- past it.
func qualifies(n ast.Node) (pkg, sel string, ok bool) {
	s, is := n.(*ast.SelectorExpr)
	if !is {
		return "", "", false
	}
	if id, is := s.X.(*ast.Ident); is {
		return id.Name, s.Sel.Name, true
	}
	return qualifies(s.X)
}

// The rows the read came back with are the rows the page draws. Nothing else
// says that the snapshot crosses the seam at all -- a read that returned only
// its error would leave the dashboard drawing whatever it had before, forever,
// with the suite green.
func TestTheRowsTheReadReturnedAreTheRowsThePageDraws(t *testing.T) {
	stubTick(t)
	fresh := fixtureSnapshot(fixtureReport(113, 26))
	fresh.Rows = fresh.Rows[:2]
	fresh.Rows[0].Account.Email = "arrived@example.com"

	o := fixtureOptions()
	o.Load = func(time.Time) (view.Snapshot, error) { return fresh, nil }

	a := appAt(t, fixtureOptions(), 113, 26)
	a.opts = o
	asked, cmd := a.reloading()
	var next tea.Model = asked
	for _, msg := range drain(cmd) {
		next, _ = next.Update(msg)
	}
	got := next.(App)
	if len(got.m.Snap.Rows) != 2 {
		t.Fatalf("the page has %d rows, want the 2 the read returned", len(got.m.Snap.Rows))
	}
	// The head of the address, because the ACCOUNT column truncates the tail
	// at this width and the point here is that the row arrived at all.
	if !strings.Contains(got.body(), "arrived@example") {
		t.Fatalf("the account the read returned is not on the page:\n%s", got.body())
	}
}

// Up and down move the cursor, which is what the switch picker opens on and
// what drags the window once the table is taller than the terminal.
func TestUpAndDownMoveTheCursor(t *testing.T) {
	a := appAt(t, fixtureOptions(), 113, 26)
	if len(a.m.Snap.Rows) < 3 {
		t.Fatalf("the fixture pool has %d rows, which cannot exercise this", len(a.m.Snap.Rows))
	}
	if a.m.Cursor != 0 {
		t.Fatalf("the cursor started at %d", a.m.Cursor)
	}

	a, _, _ = a.key(keyPress("down"))
	if a.m.Cursor != 1 {
		t.Fatalf("down left the cursor at %d, want 1", a.m.Cursor)
	}
	a, _, _ = a.key(keyPress("down"))
	a, _, _ = a.key(keyPress("up"))
	if a.m.Cursor != 1 {
		t.Fatalf("down, down, up left the cursor at %d, want 1", a.m.Cursor)
	}

	// It clamps at both ends rather than wrapping. A held key that wrapped
	// past the bottom would put the switch picker on an account the user was
	// not looking at when they let go.
	for i := 0; i < len(a.m.Snap.Rows)+5; i++ {
		a, _, _ = a.key(keyPress("up"))
	}
	if a.m.Cursor != 0 {
		t.Fatalf("the cursor wrapped past the top to %d", a.m.Cursor)
	}
	for i := 0; i < len(a.m.Snap.Rows)+5; i++ {
		a, _, _ = a.key(keyPress("down"))
	}
	if want := len(a.m.Snap.Rows) - 1; a.m.Cursor != want {
		t.Fatalf("the cursor wrapped past the bottom to %d, want %d", a.m.Cursor, want)
	}
}

// The field has to be carried across, or the screen compares a release against
// an empty string and is silent forever on the one surface that reads it.
func TestTheDaemonScreenIsHandedThisBinarysVersion(t *testing.T) {
	a := App{m: Model{Snap: view.Snapshot{Version: "0.6.1"}}}
	if got := a.daemonScreen().Version; got != "0.6.1" {
		t.Errorf("daemonScreen().Version = %q, want the snapshot's %q", got, "0.6.1")
	}
}

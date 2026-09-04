package cli

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/theme"
	"github.com/Kweiza/ccdaddy/internal/tui"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// stubProgram swaps runProgram for a stub that returns immediately with canned
// output, so a test can drive the interactive branch of runTui without ever
// opening a real tea.Program against a terminal `go test` does not have.
//
// Without this seam, TestBareCcdadIsADashboardOnlyWhenStdoutAndStdinAreBothTTYs's
// both-TTY row would launch a live Program the moment bare `ccdad` starts
// calling this code path.
func stubProgram(t *testing.T, fn func(tui.Options) error) {
	t.Helper()
	old := runProgram
	runProgram = fn
	t.Cleanup(func() { runProgram = old })
}

// The dashboard and `ccdad status` read the same documents through the same
// function, so a row that disagrees is a second read order that got in. This is
// the test that would catch it: json_contract_test.go walks the cobra tree, so
// a renderer outside the tree is invisible to it.
func TestTheDashboardAndStatusDescribeEveryAccountTheSameWay(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	stubDaemon(t, daemon.Report{State: daemon.DaemonRunning}, nil)
	seedAccount(t, "uuid-a", "work@example.com")
	seedAccount(t, "uuid-b", "spare@example.com")
	stubTTYs(t, true, true)
	stubProgram(t, func(o tui.Options) error {
		_, err := tui.Render(o)
		return err
	})

	_, dash, _, _ := runRoot(t)
	_, status, _, _ := runRoot(t, "status")
	for _, want := range []string{"work@example.com", "spare@example.com"} {
		if !strings.Contains(dash, want) || !strings.Contains(status, want) {
			t.Errorf("%q is in one rendering and not the other", want)
		}
	}
}

// The seam itself: swapping runProgram is what makes the interactive branch
// observable from a test at all.
func TestTheInteractiveBranchCallsTheStubbableSeamNotTuiRunDirectly(t *testing.T) {
	isolate(t)
	stubTTYs(t, true, true)
	called := false
	stubProgram(t, func(tui.Options) error { called = true; return nil })
	runRoot(t)
	if !called {
		t.Fatal("the interactive branch did not go through runProgram")
	}
}

// A live Program owns the terminal, and loadSnapshot writes every notice to
// cmd.ErrOrStderr() as well as into the Snapshot. Under bubbletea those lines
// land on the alternate screen through a writer the Program cannot redraw over
// -- once per refresh tick, for as long as the condition lasts -- so the
// interactive half silences the stream and renders them from Snapshot.Notices,
// which is what loadSnapshot's own comment says that copy is for.
func TestTheInteractiveHalfKeepsNoticesOffTheTerminalItDoesNotOwn(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	stubDaemon(t, daemon.Report{}, errors.New("the lock could not be probed"))
	seedAccount(t, "uuid-a", "work@example.com")
	stubTTYs(t, true, true)

	var loaded view.Snapshot
	stubProgram(t, func(o tui.Options) error {
		// The event loop reloads on every tick; one call is enough to show
		// where the notices go.
		snap, err := o.Load(o.Now())
		loaded = snap
		return err
	})

	_, _, stderr, top := runRoot(t)
	if stderr != "" || top != "" {
		t.Fatalf("the interactive half wrote onto the terminal the Program owns:\nstderr: %q\ntop: %q", stderr, top)
	}
	if len(loaded.Notices) == 0 {
		t.Fatal("the notice was silenced without reaching Snapshot.Notices, so the page has no way to show it either")
	}
}

// The executor is the whole interaction layer's contract, and it is V0's
// freshRootExec -- this test exercises it through this package's call site
// rather than re-testing exec.go's own unit tests for it. SetArgs must never be
// omitted: cobra reads os.Args[1:] when it is nil, and for a dashboard launched
// as bare `ccdad` that re-enters the dashboard from inside itself.
func TestTheExecutorAlwaysPassesItsArgumentsExplicitly(t *testing.T) {
	isolate(t)
	seedAccount(t, "uuid-a", "work@example.com")
	ex := freshRootExec(NewRootCmd())
	code, out, _ := ex([]string{"which", "--json"})
	if out == "" {
		t.Fatalf("the executor captured no stdout (code %d); SetOut is missing or SetArgs read os.Args", code)
	}
}

// Exit 5 is a full valid payload and a negative answer, not a failure. The
// executor passes the code through untouched rather than folding it into 1,
// which is what lets a key on the dashboard render the same distinction a shell
// would have seen.
func TestTheExecutorPassesTheExitCodeThroughUnchanged(t *testing.T) {
	isolate(t)
	seedAccount(t, "uuid-a", "work@example.com")
	// Nothing has been switched to, so `which` cannot attribute the live
	// login: a complete document on stdout, and exit 5.
	code, out, _ := freshRootExec(NewRootCmd())([]string{"which", "--json"})
	if code != int(ExitProbeNegative) {
		t.Fatalf("the executor reported %d for a negative probe, want %d:\n%s", code, ExitProbeNegative, out)
	}
	if !strings.Contains(out, "\"attributed\"") {
		t.Fatalf("exit %d came back without the document that goes with it:\n%s", code, out)
	}
}

// Options is built once for both halves, so the two cannot be handed
// differently-configured worlds -- and every field has to arrive, because
// package tui may read none of them for itself. A nil Load is the one that
// fails silently: the event loop's first tick returns "could not read the
// accounts" and the page looks like a store problem.
func TestEveryFieldPackageTuiCannotReadForItselfIsFilledIn(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedAccount(t, "uuid-a", "work@example.com")

	var out bytes.Buffer
	root := NewRootCmd()
	root.SetOut(&out)
	o := tuiOptions(root)

	if o.Load == nil {
		t.Fatal("Options.Load is nil: the page would report the store unreadable on its first tick")
	}
	if o.Exec == nil {
		t.Fatal("Options.Exec is nil: every mutating key would panic instead of running a command")
	}
	if o.Now == nil {
		t.Fatal("Options.Now is nil: nothing in package tui may call time.Now itself")
	}
	if o.Out == nil {
		t.Fatal("Options.Out is nil: the one-shot render has nowhere to write")
	}
	if o.Theme == "" {
		t.Fatal("Options.Theme is the zero Name, which means `nobody said` and paints " +
			"nothing at all: the dashboard would run permanently monochrome and never say why")
	}
	if o.GlyphSet == "" {
		t.Fatal("Options.GlyphSet is empty: the page has no vocabulary to draw from and " +
			"no word for the default either, so the glyph picker cannot even ask")
	}

	snap, err := o.Load(o.Now())
	if err != nil {
		t.Fatalf("Options.Load: %v", err)
	}
	if len(snap.Rows) != 1 {
		t.Fatalf("Options.Load returned %d rows for one seeded account", len(snap.Rows))
	}
	if !snap.Now.Equal(statusNow) {
		t.Fatalf("Options.Now = %v, want the frozen clock %v: the page must not read a second one", snap.Now, statusNow)
	}
}

// The [D] screen's credential-home warning is on, and these are the two halves
// package tui may not produce for itself: this process's own resolution, which
// is an environment read, and the comparison, which is a filesystem one.
//
// The comparison is credhome.SamePath and never ==, which is the same call
// `doctor` makes for the reason written beside it: ccdad manufactures the two
// spellings itself -- daemon.ChildEnv pins an absolute, symlink-resolved path
// into every daemon it spawns while ccpath hands this shell's own spelling back
// untouched -- so a trailing slash is enough to make a string compare tell a
// user their daemon is driving the wrong directory when it is driving exactly
// the right one. That would be a permanent false warning on the one screen a
// reader opens to find out whether their daemon is healthy.
func TestTheDashboardComparesCredentialHomesAsPathsAndNotAsStrings(t *testing.T) {
	claude := isolate(t)
	o := tuiOptions(NewRootCmd())

	if o.CredentialHome != mustPath(ccpath.CredentialHome()) {
		t.Fatalf("Options.CredentialHome = %q, want this process's own resolution %q",
			o.CredentialHome, mustPath(ccpath.CredentialHome()))
	}
	if o.SamePath == nil {
		t.Fatal("Options.SamePath is nil, so the [D] screen has no way to compare and the warning stays off forever")
	}
	// The spelling a daemon would have been handed, against the one this shell
	// resolves. A string compare calls these two different directories.
	if !o.SamePath(claude, claude+string(filepath.Separator)) {
		t.Fatal("two spellings of one directory were called a disagreement; " +
			"this is a `doctor` restart instruction printed at a healthy daemon, forever")
	}
	if o.SamePath(claude, filepath.Join(claude, "somewhere-else")) {
		t.Fatal("two genuinely different directories were called the same, which is the warning switched off")
	}
}

// tuiOptions must not be handed a clock of its own. Now is a function value so
// a caller can pin time, and the dashboard's is this package's one clock var --
// the same one `status` reads.
func TestTheDashboardReadsThePackageClockAndNotASecondOne(t *testing.T) {
	isolate(t)
	pinned := statusNow.Add(90 * time.Minute)
	freezeClock(t, pinned)

	o := tuiOptions(NewRootCmd())
	if got := o.Now(); !got.Equal(pinned) {
		t.Fatalf("Options.Now() = %v, want %v", got, pinned)
	}
}

// The configured value crosses the seam UNRESOLVED, and this is the assertion
// the whole commit exists for.
//
// Both terminals are stubbed present on purpose. That is the exact shape under
// which a tuiOptions that resolved "auto" for itself would put stdin into raw
// mode and block for up to four seconds waiting to be told what colour the
// background is -- two per stdio end, both legs run -- on the branch that opens
// a live program, which is the one
// path in this binary that must never block, and never on the redirected branch
// that the synchronous ask is actually scoped to. An implementation that
// resolved would redden both rows below on the VALUE rather than by hanging,
// because "auto" would have arrived as "dark" or "light" and "auto" would have
// arrived as "unicode" or "ascii".
func TestTheConfiguredAppearanceCrossesTheSeamUnresolved(t *testing.T) {
	isolate(t)
	writeConfig(t, "[tui]\ntheme = \"auto\"\nglyphs = \"auto\"\n")
	stubTTYs(t, true, true)

	o := tuiOptions(NewRootCmd())
	if o.Theme != theme.Auto {
		t.Fatalf("Options.Theme = %q, want %q: package cli answered a question about the "+
			"terminal's own background that belongs to the event loop and to the one-shot "+
			"render, and answered it before it knows which of the two it is on", o.Theme, theme.Auto)
	}
	if o.GlyphSet != "auto" {
		t.Fatalf("Options.GlyphSet = %q, want \"auto\": collapsing it here makes the glyph "+
			"picker's auto arm unreachable in a shipping binary, and that arm is the only "+
			"place the east-asian width fallback lives", o.GlyphSet)
	}
}

// An explicit spelling crosses untouched too, which is the same property read
// from the other end: this seam carries what the file said, and it neither
// resolves a question nor second-guesses an answer.
//
// A user who wrote `light` on a dark terminal has overruled every detector in
// this binary, in the one direction that has no other escape hatch -- a
// multiplexer that swallows the background request answers dark forever, and
// there is nothing else such a user could type.
func TestTheDashboardCarriesTheThemeAndGlyphSetTheConfigurationNames(t *testing.T) {
	isolate(t)
	writeConfig(t, "[tui]\ntheme = \"light\"\nglyphs = \"ascii\"\n")
	stubTTYs(t, false, false)

	o := tuiOptions(NewRootCmd())
	if o.Theme != theme.Light {
		t.Fatalf("Options.Theme = %q, want %q: the page would paint in a theme the user "+
			"did not choose", o.Theme, theme.Light)
	}
	if o.GlyphSet != "ascii" {
		t.Fatalf("Options.GlyphSet = %q, want \"ascii\": an explicit set is an instruction, "+
			"not a suggestion the console gets to overrule", o.GlyphSet)
	}
}

// A spelling internal/theme does not recognise resolves to auto, and NOT to the
// zero Name. The difference between those two is a page.
//
// The zero Name means "nobody said" downstream and renders with no colour
// whatsoever, so a typo that fell through to it would answer a mistyped config
// file with a permanently monochrome dashboard that never mentions why. Auto
// answers it with the default the same file would have got by saying nothing,
// which is what a user who mistyped a value was trying to configure. `ccdad
// config set` refuses the typo at the point of writing; this row is a file
// edited by hand afterwards.
func TestAThemeNobodyCanSpellFallsToAutoAndNotToTheColourlessName(t *testing.T) {
	isolate(t)
	writeConfig(t, "[tui]\ntheme = \"chartreuse\"\n")
	stubTTYs(t, false, false)

	if o := tuiOptions(NewRootCmd()); o.Theme != theme.Auto {
		t.Fatalf("Options.Theme = %q for an unspellable theme, want %q", o.Theme, theme.Auto)
	}
}

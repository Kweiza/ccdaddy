package cli

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/daemon"
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
	stubTTYs(t, false, false)

	_, dash, _, _ := runRoot(t, "tui")
	_, status, _, _ := runRoot(t, "status")
	for _, want := range []string{"work@example.com", "spare@example.com"} {
		if !strings.Contains(dash, want) || !strings.Contains(status, want) {
			t.Errorf("%q is in one rendering and not the other", want)
		}
	}
}

// An explicit verb asked for by name answers, even in a pipe. That is the
// difference from bare `ccdad`, which is a usage error there.
func TestTuiInAPipeRendersOnceAndExitsZero(t *testing.T) {
	isolate(t)
	stubTTYs(t, false, false)
	code, stdout, _, top := runRoot(t, "tui")
	if code != ExitOK {
		t.Fatalf("`ccdad tui` in a pipe = %d (%s), want 0", code, top)
	}
	if stdout == "" {
		t.Fatal("`ccdad tui` in a pipe printed nothing")
	}
}

// Zero accounts is a valid, tested state -- a fresh install, or every account
// removed -- not an accident this test happens to exercise. The table renders
// its header and the explicit "no accounts" row, never a blank body.
func TestTuiInAPipeWithNoAccountsRendersTheEmptyStateNotABlankBody(t *testing.T) {
	isolate(t)
	stubTTYs(t, false, false)
	// deliberately: no seedAccount call
	code, stdout, _, top := runRoot(t, "tui")
	if code != ExitOK {
		t.Fatalf("`ccdad tui` with no accounts = %d (%s), want 0", code, top)
	}
	if !strings.Contains(stdout, "no accounts") {
		t.Fatalf("`ccdad tui` with no accounts did not render the explicit empty-state row:\n%s", stdout)
	}
}

// The seam itself: swapping runProgram is what makes the interactive branch
// observable from a test at all.
func TestTheInteractiveBranchCallsTheStubbableSeamNotTuiRunDirectly(t *testing.T) {
	isolate(t)
	stubTTYs(t, true, true)
	called := false
	stubProgram(t, func(tui.Options) error { called = true; return nil })
	runRoot(t, "tui")
	if !called {
		t.Fatal("the interactive branch did not go through runProgram")
	}
}

// The gate is stdout AND stdin, the same one bare `ccdad` uses, and the four
// combinations rather than two: a gate written as OR, or on stdout alone, or on
// stdin alone, all agree with the interactive case and with the fully
// redirected one. Only the two mixed rows tell them apart.
//
// Every row answers -- the exit code is 0 throughout, which is what makes this
// command different from bare `ccdad`. What changes across the rows is WHICH
// half answered.
func TestTuiTakesTheInteractiveBranchOnlyWithATerminalOnBoth(t *testing.T) {
	for _, tc := range []struct {
		name          string
		stdout, stdin bool
		interactive   bool
	}{
		{"a terminal on both", true, true, true},
		{"stdout redirected: ccdad tui > out", false, true, false},
		{"stdin redirected: ccdad tui < /dev/null", true, false, false},
		{"neither, which is cron", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			freezeClock(t, statusNow)
			stubDaemon(t, daemon.Report{State: daemon.DaemonStopped}, nil)
			seedAccount(t, "uuid-a", "work@example.com")
			stubTTYs(t, tc.stdout, tc.stdin)

			program := false
			stubProgram(t, func(tui.Options) error { program = true; return nil })

			code, stdout, _, top := runRoot(t, "tui")
			if code != ExitOK {
				t.Fatalf("`ccdad tui` = %d (%s), want %d", code, top, ExitOK)
			}
			if program != tc.interactive {
				t.Fatalf("`ccdad tui` opened a Program = %v, want %v", program, tc.interactive)
			}
			if tc.interactive {
				return
			}
			if !strings.Contains(stdout, "work@example.com") {
				t.Fatalf("the one-shot render did not reach stdout:\n%s", stdout)
			}
		})
	}
}

// lipgloss v2 renders truecolor whatever it is writing to. One forgotten writer
// puts escape bytes into every redirected invocation and every CI log, and
// until this feature there was not one escape byte in this repository.
//
// Read this one honestly: every style in package tui is unset today and the
// gauge's two colours are lipgloss.NoColor, so a `ccdad tui` that wrote
// STRAIGHT to stdout would produce byte-identical output and pass this. It is
// the end-to-end property, and it is worth keeping because it is the one that
// breaks the day a colour lands. What pins the WIRING before that day is
// TestTuiHonoursNoColor below, which reads the writer rather than the bytes.
func TestTuiWritesNoEscapeBytesIntoAPipe(t *testing.T) {
	isolate(t)
	stubTTYs(t, false, false)
	_, stdout, _, _ := runRoot(t, "tui")
	if strings.ContainsRune(stdout, 0x1b) {
		t.Fatalf("`ccdad tui` wrote an escape byte into a pipe: %q", stdout)
	}
}

// NO_COLOR is the user's explicit instruction, and it reaches the dashboard
// only if the one-shot render goes through colorWriter at all.
//
// This asks the WRITER rather than the output, for the reason stated above: the
// page carries no colour yet, so an assertion on bytes cannot tell a render
// that honoured NO_COLOR from one that never had any colour to strip, nor from
// one that bypassed the writer entirely. Capturing what runTui handed the
// renderer answers all three.
//
// TTY_FORCE is colorprofile's own escape hatch for a destination that can never
// satisfy term.File, and the control case is half the test: "profile <= ASCII"
// is equally what NO_COLOR flooring a colour-capable profile looks like and
// what TTY_FORCE silently failing looks like.
func TestTuiHonoursNoColor(t *testing.T) {
	profileOfTheRender := func(t *testing.T) colorprofile.Profile {
		t.Helper()
		var got io.Writer
		saved := colorWriter
		t.Cleanup(func() { colorWriter = saved })
		colorWriter = func(w io.Writer) io.Writer {
			got = saved(w)
			return got
		}
		stubTTYs(t, false, false)
		if code, _, _, top := runRoot(t, "tui"); code != ExitOK {
			t.Fatalf("`ccdad tui` = %d (%s)", code, top)
		}
		w, ok := got.(*colorprofile.Writer)
		if !ok {
			t.Fatalf("the one-shot render did not go through colorWriter (got %T); "+
				"lipgloss v2 emits truecolor whatever the destination, so the first "+
				"colour to land in package tui would go straight into a pipe", got)
		}
		return w.Profile
	}

	t.Run("control: a colour-capable destination", func(t *testing.T) {
		isolate(t)
		t.Setenv("TERM", "xterm-256color")
		t.Setenv("TTY_FORCE", "1")
		t.Setenv("CLICOLOR_FORCE", "")
		t.Setenv("COLORTERM", "")
		// Empty rather than absent: t.Setenv has no unset, and empty is what
		// colorprofile's ParseBool reads as "not set".
		t.Setenv("NO_COLOR", "")
		if p := profileOfTheRender(t); p <= colorprofile.ASCII {
			t.Fatalf("control: profile is %v, which carries no colour -- TTY_FORCE did not "+
				"force a colour-capable destination, so the NO_COLOR case below would "+
				"pass even if NO_COLOR did nothing at all", p)
		}
	})

	t.Run("NO_COLOR=1 floors the same destination", func(t *testing.T) {
		isolate(t)
		t.Setenv("TERM", "xterm-256color")
		t.Setenv("TTY_FORCE", "1")
		t.Setenv("CLICOLOR_FORCE", "")
		t.Setenv("COLORTERM", "")
		t.Setenv("NO_COLOR", "1")
		if p := profileOfTheRender(t); p > colorprofile.ASCII {
			t.Fatalf("NO_COLOR=1 left the dashboard's writer at %v, which still carries colour", p)
		}
	})
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

	_, _, stderr, top := runRoot(t, "tui")
	if stderr != "" || top != "" {
		t.Fatalf("the interactive half wrote onto the terminal the Program owns:\nstderr: %q\ntop: %q", stderr, top)
	}
	if len(loaded.Notices) == 0 {
		t.Fatal("the notice was silenced without reaching Snapshot.Notices, so the page has no way to show it either")
	}
}

// The one-shot half is not the Program, and it keeps `status`'s behaviour: the
// notices go to stderr in full. The page's own notice rung shows the FIRST one
// and a count of the rest, so a caller who redirected stdout still has
// somewhere to read all of them.
func TestTheOneShotHalfStillReportsItsNoticesOnStderr(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	stubDaemon(t, daemon.Report{}, errors.New("the lock could not be probed"))
	seedAccount(t, "uuid-a", "work@example.com")
	stubTTYs(t, false, false)

	code, _, stderr, top := runRoot(t, "tui")
	if code != ExitOK {
		t.Fatalf("`ccdad tui` in a pipe = %d (%s), want 0", code, top)
	}
	if !strings.Contains(stderr, "Cannot tell whether a daemon is running") {
		t.Fatalf("the one-shot half swallowed the notice `ccdad status` prints:\n%s", stderr)
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

// The dashboard declares no --json flag, so the JSON contract walk stays silent
// about it. That silence is a decision: its output is a rendering, and
// `ccdad status --json` is already the document form.
func TestTheDashboardDeclaresNoJsonFlag(t *testing.T) {
	c, _, err := NewRootCmd().Find([]string{"tui"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Flags().Lookup("json") != nil {
		t.Fatal("`ccdad tui` declares --json, which pulls it into four contract rules it cannot satisfy")
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
	c, _, err := root.Find([]string{"tui"})
	if err != nil {
		t.Fatal(err)
	}
	o := tuiOptions(c)

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

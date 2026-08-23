package cli

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/buildinfo"
	"github.com/Kweiza/ccdaddy/internal/daemon"
)

func TestRootVersionFlag(t *testing.T) {
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}
	if got := out.String(); !strings.Contains(got, buildinfo.Version) {
		t.Fatalf("--version output %q does not contain version %q", got, buildinfo.Version)
	}
}

func TestRootUnknownCommandIsUsageError(t *testing.T) {
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"definitely-not-a-command"})

	err := ExecuteCmd(cmd)
	if err == nil {
		t.Fatal("ExecuteCmd() = nil, want an error")
	}
	if got := CodeFor(err); got != ExitUsage {
		t.Fatalf("CodeFor(unknown command) = %d, want %d", got, ExitUsage)
	}
}

func TestRootUnknownSubcommandThroughFind(t *testing.T) {
	root := NewRootCmd()
	// NewRootCmd now registers real subcommands, so Cobra's Find() evaluates
	// unknown-command detection natively and this test exercises the
	// normalization path rather than the root's own RunE.
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"definitely-not-a-command"})

	err := ExecuteCmd(root)
	if err == nil {
		t.Fatal("ExecuteCmd() = nil, want an error")
	}
	if got := CodeFor(err); got != ExitUsage {
		t.Fatalf("CodeFor(unknown subcommand via Find()) = %d, want %d", got, ExitUsage)
	}
}

func TestExecuteWithUnknownSubcommandThroughFind(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"bogus-subcommand"})

	var errBuf bytes.Buffer
	code := ExecuteWith(root, &errBuf)
	if code != ExitUsage {
		t.Fatalf("ExecuteWith(unknown subcommand) = %d, want %d", code, ExitUsage)
	}
}

func TestExecuteWithSuccessIsOKAndPrintsNothing(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"--version"})

	var errBuf bytes.Buffer
	code := ExecuteWith(root, &errBuf)
	if code != ExitOK {
		t.Fatalf("ExecuteWith(--version) = %d, want %d", code, ExitOK)
	}
	if got := errBuf.String(); got != "" {
		t.Fatalf("error buffer = %q, want empty", got)
	}
}

func TestExecuteWithErrorWrittenToErrOut(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"unknown-cmd"})

	var errBuf bytes.Buffer
	code := ExecuteWith(root, &errBuf)
	if code != ExitUsage {
		t.Fatalf("ExecuteWith(unknown) code = %d, want %d", code, ExitUsage)
	}
	if got := errBuf.String(); !strings.Contains(got, "ccdad: ") {
		t.Fatalf("error buffer %q does not contain %q", got, "ccdad: ")
	}
}

func TestExecuteWithEPIPEIsOK(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{})
	// Replace the RunE to return EPIPE error
	root.RunE = func(*cobra.Command, []string) error {
		return fmt.Errorf("writing output: %w", errBrokenPipeForTest)
	}

	var errBuf bytes.Buffer
	code := ExecuteWith(root, &errBuf)
	if code != ExitOK {
		t.Fatalf("ExecuteWith(EPIPE) = %d, want %d", code, ExitOK)
	}
	if got := errBuf.String(); got != "" {
		t.Fatalf("error buffer = %q, want empty on EPIPE", got)
	}
}

// Cobra's generated completion command answers an unknown shell by printing its
// own help and exiting 0, which puts help text on stdout — a caller doing
// `ccdad completion "$SHELL" > _ccdad` gets help text in the completion file.
func TestCompletionWithAnUnknownShellIsUsageError(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"completion", "nonsense"})
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)

	code := ExecuteWith(root, &errBuf)
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	if strings.Contains(out.String(), "Usage:") {
		t.Fatalf("help text went to stdout: %q", out.String())
	}
}

// Spec §5.1 requires the stability contract in --help. A --json consumer that
// keys on idx breaks the first time an account is removed.
func TestRootHelpStatesTheStabilityContract(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"--help"})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"idx", "ordinal", "uuid"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("--help does not state the stability contract (missing %q):\n%s", want, out.String())
		}
	}
}

// stubTTYs describes the two terminals a test is not attached to. Both axes
// have to be stubbed for a bare `ccdad`: the gate is stdout AND stdin, so a
// test that pinned only one would pass under an implementation that read only
// the other.
func stubTTYs(t *testing.T, stdout, stdin bool) {
	t.Helper()
	stubStdoutTTY(t, stdout)
	stubEnvironment(t, stdin, false)
}

// §9.2, and the four combinations rather than two: a gate written as OR, or on
// stdout alone, or on stdin alone, all agree with the interactive case and with
// the fully-redirected one. Only the two mixed rows tell them apart.
//
// The non-interactive answer is a USAGE ERROR from the first release that has
// this slot at all, so no script can ever come to depend on interactive output
// — which is what makes the later flip from `status` to a TUI a widening rather
// than a break.
func TestBareCcdadIsADashboardOnlyWhenStdoutAndStdinAreBothTTYs(t *testing.T) {
	cases := []struct {
		name          string
		stdout, stdin bool
		want          ExitCode
	}{
		{"a terminal on both", true, true, ExitOK},
		{"stdout redirected: ccdad > out", false, true, ExitUsage},
		{"stdin redirected: ccdad < /dev/null", true, false, ExitUsage},
		{"neither, which is cron", false, false, ExitUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			freezeClock(t, statusNow)
			stubDaemon(t, daemon.Report{State: daemon.DaemonStopped}, nil)
			seedAccount(t, "uuid-a", "work@example.com")
			stubTTYs(t, tc.stdout, tc.stdin)

			code, stdout, stderr, top := runRoot(t)
			if code != tc.want {
				t.Fatalf("bare ccdad = %d (%s%s), want %d", code, stderr, top, tc.want)
			}
			if tc.want == ExitOK {
				if !strings.Contains(stdout, "work@example.com") {
					t.Fatalf("the dashboard did not render on stdout:\n%s", stdout)
				}
				return
			}
			if !strings.Contains(stderr, "Usage:") {
				t.Fatalf("usage did not go to stderr:\n%s", stderr)
			}
			if stdout != "" {
				t.Fatalf("a non-interactive bare ccdad wrote to stdout, which is what a script would capture:\n%s", stdout)
			}
		})
	}
}

// The interactive half is `status` itself, not a second renderer that agrees
// with it today. runStatus exists factored out for exactly this, and the
// assertion is equality rather than a keyword: a near-copy would pass any
// "contains a table" check and then drift on the first change to either.
func TestBareCcdadRendersTheStatusDashboardItselfPlusAFooter(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	stubDaemon(t, daemon.Report{State: daemon.DaemonStopped}, nil)
	seedAccount(t, "uuid-a", "work@example.com")
	stubTTYs(t, true, true)

	_, viaStatus, _, _ := runRoot(t, "status")
	code, bare, _, top := runRoot(t)
	if code != ExitOK {
		t.Fatalf("bare ccdad = %d (%s), want %d", code, top, ExitOK)
	}
	if !strings.HasPrefix(bare, viaStatus) {
		t.Fatalf("bare ccdad is not the status dashboard.\nbare:\n%s\nstatus:\n%s", bare, viaStatus)
	}
	footer := strings.TrimPrefix(bare, viaStatus)
	for _, verb := range topVerbs {
		if !strings.Contains(footer, verb) {
			t.Fatalf("the footer does not name %q:\n%s", verb, footer)
		}
	}
	if lines := strings.Count(strings.TrimSpace(footer), "\n"); lines != 0 {
		t.Fatalf("§9.2 asks for a ONE-line footer; got %d:\n%s", lines+1, footer)
	}
}

// A footer that names a verb the binary does not have is worse than no footer,
// and nothing else in the tree would notice: it is a string, printed to a
// terminal, that no other test reads.
func TestTheDashboardFooterNamesOnlyRealCommands(t *testing.T) {
	root := NewRootCmd()
	// A for-all over an empty list is vacuously true, and so is the loop in the
	// dashboard test above: emptying topVerbs ships a footer reading `Verbs:  `
	// past both of them. §9.2 asks for a footer OF THE TOP VERBS, so the list
	// having members is part of what is being checked, not a precondition.
	if len(topVerbs) == 0 {
		t.Fatal("topVerbs is empty, so this test and the footer both assert nothing")
	}
	for _, verb := range topVerbs {
		found, _, err := root.Find([]string{verb})
		if err != nil || found == root {
			t.Fatalf("the footer offers `ccdad %s`, which is not a command", verb)
		}
	}
}

// Cobra's Windows-only mousetrap fires before any of this runs: launched from
// Explorer it prints its own message, waits for a keypress and exits 1, which
// bypasses the §9.2 gate entirely. Emptying the text is how Cobra is told not
// to — and on the machine this test runs on, the assignment is the only
// observable half of it.
func TestTheExplorerMousetrapIsNeutered(t *testing.T) {
	NewRootCmd()
	if cobra.MousetrapHelpText != "" {
		t.Fatalf("cobra.MousetrapHelpText = %q, want empty: a double-clicked ccdad.exe must fall through to the §9.2 gate", cobra.MousetrapHelpText)
	}
}

// The gate is for the BARE invocation. An explicit request for help or for the
// version is answered on stdout at exit 0 wherever it is run, which is what
// `ccdad --version | cut -d' ' -f2` in an install script depends on.
func TestAnExplicitHelpOrVersionIsAnsweredWithNoTerminal(t *testing.T) {
	for _, flag := range []string{"--help", "--version"} {
		t.Run(flag, func(t *testing.T) {
			isolate(t)
			stubTTYs(t, false, false)

			code, stdout, _, top := runRoot(t, flag)
			if code != ExitOK {
				t.Fatalf("`ccdad %s` = %d (%s), want %d", flag, code, top, ExitOK)
			}
			if stdout == "" {
				t.Fatalf("`ccdad %s` printed nothing on stdout", flag)
			}
		})
	}
}

// autostart.go left this decision to this task. The dashboard is `ccdad status`
// under another name and status IS on the allow-list, so the interactive half
// starts an engine for the same reason it does — and the usage-error half must
// not, or `ccdad | head` in a script spawns a daemon while returning 2.
func TestBareCcdadAutoStartsOnlyOnTheDashboardHalf(t *testing.T) {
	cases := []struct {
		name          string
		stdout, stdin bool
		want          int
	}{
		{"a terminal on both", true, true, 1},
		{"in a pipe", false, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			freezeClock(t, statusNow)
			f := stubDaemonWorld(t, &fakeDaemon{})
			enableAutoStart(t)
			stubTTYs(t, tc.stdout, tc.stdin)

			runRoot(t)
			if f.spawns != tc.want {
				t.Fatalf("bare ccdad spawned %d daemons, want %d", f.spawns, tc.want)
			}
		})
	}
}

// §10.3: "SetConsoleMode VT at startup, console only, never in the daemon".
// The daemon's stdout is a log file rather than a console, so the call would be
// a harmless no-op there — but that is a property of how Spawn redirects, and
// this is the process that must not touch a console mode it does not own.
func TestConsoleVTIsEnabledForACommandAndNeverForTheDaemon(t *testing.T) {
	var handles []*os.File
	saved := consoleVT
	t.Cleanup(func() { consoleVT = saved })
	consoleVT = func(f *os.File) error { handles = append(handles, f); return nil }

	enableConsoleVT(nil)
	if len(handles) != 1 || handles[0] != os.Stdout {
		t.Fatalf("bare ccdad enabled VT on %v, want exactly [os.Stdout]", handles)
	}
	enableConsoleVT([]string{"status"})
	if len(handles) != 2 {
		t.Fatalf("`ccdad status` enabled VT %d times in total, want 2", len(handles))
	}
	enableConsoleVT([]string{daemon.RunArg})
	if len(handles) != 2 {
		t.Fatalf("`ccdad %s` enabled VT on its own output: §10.3 is console only, never in the daemon", daemon.RunArg)
	}
}

// Cobra's stripFlags drops `--`, a lone `-` and an empty argument before Find
// looks for a subcommand, so these reach the root's own RunE with the rest of
// the command line still in args: `ccdad -- list` is not `ccdad list`, and
// nothing downstream ever sees the word. Swallowing that into the dashboard
// reports success for a command that never ran — and, since the dashboard half
// auto-starts, leaves an engine behind for it.
//
// Both terminals are stubbed present deliberately: that is the shape that would
// otherwise render, so the refusal is being tested rather than the TTY gate.
func TestBareCcdadRefusesAnArgumentItCannotDispatch(t *testing.T) {
	for _, args := range [][]string{{"--", "list"}, {"-"}, {""}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			isolate(t)
			freezeClock(t, statusNow)
			f := stubDaemonWorld(t, &fakeDaemon{})
			enableAutoStart(t)
			stubTTYs(t, true, true)

			code, stdout, stderr, top := runRoot(t, args...)
			if code != ExitUsage {
				t.Fatalf("`ccdad %s` = %d (%s%s), want %d", strings.Join(args, " "), code, stderr, top, ExitUsage)
			}
			if strings.Contains(stdout, "Daemon:") {
				t.Fatalf("`ccdad %s` rendered the dashboard for an argument it discarded:\n%s", strings.Join(args, " "), stdout)
			}
			if f.spawns != 0 {
				t.Fatalf("`ccdad %s` auto-started %d daemons for an invocation it refused", strings.Join(args, " "), f.spawns)
			}
		})
	}
}

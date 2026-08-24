package cli

import (
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/credhome"
	"github.com/Kweiza/ccdaddy/internal/tui"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// newTuiCmd is the terminal dashboard under the name a user types for it.
//
// It declares NO --json flag, and that silence is a decision rather than an
// omission. Its output is a rendering; `ccdad status --json` is the document
// form and already exists. TestJSONContractCoversEveryJSONCommand fires only
// for a command that declares the flag, so adding one here would pull a
// full-screen program into four contract rules it cannot honestly satisfy.
func newTuiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Open the dashboard",
		Long: "tui is the interactive dashboard: the accounts, their quota, the daemon's\n" +
			"own state, and a key for each of the things a reader of it does next.\n\n" +
			"It reads what is already on disk and never fetches -- the usage endpoint\n" +
			"allows roughly 28-30 requests per identity per rolling hour on a sliding\n" +
			"window, so a dashboard that polled would let one burst saturate an account\n" +
			"for a full hour.\n\n" +
			"Every key that changes something runs the ordinary command for it, so it\n" +
			"gets the same refusals, the same wording and the same exit codes you would\n" +
			"see typing it. Bare `ccdad` opens this too.\n\n" +
			"With stdout redirected it renders the dashboard once and exits.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runTui,
	}
}

// runProgram is tui.Run, as a package var beside stdoutIsTTY and consoleVT --
// the same idiom, for the same reason: a real tea.Program is not something a
// test can arrange, and the decision hanging on it (does bare `ccdad` launch
// one) is exactly the class that has to be exercised without actually
// launching one. runTui calls THIS, never tui.Run.
var runProgram = tui.Run

// runTui is the dashboard, factored out of the command so bare `ccdad` can
// dispatch to exactly this and not to a near-copy of it.
//
// The split is stdout AND stdin, the same gate bare `ccdad` uses and for a
// reason that is stronger here: a full-screen program needs a terminal on
// stdin even more than a printed dashboard did.
//
// Off a terminal it ANSWERS rather than refusing, and that is the whole
// difference from bare `ccdad`, which keeps its usage error there: this one is
// an explicit verb somebody asked for by name. It renders once, through the
// colour writer, and exits 0.
func runTui(cmd *cobra.Command, _ []string) error {
	if stdoutIsTTY() && stdinIsTTY() {
		// loadSnapshot writes every notice to cmd.ErrOrStderr() as well as
		// into the Snapshot it returns. That is right for `status` and wrong
		// for a program that owns the terminal: Load runs again on every
		// refresh tick, from inside the event loop, and each of those lines
		// would be painted across the page by a writer bubbletea knows
		// nothing about and cannot redraw over. The page has a notice rung of
		// its own and the Snapshot carries the copy it draws from -- which is
		// what loadSnapshot's own comment says that copy is for -- so this
		// half takes them from there and silences the stream.
		cmd.SetErr(io.Discard)
		return runProgram(tuiOptions(cmd))
	}
	_, err := tui.Render(tuiOptions(cmd))
	return err
}

// tuiOptions is everything package tui may not read for itself, built once so
// the two halves above cannot be handed differently-configured worlds.
//
// The executor is freshRootExec, from internal/cli/exec.go, and this file
// defines none of its own: the three refusals that seam exists to hold --
// never omit SetArgs, never omit SetOut/SetErr, never call cli.Execute() in a
// handler -- are stated once there, and a second copy here is exactly the
// drift internal/view.Exec was introduced to prevent.
//
// Load is loadSnapshot, called rather than reimplemented. Task V0 factored
// `status`'s read sequence out for this, and a second read order here would be
// a second chance to derive a number differently -- which json_contract_test's
// walk of the cobra tree would never see, because a renderer is not a command.
func tuiOptions(cmd *cobra.Command) tui.Options {
	return tui.Options{
		Load: func(now time.Time) (view.Snapshot, error) {
			// probeErr is dropped rather than folded into the error. It is the
			// daemon probe's own value, which `status --json` needs and a
			// rendering does not: loadSnapshot has already turned it into the
			// sentence in Snapshot.Notices, which is what the page draws.
			snap, _, err := loadSnapshot(cmd, now)
			return snap, err
		},
		Exec: freshRootExec(cmd),
		Now:  timeNow,
		// Out is where the ONE-SHOT render writes, and it is filled for both
		// halves because tui.Run ignores it: a live Program probes the
		// terminal it was given and makes the colour decision for itself,
		// which is why colorWriter's own header says a Program does not go
		// through here.
		Out:       colorWriter(cmd.OutOrStdout()),
		StderrTTY: stderrIsTTY(),
		// The [D] screen's credential-home warning, in the two pieces package
		// tui may not produce for itself: this process's own resolution, which
		// is an environment read, and the comparison, which is a filesystem
		// one. An unresolvable home is left empty, which omits the warning --
		// the honest answer for a caller that cannot answer.
		CredentialHome: credentialHomeOrEmpty(),
		SamePath:       credhome.SamePath,
	}
}

// credentialHomeOrEmpty is ccpath.CredentialHome with its error spent here.
//
// A home that cannot be resolved has nothing to compare, and the [D] screen's
// contract for that is an empty string rather than a sentence: it is one line
// on a screen full of them, and the ways this fails -- no home directory at all
// -- are already reported by every other row in `ccdad doctor` in words that
// say what to do about it.
func credentialHomeOrEmpty() string {
	home, err := ccpath.CredentialHome()
	if err != nil {
		return ""
	}
	return home
}

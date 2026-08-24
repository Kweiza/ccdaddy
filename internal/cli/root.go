package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/buildinfo"
)

// NewRootCmd builds the ccdad command tree. It is a constructor rather than a
// package-level var so tests can build an isolated tree with its own output
// buffers and argument list.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "ccdad",
		Short: "Claude Code Daemon: Always Drilling Don't Yap",
		Long: "ccdad manages multiple Claude Code accounts and switches between them\n" +
			"before a rate limit stops you.\n\n" +
			"Stability contract: idx is a display ordinal, not a key. It is recompacted\n" +
			"when an account is removed. Scripts must reference accounts by uuid or alias.",
		Version:       buildinfo.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Subcommands are registered below, so Cobra's Find() intercepts an
			// unknown subcommand before this RunE is ever reached. What is left
			// is the bare `ccdad` slot — and the handful of shapes that LOOK
			// dispatched and are not, which is why args is read here at all.
			// See runBare.
			return runBare(cmd, args)
		},
	}

	// Cobra's mousetrap fires from ExecuteC on Windows, before argument parsing
	// and before any RunE: a binary launched from Explorer prints
	// MousetrapHelpText, sleeps MousetrapDisplayDuration (5s by default) and
	// calls os.Exit(1). Two things are wrong with that here. It bypasses the TTY
	// gate on bare `ccdad` entirely, so the one invocation shape that cannot
	// reach the dashboard is the one Cobra intercepts before this binary has an
	// opinion; and 1 means "runtime failure" in the exit-code table, which makes
	// it the only exit code ccdad can produce that does not come from the exit
	// contract.
	// Emptying the text is Cobra's documented way to be told to skip it.
	//
	// What a double-clicked ccdad.exe gets instead is the honest answer: its
	// console is a terminal on both axes, so the gate renders the dashboard —
	// and the window closes when the process exits, which is what every console
	// tool started that way does. Reached through the contract rather than
	// around it.
	cobra.MousetrapHelpText = ""

	// Two hooks in one function, because cobra will only run one: it walks up
	// from the command to the first parent carrying a persistent pre-run, and
	// PersistentPreRunE wins over PersistentPreRun on the same command. Two
	// fields here would mean the second one silently never fires.
	//
	// The refusal comes first, because it decides whether this command may act
	// on the world at all. See scoped.go: inside a `ccdad run` session, a
	// command that writes Claude Code's own state writes the SESSION's copy
	// and reports success.
	//
	// Auto-start must still never fail the command it rode in on — rule 4 of
	// the five in autostart.go — and that rule now rests on autoStart's
	// signature rather than on this hook's: it returns NOTHING, so no later
	// change here can wire it into an error without changing the hook's own
	// type. Which commands it acts for is autostart.go's allow-list; this is
	// only where the tree offers it the chance.
	//
	// Bare `ccdad` is the one command auto-start deliberately does not act
	// for, and it is on the allow-list all the same: the TTY gate decides in
	// RunE whether this invocation is a dashboard or a usage error, and a hook
	// that ran first would spawn a daemon for `ccdad | head` too — a script
	// that asked for nothing, got a 2, and left an engine behind. runBare
	// calls the hook itself once it knows which half it is in.
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if err := refuseInsideScopedSession(cmd); err != nil {
			return err
		}
		if cmd == root {
			return nil
		}
		autoStart(cmd)
		return nil
	}

	// Cobra reports a mistyped subcommand and a mistyped flag as plain errors.
	// Both are usage errors under this binary's exit contract, so retag them at
	// the single point where Cobra hands them back.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return UsageError("%s", err.Error())
	})

	root.AddCommand(newAddCmd())
	root.AddCommand(newAddTokenCmd())
	root.AddCommand(newWhichCmd())
	root.AddCommand(newListCmd())
	root.AddCommand(newSwitchCmd())
	root.AddCommand(newRunCmd())
	root.AddCommand(newProbeCmd())
	root.AddCommand(newAutoCmd())
	root.AddCommand(newRemoveCmd())
	root.AddCommand(newDisableCmd())
	root.AddCommand(newEnableCmd())
	root.AddCommand(newAliasCmd())
	root.AddCommand(newMoveCmd())
	root.AddCommand(newPrimaryCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newConfigCmd())
	root.AddCommand(newHoverCmd())
	root.AddCommand(newExportCmd())
	root.AddCommand(newImportCmd())
	root.AddCommand(newBootstrapCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newDaemonCmd())
	root.AddCommand(newSetupPathCmd())
	root.AddCommand(newUninstallCmd())
	// Hidden, and registered on the root rather than under `daemon`: Spawn
	// re-execs `ccdad <daemon.RunArg>` as a single argument.
	root.AddCommand(newDaemonRunCmd())

	// Cobra adds `completion` lazily, during Execute, so it has to be
	// materialized before it can be corrected. Left alone it answers an unknown
	// shell by printing its own help and returning nil — exit 0, with help text
	// on stdout, so `ccdad completion "$SHELL" > _ccdad` writes help into the
	// completion file. A shell it cannot generate for is a usage error.
	root.InitDefaultCompletionCmd()
	for _, c := range root.Commands() {
		if c.Name() != "completion" {
			continue
		}
		shells := shellNames(c)
		c.RunE = func(*cobra.Command, []string) error {
			return UsageError("completion needs a shell to generate for: one of %s", strings.Join(shells, ", "))
		}
		c.Args = usageArgs(cobra.ArbitraryArgs)
		c.SilenceUsage, c.SilenceErrors = true, true
	}
	return root
}

// usageArgs wraps a positional-argument validator so a violation exits 2.
//
// Cobra reports an arg-count mistake as a plain error, which CodeFor maps to
// ExitFailure — indistinguishable from a network failure. The exit contract
// reserves 2 for "a bad flag, a bad flag combination, an unknown account
// reference, a missing argument", and this is the only place that class of
// error is produced.
func usageArgs(validate cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := validate(cmd, args); err != nil {
			return UsageError("%s", err.Error())
		}
		return nil
	}
}

// ExecuteCmd runs an already-built root command and maps Cobra's raw errors
// into ccdad's exit taxonomy. Execute and the tests both go through it, so the
// mapping cannot depend on which caller ran the command — Cobra reports an
// unknown subcommand from Find(), before the root's own RunE is ever reached.
func ExecuteCmd(root *cobra.Command) error {
	err := root.Execute()
	if err == nil {
		return nil
	}
	if isUnknownCommand(err) {
		return UsageError("%s", err.Error())
	}
	return err
}

// Execute builds the command tree, runs it, and returns the process exit code.
//
// SIGINT is deliberately NOT trapped here. Trapping it process-wide removes its
// default terminating disposition for every command, and only the commands that
// actually watch the context can then do anything about it — which turned
// Ctrl-C into a no-op on `switch` waiting for a credential lock and on
// `add-token` blocked reading stdin, where the process had to be killed
// outright.
//
// The refusal is about DURATION AND REACH, not about the mechanism, and three
// places in the tree hold SIGINT for a bounded span on exactly that reading.
// `add` holds it for its own blocking login, `auto` for its tick — both because
// the thing they are in the middle of is worse to abandon than to unwind — and
// internal/store holds it for the span of one transaction's write, because a
// process killed between the credential file and the document it belongs to
// leaves a live refresh token nothing on the machine can find. None of the
// three outlives what it is protecting, and none of them turns Ctrl-C into a
// no-op: each one stops, and each one exits 130.
//
// Everywhere else — which is almost everywhere, since a store write is a
// handful of file operations — Ctrl-C keeps its default meaning, and the shell
// reports the same 130 without this binary being involved at all.
func Execute() ExitCode {
	ignoreSIGPIPE()
	enableConsoleVT(os.Args[1:])
	return ExecuteWith(NewRootCmd(), os.Stderr)
}

// ExecuteWith runs an already-built root command and reports the exit code,
// writing any error to errOut. Execute is a thin wrapper over it so the
// error-to-exit-code mapping — the contract every command shares — is testable
// without touching os.Stderr or os.Args.
func ExecuteWith(root *cobra.Command, errOut io.Writer) ExitCode {
	err := ExecuteCmd(root)
	if err == nil {
		return ExitOK
	}
	// A closed stdout reader is not a failure: `ccdad list --json | head -1`
	// must exit 0. What that looks like is per-platform -- EPIPE here, two
	// Windows error codes there -- so the predicate is build-tagged rather
	// than an errno spelled inline.
	if isBrokenPipe(err) {
		return ExitOK
	}
	// A silent error carries an exit code without a message: the command has
	// already said what happened in its own words on stderr.
	if errors.Is(err, errSilent) {
		return CodeFor(err)
	}
	fmt.Fprintf(errOut, "ccdad: %s\n", err)
	return CodeFor(err)
}

// shellNames lists the shells the generated completion command can produce for.
func shellNames(completion *cobra.Command) []string {
	out := make([]string, 0, len(completion.Commands()))
	for _, c := range completion.Commands() {
		out = append(out, c.Name())
	}
	return out
}

func isUnknownCommand(err error) bool {
	msg := err.Error()
	return strings.HasPrefix(msg, "unknown command")
}

// runBare is the bare `ccdad` slot: the TTY gate, and the two answers
// behind it.
//
// The gate is stdout AND stdin, not either. Both have to be terminals because
// both are what an interactive session is made of — a dashboard printed into a
// pipe is data nobody asked for, and one printed to a terminal whose stdin is a
// file is the first half of a TUI that can never read a key.
//
// The non-interactive answer is a usage ERROR rather than something useful, and
// that is a decision about timing rather than about this release. This slot is
// promised to a TUI. Anything printed here today that a script could read would
// be a contract by the time the TUI arrives, so the only answer that keeps the
// eventual change a widening rather than a break is the one no script can build
// on. Every release that exits 0 here is a release such a script can be written
// against.
//
// The interactive answer is `ccdad status` itself, through runStatus, and not a
// second renderer that agrees with it today.
func runBare(cmd *cobra.Command, args []string) error {
	// Arguments first, because an argument is a usage error whatever the
	// terminal says and the gate below would otherwise answer a different
	// question about it.
	//
	// Reaching this with anything in args means Cobra's Find() did not treat it
	// as a subcommand: stripFlags ends at `--` and skips both a lone `-` and an
	// empty argument, so `ccdad -- list`, `ccdad -` and `ccdad ""` all arrive
	// here with the word still in hand and nothing downstream that will ever
	// see it. Rendering the dashboard for those reports success for a command
	// that never ran, and — since the dashboard half auto-starts — leaves an
	// engine behind for it.
	if len(args) > 0 {
		return bareUsage(cmd, "ccdad takes a command, not a bare argument (%q)", args[0])
	}
	if !stdoutIsTTY() || !stdinIsTTY() {
		return bareUsage(cmd,
			"bare `ccdad` opens the dashboard, which needs a terminal on stdin and stdout; name a command instead")
	}
	// See root.PersistentPreRun: this is the dashboard half of bare `ccdad`,
	// and it auto-starts for exactly the reason `ccdad status` does — the user
	// is looking at an engine that is not running.
	autoStart(cmd)
	if err := runStatus(cmd, false); err != nil {
		return err
	}
	// On stdout, with the dashboard it belongs to. The --json contract keeps
	// notices on stderr so that a --json document stands alone; this line is
	// reachable only when stdout is a terminal, where there is no document and
	// no consumer to protect.
	fmt.Fprintf(cmd.OutOrStdout(), "\nVerbs: %s  (ccdad <verb> --help)\n", strings.Join(topVerbs, ", "))
	return nil
}

// bareUsage is how the bare slot refuses: the usage text on stderr, and the
// reason as a usage error.
//
// Printed here rather than left to Cobra: the root sets SilenceUsage, and
// Cobra's Help and Usage writers default to OutOrStdout — which is the one
// stream a non-interactive caller may be capturing. The reason comes back as an
// error rather than being printed too, so that ExecuteWith gives it the same
// `ccdad: ` prefix every other error in this binary gets and lands it after the
// usage text, where a reader is looking.
func bareUsage(cmd *cobra.Command, format string, a ...any) error {
	fmt.Fprint(cmd.ErrOrStderr(), cmd.UsageString())
	return UsageError(format, a...)
}

// topVerbs is the one-line footer behind the TTY gate — what a reader of the
// dashboard does next, in the order they would need them: log an account in,
// move to one, take one for a single session, hand the wheel to the engine,
// see them all, find out what is wrong.
var topVerbs = []string{"add", "switch", "run", "auto", "list", "doctor"}

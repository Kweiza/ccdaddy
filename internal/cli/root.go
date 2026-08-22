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
		RunE: func(_ *cobra.Command, _ []string) error {
			// Subcommands are registered below, so Cobra's Find() intercepts an
			// unknown subcommand before this RunE is ever reached. What is left
			// is the bare `ccdad` slot, where a later task adds the TTY-aware
			// dashboard-or-usage-error behaviour.
			return nil
		},
	}
	// Auto-start hangs off PersistentPreRun rather than PersistentPreRunE, and
	// the missing E is the point: §8's auto-start must never fail the command
	// it rode in on, and a hook with no error to return cannot be wired into
	// one later. Which commands it actually acts for is autostart.go's
	// allow-list; this is only where the tree offers it the chance.
	root.PersistentPreRun = func(cmd *cobra.Command, _ []string) { autoStart(cmd) }

	// Cobra reports a mistyped subcommand and a mistyped flag as plain errors.
	// Both are usage errors under this binary's exit contract, so retag them at
	// the single point where Cobra hands them back.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return UsageError("%s", err.Error())
	})

	// Cobra's generated `completion` accepts an unknown shell by printing its
	// own help and returning nil, which exits 0 and puts help text on stdout —
	// a caller redirecting that into a completion file gets help text instead.
	// A shell it cannot generate for is a usage error like any other.
	root.AddCommand(newAddCmd())
	root.AddCommand(newAddTokenCmd())
	root.AddCommand(newWhichCmd())
	root.AddCommand(newListCmd())
	root.AddCommand(newSwitchCmd())
	root.AddCommand(newAutoCmd())
	root.AddCommand(newRemoveCmd())
	root.AddCommand(newDisableCmd())
	root.AddCommand(newEnableCmd())
	root.AddCommand(newAliasCmd())
	root.AddCommand(newMoveCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newConfigCmd())
	root.AddCommand(newExportCmd())
	root.AddCommand(newImportCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newDaemonCmd())
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
// outright. The trap is installed by `add` alone, for the span of its own
// blocking login; everywhere else Ctrl-C keeps its default meaning, which the
// shell already reports as 130.
func Execute() ExitCode {
	ignoreSIGPIPE()
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

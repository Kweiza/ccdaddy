package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/buildinfo"
)

// NewRootCmd builds the ccdad command tree. It is a constructor rather than a
// package-level var so tests can build an isolated tree with its own output
// buffers and argument list.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "ccdad",
		Short:         "Claude Code Daemon: Always Drilling Don't Yap",
		Long:          "ccdad manages multiple Claude Code accounts and switches between them\nbefore a rate limit stops you.",
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
	root.AddCommand(newRemoveCmd())
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
// The root carries a context cancelled by SIGINT. `add` is the first command
// that blocks for minutes — on a browser callback or a pasted code — and
// without this its cancellation arm is unreachable: Ctrl-C would kill the
// process by default disposition, abandoning the loopback listener and any lock
// directory rather than unwinding them. Spec §6.4 asks for a clean exit at 130,
// and 130 is only correct if something actually cleaned up.
func Execute() ExitCode {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	root := NewRootCmd()
	root.SetContext(ctx)
	return ExecuteWith(root, os.Stderr)
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
	// must exit 0.
	if errors.Is(err, syscall.EPIPE) {
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

func isUnknownCommand(err error) bool {
	msg := err.Error()
	return strings.HasPrefix(msg, "unknown command")
}

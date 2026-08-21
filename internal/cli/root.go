package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
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
		RunE: func(_ *cobra.Command, args []string) error {
			// The len(args) > 0 branch is a Task-1-only stopgap. Once subcommands
			// are registered (Task 14 adds `add`), Cobra's Find() intercepts unknown
			// subcommands before this RunE is ever reached, so this branch goes dead.
			// The len(args) == 0 branch is where a later task adds the TTY-aware
			// dashboard-or-usage-error behaviour.
			if len(args) > 0 {
				return UsageError("unknown command %q", args[0])
			}
			return nil
		},
	}
	// Cobra reports a mistyped subcommand and a mistyped flag as plain errors.
	// Both are usage errors under this binary's exit contract, so retag them at
	// the single point where Cobra hands them back.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return UsageError("%s", err.Error())
	})
	return root
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
func Execute() ExitCode {
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
	// must exit 0.
	if errors.Is(err, syscall.EPIPE) {
		return ExitOK
	}
	fmt.Fprintf(errOut, "ccdad: %s\n", err)
	return CodeFor(err)
}

func isUnknownCommand(err error) bool {
	msg := err.Error()
	return strings.HasPrefix(msg, "unknown command")
}

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
		Use:           "ccdad",
		Short:         "Claude Code Daemon: Always Drilling Don't Yap",
		Long:          "ccdad manages multiple Claude Code accounts and switches between them\nbefore a rate limit stops you.",
		Version:       buildinfo.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, args []string) error {
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

// Execute runs the command tree and returns the process exit code. It prints
// the error itself, so main only has to exit.
func Execute() ExitCode {
	root := NewRootCmd()
	err := root.Execute()
	if err == nil {
		return ExitOK
	}
	if isUnknownCommand(err) {
		err = UsageError("%s", err.Error())
	}
	// A closed stdout reader is not a failure: `ccdad list --json | head -1`
	// must exit 0.
	if errors.Is(err, io.ErrClosedPipe) || strings.Contains(err.Error(), "broken pipe") {
		return ExitOK
	}
	fmt.Fprintf(os.Stderr, "ccdad: %s\n", err)
	return CodeFor(err)
}

func isUnknownCommand(err error) bool {
	msg := err.Error()
	return strings.HasPrefix(msg, "unknown command") ||
		strings.HasPrefix(msg, "unknown flag") ||
		strings.HasPrefix(msg, "unknown shorthand flag")
}

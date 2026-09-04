package cli

import (
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/buildinfo"
	"github.com/Kweiza/ccdaddy/internal/mcpsrv"
)

// newMCPCmd is the Model Context Protocol server under the name a client
// starts it by.
//
// It declares NO --json flag, and that silence is a decision rather than an
// omission -- the bare dashboard makes the mirror-image choice. Its
// stdout carries the protocol, not a document, so the flag would promise four
// contract rules it cannot satisfy. TestJSONContractCoversEveryJSONCommand
// fires only for a command that declares the flag, which is why it stays
// silent here.
func newMCPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve ccdad's tools to Claude Code over the Model Context Protocol",
		Long: "Serve ccdad's tools to Claude Code over the Model Context Protocol.\n\n" +
			"This is not a command to run by hand. Claude Code starts it, talks to it over\n" +
			"this process's standard input and output, and stops it when the session ends.\n" +
			"Run 'ccdad mcp install' once to register it, or 'ccdad mcp install --print-config'\n" +
			"to see the entry without writing anything.\n\n" +
			"Standard output carries the protocol and nothing else. Diagnostics go to\n" +
			"standard error, which the client treats as server logs.\n\n" +
			"Every tool runs the ordinary ccdad command it names, through a fresh command\n" +
			"tree, so each call gets its own scoped-session verdict, its own exit code and\n" +
			"the same output the command line would give. The tool that switches the live\n" +
			"login asks the person at the keyboard first.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			srv, err := mcpsrv.New(mcpsrv.Options{
				// freshRootExec, from exec.go, and this file declares none of
				// its own. The three refusals that seam holds -- never omit
				// SetArgs, never omit SetOut/SetErr, never call cli.Execute()
				// in a handler -- are stated once there. cmd is this RunE's
				// own parameter, so the fresh root each tool call builds
				// inherits THIS process's context: a client that goes away
				// mid-call cancels what it started.
				Exec:    freshRootExec(cmd),
				Version: buildinfo.String(),
				// STDERR, always. Stdout is the protocol, and a logger pointed
				// at it kills the session on its first line. It is also not
				// optional: with no logger, a tool registered under an invalid
				// name fails silently, which is why mcpsrv.New refuses one.
				Logger: slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil)),
			})
			if err != nil {
				return err
			}
			return mcpsrv.Serve(cmd.Context(), srv)
		},
	}
	// The two halves of the registration. They are subcommands of the server
	// rather than of the root because what they configure IS this command --
	// and both are refused inside a `ccdad run` session, out of scoped.go's
	// map, under exactly these two paths.
	cmd.AddCommand(newMCPInstallCmd())
	cmd.AddCommand(newMCPUninstallCmd())
	return cmd
}

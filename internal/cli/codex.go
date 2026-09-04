package cli

import "github.com/spf13/cobra"

// newCodexCmd is the parent of everything Codex.
//
// Codex accounts are a different mechanism from Claude ones and the command
// tree says so. Claude Code reads its credential from a file on every request,
// so ccdad swaps that file and a session in flight follows. Codex caches its
// credential in memory for the life of the process and re-reads it on exactly
// one condition — an HTTP 401 — while quota exhaustion is a 429, so the two
// never meet and no file swap can move a running codex. ccdad therefore owns
// the login, the refresh and the quota, and codex reaches the API through a
// local proxy holding no token of its own.
//
// Folding these under `ccdad add` and `ccdad run` instead was the alternative,
// and it was refused for a reason a user can see: the scoped-session verdicts,
// the refusals and the exit codes differ between the two providers, and one
// command carrying both would have to explain which half a message was about.
func newCodexCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "codex",
		Short: "Manage Codex accounts",
		Long: "Codex accounts are managed the way Claude accounts are — added, listed,\n" +
			"switched and rotated on quota — through a different mechanism.\n\n" +
			"codex holds no OAuth token: ccdad owns the login, the refresh and the quota\n" +
			"reading, and codex reaches the API through a loopback proxy this daemon runs.\n" +
			"ccdad never writes codex's own home and never runs 'codex login' or\n" +
			"'codex logout' — both of those revoke the stored grant server-side, with no\n" +
			"undo, and would destroy an account ccdad is managing.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(*cobra.Command, []string) error {
			// Cobra's own answer is to print help and exit 0. A caller that
			// meant to type a verb gets a usage error, as everywhere else.
			return UsageError("codex needs a subcommand: add, exec, shim")
		},
	}
	cmd.AddCommand(newCodexAddCmd())
	cmd.AddCommand(newCodexExecCmd())
	cmd.AddCommand(newCodexShimCmd())
	return cmd
}

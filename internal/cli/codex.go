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
// What is left here after the login moved is what has no Claude counterpart.
// The login itself folded into `ccdad add codex`, where it sits beside `ccdad
// add claude`: adding an account is one thing a user does, and having to know
// which half of ccdad's history a provider arrived in to spell it was the seam
// this group used to be. `exec` and `shim` did not fold, and not because the
// same argument stops applying — there is no `ccdad claude exec` for them to be
// a peer of. `exec` launches codex through the proxy, `shim` is what makes
// typing `codex` reach ccdad at all, and `ccdad codex exec` is written
// byte-for-byte into every shim already installed, so renaming it would orphan
// the shims on disk.
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
		Args:          codexArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(*cobra.Command, []string) error {
			// Cobra's own answer is to print help and exit 0. A caller that
			// meant to type a verb gets a usage error, as everywhere else.
			return UsageError("codex needs a subcommand: exec, shim")
		},
	}
	cmd.AddCommand(newCodexExecCmd())
	cmd.AddCommand(newCodexShimCmd())
	return cmd
}

// codexArgs is the group's argument check and the tombstone for `ccdad codex
// add`, which is one release old and named on no disk.
//
// A tombstone rather than a hidden alias, and the difference is what a script
// learns. An alias exits 0, so a pipeline still spelling it the old way would
// go on working and its author would never find out the name had moved — until
// the alias was eventually removed, by which time nothing would connect the
// failure to this rename. A usage error naming the new spelling costs one run
// and is read by the person who can fix it.
func codexArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 && args[0] == "add" {
		return UsageError("'ccdad codex add' is now 'ccdad add codex', beside 'ccdad add claude'")
	}
	return usageArgs(cobra.NoArgs)(cmd, args)
}

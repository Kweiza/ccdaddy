package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/switcher"
)

func newWhichCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:           "which",
		Short:         "Show which managed account Claude Code is logged in as",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			live, err := cclink.Load()
			if err != nil {
				return err
			}
			// A config that cannot be read is not a reason to refuse the whole
			// question: it costs the two api-key inputs, and the environment
			// axes — which are the ones that OVERRIDE a login — still answer.
			cfg, cfgErr := cclink.LoadGlobalConfig()
			if cfgErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"note: could not read Claude Code's config (%v); a stored API key cannot be seen from here\n", cfgErr)
				cfg = nil
			}
			env := claudeAPIKeyEnvironment(cfg)
			res := switcher.AttributeLogin(live, s.Accounts(), s.Credentials, env)

			if asJSON {
				payload := map[string]any{"schemaVersion": 1, "attributed": res.OK, "via": res.Via}
				if res.OK {
					payload["account"] = accountJSON(res.Account)
				}
				if env.EnvKeyNeedsApproval() {
					payload["envKeyNeedsApproval"] = true
				}
				if unknown := cclink.UnknownKeys(live); len(unknown) > 0 {
					payload["unknownKeys"] = unknown
				}
				if err := writeJSON(cmd, payload); err != nil {
					return err
				}
				if !res.OK {
					// The exit code is the same with or without --json: the flag
					// changes the representation, never the answer.
					return WithCode(errSilent, ExitProbeNegative)
				}
				return nil
			}

			noteEnvKeyApproval(cmd, env)
			if !res.OK {
				fmt.Fprintf(cmd.ErrOrStderr(), "The current credential (%s) is not one ccdad manages.\n", res.Via)
				// A negative probe answer, not a failure: exit 5 so a script can
				// tell it from a real error.
				return WithCode(errSilent, ExitProbeNegative)
			}
			fmt.Fprintln(cmd.OutOrStdout(), res.Account.Label())
			fmt.Fprintf(cmd.ErrOrStderr(), "via %s\n", res.Via)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a machine-readable object on stdout")
	return cmd
}

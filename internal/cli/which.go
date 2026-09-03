package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/switcher"
	"github.com/Kweiza/ccdaddy/internal/view"
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
			// WithSource, because `via` is half this command's answer: on macOS
			// the keychain item is read before the file, and naming the file
			// there was wrong on exactly the machines where the difference is
			// the reason someone ran `which`.
			live, liveSource, err := cclink.LoadWithSource()
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
			res := switcher.AttributeLogin(live, s.Accounts(), s.Credentials, env,
				claudeOAuthEnvironment(), liveSource)

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
			// Resolved BEFORE the !res.OK branch below, and not after it: a
			// machine can have no Claude login attributed at all while codex is
			// still being served, and that is not a reason for `which` to go
			// silent about the half of the question it CAN answer. status.go's
			// dashboard shares loadSnapshot and answers this exact state with a
			// Codex clause; two commands describing one machine two ways is the
			// failure this task exists to remove.
			codexLabel := ""
			if serving, ok := codexServingAccount(s.Accounts()); ok {
				codexLabel = serving.Label()
			}
			if !res.OK {
				fmt.Fprintf(cmd.ErrOrStderr(), "%s\n", unattributedNotice(res))
				if codexLabel != "" {
					// The exit code stays ExitProbeNegative: it reports whether
					// Claude Code's OWN credential was attributed, and codex
					// being served does not change that answer -- it only means
					// there is a second clause worth printing alongside it.
					fmt.Fprintln(cmd.OutOrStdout(), view.ActiveLine(noActiveAccountLabel, codexLabel))
				}
				// A negative probe answer, not a failure: exit 5 so a script can
				// tell it from a real error.
				return WithCode(errSilent, ExitProbeNegative)
			}
			// The bare label is kept when there is no codex account, and that
			// is a contract rather than a nicety: `ccdad which` is what a shell
			// prompt and a CI step read, and every one of them predates the
			// second provider. The two-clause form appears only on a machine
			// where the second clause is a fact.
			fmt.Fprintln(cmd.OutOrStdout(), view.ActiveLine(res.Account.Label(), codexLabel))
			fmt.Fprintf(cmd.ErrOrStderr(), "via %s\n", res.Via)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a machine-readable object on stdout")
	return cmd
}

// unattributedNotice says what ccdad could not do, rather than asserting whose
// the credential is.
//
// On the login-store axis it must not claim the account is unmanaged: the
// refresh token is rotated by Claude Code on every refresh and attribution
// matches on that token, so the commonest reason this fires is one of the
// user's own accounts, moments after a refresh. That is the same fact the
// engine encodes as "cannot attribute is not nobody is live".
//
// The environment axis keeps the blunt sentence, because there it is true: a
// token supplied through the environment is not rotated behind ccdad's back,
// so failing to match it really does mean ccdad does not hold it.
func unattributedNotice(res switcher.Attribution) string {
	if !res.FromLoginStore {
		return fmt.Sprintf("The current credential (%s) is not one ccdad manages.", res.Via)
	}
	return fmt.Sprintf(
		"ccdad could not attribute the current credential (%s) to a managed account. "+
			"That is not proof it is unmanaged: Claude Code rotates a login's refresh token on "+
			"every refresh, and ccdad matches on that token, so one of your own accounts stops "+
			"being recognisable the moment it refreshes. `ccdad add` re-registers an account whose "+
			"token has moved on.", res.Via)
}

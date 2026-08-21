package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/store"
)

func newRemoveCmd() *cobra.Command {
	var assumeYes bool

	cmd := &cobra.Command{
		Use:           "remove <ACCOUNT>",
		Short:         "Stop managing an account and delete its stored credentials",
		Args:          exactlyOneAccount("remove"),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			accounts := s.Accounts()
			target, err := store.Resolve(accounts, args[0])
			if err != nil {
				return UsageError("%s", err.Error())
			}

			// Attribute before removing: afterwards the stored credentials are
			// gone and the question can no longer be answered.
			live, _ := cclink.Load()
			current, hasLive := attributeLogin(live, accounts, s.Credentials)
			wasLive := hasLive && current.UUID == target.UUID

			// A destructive command with no terminal to confirm at must be told
			// explicitly, or a script deletes credentials nobody meant to lose.
			if !assumeYes {
				if !stdinIsTTY() {
					return UsageError("removing %s deletes its stored credentials; pass --yes to confirm", target.Label())
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "Remove %s and delete its stored credentials? [y/N] ", target.Label())
				var answer string
				_, _ = fmt.Fscanln(cmd.InOrStdin(), &answer)
				if answer != "y" && answer != "Y" {
					fmt.Fprintln(cmd.ErrOrStderr(), "Left alone.")
					return WithCode(errSilent, ExitNothingToDo)
				}
			}

			if err := s.Remove(target.UUID); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Removed %s.\n", target.Label())
			if wasLive {
				// The credentials are gone from ccdad but still installed in
				// Claude Code, so the user stays logged in as an account ccdad
				// can no longer switch back to. That must not be silent.
				fmt.Fprintln(cmd.ErrOrStderr(),
					"Note: that account is still the live Claude Code login, and ccdad no longer has its credentials. "+
						"Run 'ccdad switch' to move to a managed account.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "do not prompt for confirmation")
	return cmd
}

package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/store"
)

func newListCmd() *cobra.Command {
	var (
		asJSON  bool
		showAll bool
	)

	cmd := &cobra.Command{
		Use:           "list",
		Short:         "List managed accounts",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			accounts := s.Accounts()
			// list tolerates an unreadable live file: it is decoration here, not
			// the answer, and `ccdad list` is what a user reaches for when
			// something is already wrong.
			live, _ := cclink.Load()
			active, hasActive := attributeLive(live, accounts, s.Credentials)

			visible := make([]store.Account, 0, len(accounts))
			for _, a := range accounts {
				if a.Disabled && !showAll {
					continue
				}
				visible = append(visible, a)
			}

			if asJSON {
				rows := make([]map[string]any, 0, len(visible))
				for _, a := range visible {
					row := accountJSON(a)
					row["active"] = hasActive && a.UUID == active.UUID
					rows = append(rows, row)
				}
				payload := map[string]any{"schemaVersion": 1, "accounts": rows}
				if hasActive {
					payload["activeUuid"] = active.UUID
				}
				if unknown := cclink.UnknownKeys(live); len(unknown) > 0 {
					payload["unknownKeys"] = unknown
				}
				return writeJSON(cmd, payload)
			}

			// An empty listing is exit 0. "Nothing to show" is a fact, not a
			// failure, and a scripted `ccdad list` should not have to special-case
			// it. The two reasons a listing can be empty are different advice,
			// so they are distinguished rather than collapsed.
			if len(visible) == 0 {
				if len(accounts) == 0 {
					fmt.Fprintln(cmd.ErrOrStderr(), "No accounts yet. Run 'ccdad add' to log one in.")
				} else {
					fmt.Fprintln(cmd.ErrOrStderr(), "Every account is disabled. Run 'ccdad list --all' to see them.")
				}
				return nil
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "  IDX\tACCOUNT\tTYPE\tTIER")
			for _, a := range visible {
				marker := " "
				if hasActive && a.UUID == active.UUID {
					marker = "*"
				}
				label := a.Email
				if a.Alias != "" {
					label = fmt.Sprintf("%s (%s)", a.Email, a.Alias)
				}
				tier := a.Tier
				if tier == "" {
					tier = "-"
				}
				suffix := ""
				if a.Disabled {
					suffix = "  (disabled)"
				}
				fmt.Fprintf(w, "%s %d\t%s\t%s\t%s%s\n", marker, a.Idx, label, a.Kind, tier, suffix)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a machine-readable object on stdout")
	cmd.Flags().BoolVar(&showAll, "all", false, "include disabled accounts")
	return cmd
}

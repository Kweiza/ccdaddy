package cli

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// newEngine builds the poller `--refresh` borrows. It is a seam for the same
// reason profileBaseURL is: a test that reached the real usage endpoint would
// depend on the machine being online and would send a made-up token to a live
// service. isolate() points it at one that fails the test if dialled.
var newEngine = daemon.NewEngine

func newListCmd() *cobra.Command {
	var (
		asJSON  bool
		showAll bool
		refresh bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List managed accounts",
		Long: "list reads what is already on disk — the same usage cache 'ccdad status'\n" +
			"reads, so the two can never disagree.\n\n" +
			"--refresh is the one exception, and it is a button on a rate-limited\n" +
			"endpoint: the allowance is roughly 28-30 requests per identity per rolling\n" +
			"hour on a sliding window, so a burst saturates an account for a full hour\n" +
			"and waiting does not give the capacity back early. A reading under three\n" +
			"minutes old is therefore served as it stands and no request is made, and a\n" +
			"429 holds the next one off for longer still. It says on stderr when it did\n" +
			"nothing and why; --json carries usage.fetchedAt, which is the machine's\n" +
			"answer to how fresh a number is.\n\n" +
			"It refreshes the rows it is about to print: --all puts the disabled\n" +
			"accounts back on the listing and therefore back in scope.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			now := timeNow()

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

			// The sole exception to "list never fetches", and it happens BEFORE
			// the cache is read so the listing renders what it just took rather
			// than what it found on the way in.
			if refresh {
				refreshUsage(cmd, s, visible, active, hasActive, now)
			}

			// The quota half, from the cache `status` reads and no other
			// source. Without --refresh, list still never fetches.
			cache, err := usage.LoadCache()
			if err != nil {
				return err
			}
			if cerr := cache.LoadError(); cerr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "The usage cache could not be read: %v\n", cerr)
			}
			quota := quotaRows(visible, cache, active, hasActive, now, rowThresholds(cmd, now))

			if asJSON {
				rows := make([]map[string]any, 0, len(visible))
				for _, r := range quota {
					row := accountJSON(r.Account)
					row["active"] = r.Active
					// The same object `ccdad status --json` publishes, built by
					// the same function from the same cache. That they can never
					// disagree is a property of there being one of these, not of
					// two of them being written carefully.
					if u := usageJSON(r, now); u != nil {
						row["usage"] = u
					}
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
			fmt.Fprintln(w, "  IDX\tACCOUNT\tTYPE\tTIER\tLEFT\tRESETS IN")
			for _, r := range quota {
				a := r.Account
				marker := " "
				if r.Active {
					marker = "*"
				}
				// Both the address and the handle, which is NOT Account.Label():
				// that one returns the alias alone, and a listing is where a
				// user learns which alias belongs to which address.
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
				fmt.Fprintf(w, "%s %d\t%s\t%s\t%s\t%s\t%s%s\n", marker, a.Idx, label, a.Kind,
					tier, r.leftLabel(), r.resetsLabel(now), suffix)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a machine-readable object on stdout")
	cmd.Flags().BoolVar(&showAll, "all", false, "include disabled accounts")
	cmd.Flags().BoolVar(&refresh, "refresh", false,
		"take a fresh usage reading before listing, where the poll policy allows one")
	return cmd
}

// refreshUsage is `--refresh`: the one exception to "list never fetches".
//
// It refreshes the rows this listing is about to PRINT and no others. The
// endpoint's allowance belongs to an identity and only recovers as old requests
// age out, so spending it on an account held out of the listing buys output
// nobody asked to see; `--all` puts the disabled accounts back on the listing
// and therefore back in scope. The daemon, which needs a reading to RANK on
// rather than to show, keeps polling everything on its own schedule.
//
// Every failure here is a notice and none of them is the command's exit code.
// The listing still rendered, which is what `ccdad list` was asked for, and the
// poll policy makes "no fetch happened" an ordinary outcome rather than a
// fault — a script that went red because one of five accounts was inside its
// serveTTL would go red most of the time. A caller that needs to know how fresh
// a number is reads `usage.fetchedAt` out of --json, which is the
// machine-readable answer to exactly that question.
func refreshUsage(cmd *cobra.Command, s *store.Store, want []store.Account,
	active store.Account, hasActive bool, now time.Time) {

	errw := cmd.ErrOrStderr()
	cfg, err := config.Load()
	if err != nil {
		// A mistyped threshold must not stop the work, exactly as `auto` treats
		// one. Threshold only decides this account's next cadence here.
		fmt.Fprintf(errw, "note: %v; refreshing on the built-in defaults\n", err)
		cfg = config.Defaults()
	}

	activeUUID := ""
	if hasActive {
		activeUUID = active.UUID
	}

	var fetched, cached int
	for _, r := range newEngine().Refresh(cmd.Context(), s, want, cfg, activeUUID) {
		switch r.State {
		case daemon.RefreshFetched:
			fetched++
		case daemon.RefreshCached:
			cached++
		case daemon.RefreshHeld:
			fmt.Fprintf(errw, "%s: the usage endpoint rate-limited ccdad; a refresh is allowed again in %s.\n",
				r.Account.Label(), humanDuration(r.At.Sub(now)))
		case daemon.RefreshFailed:
			fmt.Fprintf(errw, "%s could not be refreshed: %v\n", r.Account.Label(), r.Err)
		case daemon.RefreshUnpollable:
			// Silent on purpose. There is no OAuth grant behind an
			// `add-token` account and there never will be, so this is the
			// ordinary case rather than news — and the row already says so
			// twice, with an api-key TYPE and a '?' where a number would be.
		}
	}
	// Said only when nothing was fetched AND nothing went wrong, so the answer
	// to "why are these numbers the same as a minute ago" is never silence.
	if fetched == 0 && cached > 0 {
		fmt.Fprintf(errw, "Nothing needed refreshing: every reading is under %s old.\n",
			humanDuration(usage.ServeTTL))
	}
}

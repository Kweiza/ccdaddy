package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/usage"
	"github.com/Kweiza/ccdaddy/internal/view"
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
			// Read once, here, because two things hang on it: which thresholds
			// the rows are measured against, and whether the table below has to
			// say where they came from.
			cfg := rowConfig(cmd)
			quota := view.Rows(visible, cache, active, hasActive, now, rowThresholds(cmd, cfg, s, now))

			// The same measurement `ccdad status` renders and publishes, taken
			// by the same function from the same two documents: two commands
			// reading one store must not describe one measurement two ways.
			//
			// It is measured over the STORE's accounts and not over the rows
			// about to be printed. --all is a filter on a listing, and a filter
			// on a listing may not move a fleet's burn rate; measuring `visible`
			// would also make this key disagree with the identical key on
			// `ccdad status --json`, which sees every account.
			//
			// A series that cannot be read costs the rates and nothing else, so
			// it is a notice here exactly as an unreadable cache is: `ccdad
			// list` is what a user reaches for when something is already wrong.
			f, historyNotice := fleetForecast(accounts, cache, now)
			if historyNotice != "" {
				fmt.Fprint(cmd.ErrOrStderr(), historyNotice)
			}

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
				// Conditional, like activeUuid above and like `mode` on the
				// status payload: absent means nothing was measured, and an
				// object of zeros would read as a fleet burning nothing. It is
				// forecastJSON's object rather than a second spelling of it,
				// so `ccdad runway --json`, `ccdad status --json` and this
				// carry one document under one key.
				if f.Basis.Known {
					payload["forecast"] = forecastJSON(f)
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

			// Which window LEFT reports is chosen by the thresholds hover
			// DERIVED, and saying nothing about that is how a mode doing exactly
			// what it promised reads as a defect: a user who set threshold = 80
			// and finds a row reported against a window held to 93 has nothing to
			// tell the two apart, and the listing is where an account gets chosen.
			//
			// What hover moves here is that CHOICE, not the figure. LEFT is
			// Headroom.Pct, which is 100 minus the reported window's utilization
			// and has no threshold in it. The note used to say the number was
			// measured against the derived thresholds, which would have a reader
			// take LEFT = 11% as eleven points of margin on a row the same binary
			// prints a threshold of 99 for.
			//
			// It sits here rather than beside the config read, past both empty
			// returns, because it is a note about a COLUMN: printed above "No
			// accounts yet" it describes a table that was never drawn. Human-only
			// for the same reason -- --json already carries the real
			// windowThreshold on every row, which is a better answer than a
			// sentence about a column that document does not have.
			if cfg.Hover {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"note: hover is on; LEFT is how much of the window is left, and which window a row reports is chosen by the thresholds hover derived per account rather than by a value in config.toml. 'ccdad hover status' prints them.")
			}
			// Beside the hover note and for a different reason: hover's note is
			// about what a COLUMN means, and this one is about whether anything
			// will act on it. Every figure in the table below is current either
			// way, which is exactly why a reader cannot tell from the table that
			// nothing is going to move -- so the note has to.
			if cfg.Manual {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"note: manual mode is on; ccdad keeps these readings current and will not switch accounts. 'ccdad switch <account>' still works, and 'ccdad manual off' hands the wheel back.")
			}

			out, pal := renderTarget(cmd)
			cells := make([][]string, 0, len(quota))
			for _, r := range quota {
				a := r.Account
				// An account can carry both flags at once, and they say
				// opposite things: primary is "ranked beside the
				// subscriptions", disabled is "left out of rotation
				// entirely". Printing only the first one found would hide
				// whichever one the reader came looking for, half the time.
				var flags []string
				if a.Primary {
					flags = append(flags, "primary")
				}
				if a.Disabled {
					flags = append(flags, "disabled")
				}
				// Spelled as a machine and not as a policy, because that is
				// what it is: nothing is wrong with the account, it is just
				// not this machine's to drive. A reader who sees an account
				// that never gets chosen and no reason beside it goes looking
				// for the reason in the wrong place.
				if a.Elsewhere {
					flags = append(flags, "another machine")
				}
				suffix := ""
				if len(flags) > 0 {
					suffix = "  (" + strings.Join(flags, ", ") + ")"
				}
				// The flags ride on the RESETS IN cell rather than in a column
				// of their own, which is what the %s%s in the format string
				// this replaced was doing. A column would align them, and a
				// suffix that belongs to one account reads better beside that
				// account's own reset than at a fixed offset far to its right.
				cells = append(cells, []string{
					fmt.Sprintf("%s %d", r.Marker(), a.Idx), r.ListLabel(), a.Kind.String(),
					r.TierLabel(), r.LeftLabel(), r.ResetsLabel(now) + suffix,
				})
			}
			if err := columns(out, []string{"  IDX", "ACCOUNT", "TYPE", "TIER", "LEFT", "RESETS IN"},
				cells, quotaCellStyle(pal, quota, 4, view.Row.LeftLabel)); err != nil {
				return err
			}
			// After the table, and after the early return above it, which is
			// what keeps a listing with no rows from carrying a summary of
			// them. Position is what decides that, and now it decides it
			// plainly: columns() writes when it is called, so a line written
			// after it comes out after it, where list_test.go's rowFor has
			// already stopped scanning for account rows. The tabwriter this
			// replaced buffered every row until Flush, which meant a line
			// written at ANY earlier point still landed above the table -- the
			// ordering was the same and the reason was not, and a reader who
			// moves this line is now moving the bytes rather than only the
			// source.
			//
			// The empty string is the gate. view.RunwayLine returns one when
			// there is no measurement, and that is how this renderer, `ccdad
			// status` and the dashboard all decline the line without each
			// carrying its own idea of when there is nothing to say.
			//
			// time.Local, because a person is reading it. internal/forecast
			// touches no environment, so the caller nearest the reader chooses
			// the zone, and view.Timestamp always prints which one it was.
			//
			// The width is the terminal's, and zero for anything that is not
			// one; see outWidth. `ccdad status` folds the same line the same
			// way, and both leave it alone when nobody is watching.
			//
			// Measured on cmd.OutOrStdout() and NOT on out, which is the same
			// destination wearing a palette. outWidth answers by asserting
			// *os.File, out is colorWriter's wrapper around one, and a wrapper
			// fails that assertion -- so a width read off it is 0 on every
			// terminal there is and the fold silently stops happening, on a
			// branch where nothing else about the line changed. Nothing catches
			// that on its own: git merges the two edits without a word, every
			// test that stubs the outWidth seam stays green, and the only
			// reader who can see it is a person at an 80-column window.
			// TestTheFoldMeasuresTheFileAndNotTheWriterItPaintsThrough is what
			// turns that merge red, and this site is one it reaches -- it runs
			// `ccdad list` as well as `ccdad status`, because the collision
			// arrived at each of them separately.
			//
			// The rejected alternative was an Unwrap() io.Writer on
			// colorWriter's type with outWidth following the chain. That is a
			// general mechanism -- one every future decorator would have to
			// remember to implement, and one that quietly re-enables the fold
			// for a wrapper that genuinely does change the usable width -- put
			// behind two call sites. A palette does not change how wide the
			// terminal is, so the width is taken from the thing that has one.
			if line := view.RunwayLine(f, now, time.Local); line != "" {
				fmt.Fprintf(out, "\n%s\n", view.RunwayWrap("Runway:  ", line, outWidth(cmd.OutOrStdout())))
			}
			return nil
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

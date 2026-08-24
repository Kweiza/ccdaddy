package cli

import (
	"fmt"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/switcher"
)

// `ccdad hover` is the fully automatic mode and the window onto it.
//
// The three verbs share one command rather than being three subcommands so that
// the tree's totality gates cost one entry each: one scoped-session verdict, one
// row in the --json contract. The cost of that choice is that --json cannot be
// declared per verb, so `on` and `off` refuse it explicitly -- a flag quietly
// ignored is a caller who piped the output into jq and got an empty stream.
//
// The exit codes are the contract's own:
//
//	hover on|off   0 written - 3 already in that state - 2 a verb or flag ccdad does not have
//	hover status   0 hover is on - 5 hover is off, with the table printed either way
//
// 5 rather than 0 for a report on a mode that is off is what the exit contract
// reserves for a negative answer to a probe. It makes `ccdad hover status
// >/dev/null || ccdad hover on` correct, and the table is printed on both sides
// of it because the numbers hover WOULD choose are exactly what somebody
// deciding whether to turn it on needs to see.
func newHoverCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "hover on|off|status",
		Short: "Let ccdad choose every threshold for itself",
		Long: "hover derives a threshold for every account and every window from that\n" +
			"window's own reset time and the number of accounts that can take the work,\n" +
			"instead of reading one out of config.toml. A window 43% of the way through\n" +
			"its week, with four accounts sharing the load, is held to 68%; the last\n" +
			"account left is held to 99%, because there is nobody to hand the work to.\n\n" +
			"It stops reading threshold, window_threshold, credit.threshold, strategy,\n" +
			"probe_unknown, preempt_lead, hysteresis_pct, headroom_ratio, cooldown and\n" +
			"recovery_hysteresis. 'ccdad config list' marks each of them while hover is on,\n" +
			"so a number that stopped mattering says so rather than quietly not applying.\n\n" +
			"It does NOT stop reading credit.max_auto_spend, or an account's primary and\n" +
			"disabled flags. Fully automatic must not become fully automatic spending: the\n" +
			"ceiling is one of the two independent opt-ins unattended overage requires, and\n" +
			"a mode cannot supply an opt-in on your behalf. primary and disabled are facts\n" +
			"about an account rather than knobs.\n\n" +
			"'ccdad hover status' prints every number hover has chosen right now. Nothing\n" +
			"is hidden, which is what makes handing the wheel over reasonable.",
		Args:          usageArgs(cobra.ExactArgs(1)),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "status":
				return runHoverStatus(cmd, asJSON)
			case "on", "off":
				if asJSON {
					return UsageError("--json belongs to `ccdad hover status`; on and off write %s",
						config.FileName)
				}
				return setHover(cmd, args[0] == "on")
			}
			return UsageError("hover takes one of on, off, status, not %q", args[0])
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a machine-readable object on stdout")
	return cmd
}

// onOff renders the mode the way the command's own words spell it, so a message
// and the argument that produced it cannot drift apart.
func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// setHover writes the one key, having first read the current value under the
// same lock the write takes. Reading it outside would leave a window in which
// two `ccdad hover on` calls both decide they have work to do and both write,
// and the daemon detects change on the BYTES of this file -- so a second
// identical write is an edit as far as it is concerned.
func setHover(cmd *cobra.Command, on bool) error {
	changed := false
	err := config.WithDocument(func(d *config.Document) error {
		cfg, cerr := d.Config()
		if cerr != nil {
			// A document that decodes but does not validate: the engine runs on
			// its built-in defaults for exactly this file, so that is what the
			// current value has to be read from.
			fmt.Fprintf(cmd.ErrOrStderr(),
				"warning: %s still cannot be used and the engine will run on its built-in defaults until it can: %v\n",
				config.FileName, cerr)
			cfg = config.Defaults()
		}
		if cfg.Hover == on {
			// Nothing to write. The file is left byte-for-byte as it was.
			return nil
		}
		changed = true
		return d.Set(config.KeyHover, strconv.FormatBool(on))
	})
	if err != nil {
		return err
	}
	if !changed {
		fmt.Fprintf(cmd.ErrOrStderr(), "hover is already %s.\n", onOff(on))
		return WithCode(errSilent, ExitNothingToDo)
	}

	errw := cmd.ErrOrStderr()
	fmt.Fprintf(errw, "%s = %s\n", config.KeyHover, strconv.FormatBool(on))
	if !on {
		fmt.Fprintln(errw, "The values in "+config.FileName+" are in force again.")
		return nil
	}
	// What it means, in the order a user needs it: what stopped applying, and
	// then the one thing that did not. The money sentence is last because it is
	// the one somebody who typed this by mistake has to read.
	fmt.Fprintln(errw,
		"ccdad now derives every threshold from each window's reset time and the number of\n"+
			"usable accounts. It stops reading threshold, window_threshold, credit.threshold,\n"+
			"strategy, probe_unknown, preempt_lead, hysteresis_pct, headroom_ratio, cooldown\n"+
			"and recovery_hysteresis.\n"+
			"It still reads credit.max_auto_spend, so unattended spending still needs its own\n"+
			"opt-in, and it still honours each account's primary and disabled flags.\n"+
			"Run 'ccdad hover status' to see the numbers being chosen for you.")
	return nil
}

// runHoverStatus prints every threshold hover has computed, the utilization each
// is compared against, and the slack between them.
//
// It goes through switcher.Evaluate rather than deriving a second table of its
// own, and that is the whole reason the numbers here can be trusted: they are
// the engine's, produced by the engine's code over the engine's pool with
// quarantine already applied. Two derivations would agree until the day one of
// them was changed.
//
// Evaluate never writes and never fetches. It reads the cache `ccdad status`
// reads, for the same reason: the usage endpoint allows roughly 28-30 requests
// per identity per rolling hour on a sliding window, so a command a user can run
// in a loop must not be a way to spend it.
func runHoverStatus(cmd *cobra.Command, asJSON bool) error {
	now := timeNow()

	cfg, cerr := config.Load()
	if cerr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"note: %v; the numbers below are the ones the engine uses until the file is fixed.\n", cerr)
		cfg = config.Defaults()
	}
	on := cfg.Hover

	s, err := store.Open()
	if err != nil {
		return err
	}
	// Hover is forced ON for this pass whatever the file says, by handing
	// Evaluate a config with the bit set rather than by asking it for a mode
	// override of its own. The numbers hover would choose are exactly what
	// somebody deciding whether to turn it on needs, and a report that went
	// blank on the machines where the question is live would be useless on all
	// of them.
	forced := cfg
	forced.Hover = true
	ev, err := switcher.Evaluate(s, switcher.EvalOptions{
		Now:    now,
		Config: func() (config.Config, error) { return forced, nil },
	})
	if err != nil {
		return err
	}

	var plan strategy.HoverPlan
	if ev.Plan.Hover != nil {
		plan = *ev.Plan.Hover
	}
	if ev.NoReadings {
		fmt.Fprintln(cmd.ErrOrStderr(),
			"No usage readings yet, so there is no window to pace. Start the engine with 'ccdad daemon start'.")
	}

	byUUID := map[string]store.Account{}
	for _, a := range s.Accounts() {
		byUUID[a.UUID] = a
	}
	activeUUID := ""
	if ev.LiveKnown {
		activeUUID = ev.Live.UUID
	}

	if asJSON {
		if err := writeJSON(cmd, hoverPayload(on, plan, byUUID, activeUUID)); err != nil {
			return err
		}
	} else if err := renderHoverStatus(cmd, on, plan, byUUID, activeUUID); err != nil {
		return err
	}

	if !on {
		fmt.Fprintln(cmd.ErrOrStderr(),
			"hover is off, so these are the thresholds it would choose rather than the ones in force. "+
				"'ccdad hover on' puts them in force.")
		return WithCode(errSilent, ExitProbeNegative)
	}
	return nil
}

func hoverPayload(on bool, plan strategy.HoverPlan,
	byUUID map[string]store.Account, activeUUID string) map[string]any {

	rows := make([]map[string]any, 0, len(plan.Windows))
	for _, w := range plan.Windows {
		row := map[string]any{
			"account":     accountJSON(byUUID[w.UUID]),
			"window":      string(w.Window),
			"utilization": w.Utilization,
			"threshold":   w.Threshold,
			"slack":       w.Slack,
		}
		// Absent rather than zero. A window with no reset has no share elapsed,
		// and a 0 there reads as "this window has just reset", which is the most
		// generous answer there is.
		if w.HasExpected {
			row["expectedPct"] = w.ExpectedPct
		}
		if w.ProbeWanted {
			row["probeWanted"] = true
		}
		if w.Credit {
			row["credit"] = true
		}
		if activeUUID != "" && w.UUID == activeUUID {
			row["active"] = true
		}
		rows = append(rows, row)
	}
	return map[string]any{
		"schemaVersion":  1,
		"hover":          on,
		"usableAccounts": plan.Usable,
		"windows":        rows,
	}
}

// renderHoverStatus is the human table. Every column is an INPUT to the formula
// except the last two, which are its output and the comparison it feeds -- so a
// reader can check the arithmetic rather than being asked to accept it.
func renderHoverStatus(cmd *cobra.Command, on bool, plan strategy.HoverPlan,
	byUUID map[string]store.Account, activeUUID string) error {

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Hover:   %s\n", onOff(on))
	fmt.Fprintf(out, "Pool:    %d usable accounts, so each threshold is the share of its own window that\n", plan.Usable)
	fmt.Fprintf(out, "         has elapsed, plus 100/%d points, capped at %.0f.\n\n",
		max(plan.Usable, 1), strategy.HoverCap)

	if len(plan.Windows) == 0 {
		return nil
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  IDX\tACCOUNT\tWINDOW\tELAPSED\tUTIL\tTHRESHOLD\tSLACK")
	for _, row := range plan.Windows {
		a := byUUID[row.UUID]
		marker := " "
		if activeUUID != "" && row.UUID == activeUUID {
			marker = "*"
		}
		// Never "0%" for a window with no reset: unknown is not zero, and a zero
		// here would look like a window that has only just rolled over.
		elapsed := "-"
		if row.HasExpected {
			elapsed = fmt.Sprintf("%.0f%%", row.ExpectedPct)
		}
		// The note rides on the SLACK cell rather than in a column of its own.
		// tabwriter pads every cell that is followed by a tab, so a note column
		// would pad SLACK out to the width of the longest note and leave every
		// row without one ending in trailing spaces.
		note := ""
		switch {
		case row.ProbeWanted:
			note = "  (no reset yet; a probe is queued)"
		case row.Credit:
			note = "  (primary, metered in credits)"
		}
		fmt.Fprintf(w, "%s %d\t%s\t%s\t%s\t%.0f%%\t%.0f%%\t%+.0f%s\n",
			marker, a.Idx, a.Label(), row.Window, elapsed,
			row.Utilization, row.Threshold, row.Slack, note)
	}
	return w.Flush()
}

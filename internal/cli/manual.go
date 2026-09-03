package cli

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/config"
)

// `ccdad manual` holds the engine to WATCHING.
//
// It is the mode nothing else in the tree could express. Disabling every account
// reaches the same silence -- the ranking pool empties and the plan is blocked
// forever -- and it costs the user four commands, the probes that fill a window
// with no reset time, every figure `ccdad runway` is built from, the plain
// `ccdad list` table, and a permanent exit 4 on `ccdad auto --once`. It also
// re-arms itself: an account added later defaults to enabled, so the fleet
// silently starts rotating again. This key costs none of that and cannot be
// undone by an addition.
//
// It is deliberately NOT a lock. `ccdad switch <account>` is untouched -- the
// same line `ccdad disable` takes about itself, for the same reason: this is a
// policy for the auto engine, and the person at the keyboard is not the auto
// engine.
//
// The verbs and the exit codes mirror `ccdad hover` exactly, because the two are
// the same shape of thing -- a global engine mode with a window onto it -- and a
// user who has learned one grammar should not have to learn a second:
//
//	manual on|off   0 written - 3 already in that state - 2 a verb ccdad does not have
//	manual status   0 manual mode is on - 5 it is off
//
// 5 rather than 0 for a report on a mode that is off is what the exit contract
// reserves for a negative answer to a probe, and it makes `ccdad manual status
// >/dev/null || ccdad manual on` correct.
func newManualCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "manual on|off|status",
		Short: "Watch quota without ever switching accounts",
		Long: "manual mode leaves the engine running and stops it moving the live login.\n" +
			"It keeps polling on the same cadence, keeps refreshing the OAuth grants,\n" +
			"keeps writing the usage cache and the history, keeps deriving hover's\n" +
			"thresholds and keeps answering 'ccdad status', 'ccdad list', 'ccdad runway'\n" +
			"and the dashboard with exactly the numbers it would have without the mode.\n" +
			"The one thing it never does is switch.\n\n" +
			"'ccdad switch <account>' still works and still sticks. This is a policy for\n" +
			"the auto engine, not a lock -- the same line 'ccdad disable' takes.\n\n" +
			"Use it instead of disabling every account. Disabling reaches the same silence\n" +
			"and takes the probes, the forecast and the plain 'ccdad list' table with it,\n" +
			"leaves 'ccdad auto --once' on exit 4 forever, and re-arms rotation the moment\n" +
			"an account is added. None of that happens here.\n\n" +
			"It composes with hover rather than conflicting: hover decides what the numbers\n" +
			"are, manual decides whether ccdad acts on them. Watching hover's own numbers\n" +
			"while nothing moves is a reasonable way to decide whether to hand the wheel\n" +
			"over, and 'ccdad hover status' prints them throughout.\n\n" +
			"The daemon says so once when it starts a tick loop in this mode, and\n" +
			"'ccdad status', 'ccdad list' and 'ccdad doctor' all name it, so a fleet that\n" +
			"has stopped switching never looks like one that is broken.",
		Args:          usageArgs(cobra.ExactArgs(1)),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "status":
				return runManualStatus(cmd)
			case "on", "off":
				return setManual(cmd, args[0] == "on")
			}
			return UsageError("manual takes one of on, off, status, not %q", args[0])
		},
	}
	return cmd
}

// manualMode reads the effective value, falling back to the built-in defaults
// for a file that decodes and does not validate — which is what the engine
// itself runs on for exactly that file, so a report has to agree with it.
func manualMode(cmd *cobra.Command) (bool, error) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: %s still cannot be used and the engine will run on its built-in defaults until it can: %v\n",
			config.FileName, err)
		return config.Defaults().Manual, nil
	}
	return cfg.Manual, nil
}

// runManualStatus is the probe half, and it prints on both sides of its own exit
// code: somebody deciding whether to turn the mode off needs to be told what it
// is currently holding back.
func runManualStatus(cmd *cobra.Command) error {
	on, err := manualMode(cmd)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if !on {
		fmt.Fprintln(out, "Manual:  off  (ccdad switches accounts on its own)")
		return WithCode(errSilent, ExitProbeNegative)
	}
	fmt.Fprintln(out, "Manual:  on  (ccdad watches and never switches; 'ccdad switch <account>' still works)")
	return nil
}

// setManual writes the one key, having first read the current value under the
// same lock the write takes. Reading it outside would leave a window in which
// two `ccdad manual on` calls both decide they have work to do and both write,
// and the daemon detects change on the BYTES of this file — so a second
// identical write is an edit as far as it is concerned.
func setManual(cmd *cobra.Command, on bool) error {
	changed := false
	err := config.WithDocument(func(d *config.Document) error {
		cfg, cerr := d.Config()
		if cerr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"warning: %s still cannot be used and the engine will run on its built-in defaults until it can: %v\n",
				config.FileName, cerr)
			cfg = config.Defaults()
		}
		if cfg.Manual == on {
			// Nothing to write. The file is left byte-for-byte as it was.
			return nil
		}
		changed = true
		return d.Set(config.KeyManual, strconv.FormatBool(on))
	})
	if err != nil {
		return err
	}
	if !changed {
		fmt.Fprintf(cmd.ErrOrStderr(), "manual mode is already %s.\n", onOff(on))
		return WithCode(errSilent, ExitNothingToDo)
	}

	errw := cmd.ErrOrStderr()
	fmt.Fprintf(errw, "%s = %s\n", config.KeyManual, strconv.FormatBool(on))
	if !on {
		fmt.Fprintln(errw, "ccdad will switch accounts on its own again from the daemon's next tick.")
		return nil
	}
	// What it means, in the order a user needs it: what stopped, what did not,
	// and then the one thing somebody who typed this by mistake has to read —
	// that nothing will move the login off a spent account for them any more.
	fmt.Fprintln(errw,
		"ccdad will keep polling, keep the usage cache and the history current and keep\n"+
			"answering every table, and will not move the live login again until this is\n"+
			"turned off. 'ccdad switch <account>' still works and still sticks.\n"+
			"Nothing will now switch you off an account that runs out mid-session.")
	return nil
}

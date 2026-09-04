package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/switcher"
	"github.com/Kweiza/ccdaddy/internal/usage"
	"github.com/Kweiza/ccdaddy/internal/view"
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
			"It overrides the 'ccdad switch --strategy' flag too, and not just the config key\n" +
			"of the same name. That flag is required by the targetless grammar, so it still\n" +
			"has to be typed; the switch names the override on stderr rather than ranking as\n" +
			"though it had not been.\n\n" +
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

	// The daemon's own refusals, asked here because they are the half
	// strategy.Warmup deliberately does not carry. cfg and not forced: the
	// question a note answers is "will anything actually happen", and the answer
	// on a machine with hover off is the file's own probe_unknown, not the bit
	// this pass set for the sake of the numbers.
	//
	// ProbeAvailable is asked by this command rather than read off some record
	// the daemon left, because the daemon deliberately records NOTHING when it
	// fails — a machine with no Claude Code has made no attempt and must not
	// consume a rung — so there is no state to read and the only honest answer
	// is to look.
	facts := warmupFacts{
		activeUUID: activeUUID,
		liveKnown:  ev.LiveKnown,
		probeOn:    cfg.Effective().ProbeUnknown,
		noClaude:   probeAvailable(),
		now:        now,
	}

	if asJSON {
		if err := writeJSON(cmd, hoverPayload(on, plan, byUUID, facts)); err != nil {
			return err
		}
	} else if err := renderHoverStatus(cmd, on, plan, byUUID, facts); err != nil {
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

// probeAvailable is daemon.ProbeAvailable, indirected the same way
// probeClaudeInstall is: a test says what the machine has rather than inheriting
// the PATH of whatever host the suite is running on.
var probeAvailable = daemon.ProbeAvailable

// warmupFacts are the things the daemon knows about warming that the ranking
// pass cannot: which account it will refuse because it is live, whether the user
// has turned warming off, and whether there is a Claude Code to run at all.
//
// They are gathered once per invocation rather than per row. Every one of them
// is a property of the machine or the fleet, and asking ProbeAvailable once per
// account would be one exec per account for one answer.
type warmupFacts struct {
	activeUUID string
	liveKnown  bool
	probeOn    bool
	noClaude   error
	now        time.Time
}

func hoverPayload(on bool, plan strategy.HoverPlan,
	byUUID map[string]store.Account, facts warmupFacts) map[string]any {

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
		// Additive, and beside probeWanted rather than instead of it: the older
		// key keeps its meaning ("this window named no reset"), and this one
		// answers the question the older key was being misread as answering.
		if w.Warmup.Target {
			row["warmup"] = warmupJSON(w, facts)
		}
		if w.Credit {
			row["credit"] = true
		}
		if facts.activeUUID != "" && w.UUID == facts.activeUUID {
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

// warmupJSON is the machine-readable half of the note, and it carries the STATE
// rather than the sentence: a caller scripting against this wants to branch on
// "held" without matching English.
func warmupJSON(w strategy.HoverWindow, facts warmupFacts) map[string]any {
	out := map[string]any{
		"state":      warmupState(w, facts),
		"rolledOver": w.Warmup.RolledOver,
	}
	if w.Warmup.Streak > 0 {
		out["coldStreak"] = w.Warmup.Streak
	}
	if !w.Warmup.LastAttemptAt.IsZero() {
		out["lastAttemptAt"] = w.Warmup.LastAttemptAt.In(readerZone()).Format(time.RFC3339)
	}
	// The first reader ProbeState.LastError has ever had. It is reported and
	// never gated on, for the reason usage.ProbeState gives: an exit code cannot
	// tell a turn that was billed and then failed from one that never
	// authenticated.
	if w.Warmup.LastError != "" {
		out["lastError"] = w.Warmup.LastError
	}
	if at, ok := warmupAt(w, facts); ok {
		out["at"] = at.In(readerZone()).Format(time.RFC3339)
	}
	return out
}

// warmupState is the one word for what the warm-up loop will do about this row.
//
// The order is the order of finality: everything that means NOTHING WILL HAPPEN
// comes before anything that names a time, because a note that promised a
// warm-up on a machine with no Claude Code is the exact defect this replaces —
// the old build printed "a probe is queued" from a bool that could not see a
// single one of these.
func warmupState(w strategy.HoverWindow, facts warmupFacts) string {
	switch {
	case facts.noClaude != nil:
		return "impossible"
	case !facts.liveKnown:
		return "held"
	case w.UUID == facts.activeUUID:
		return "never"
	case !facts.probeOn:
		return "off"
	case w.Warmup.Credits:
		return "credits"
	case !w.Warmup.LastAttemptAt.IsZero() &&
		facts.now.Sub(w.Warmup.LastAttemptAt) < usage.ProbePollDelay:
		return "sent"
	case !w.Warmup.Eligible && !w.Warmup.LastAttemptAt.IsZero() &&
		facts.now.Sub(w.Warmup.LastAttemptAt) < usage.ProbeConfirmAfter:
		return "judging"
	case w.Warmup.Streak > 0:
		return "backoff"
	case !w.Warmup.Eligible && w.Warmup.NextAt.IsZero():
		return "spent"
	default:
		return "queued"
	}
}

// warmupAt is when the warm-up is expected to run, and whether an instant can
// honestly be named at all.
//
// It is the LATEST of the gate and the scheduler, because a warm-up runs on a
// tick where the account is poll-due as well as eligible: naming the gate's
// instant alone would be early by up to a poll interval, every cycle, which is
// the kind of note a user checks once and stops trusting.
func warmupAt(w strategy.HoverWindow, facts warmupFacts) (time.Time, bool) {
	switch warmupState(w, facts) {
	case "queued", "backoff":
	default:
		return time.Time{}, false
	}
	at := facts.now
	for _, t := range []time.Time{w.Warmup.NextAt, w.Warmup.PollAt} {
		if t.After(at) {
			at = t
		}
	}
	return at, true
}

// warmupNote is the sentence the table prints on the SLACK cell.
//
// One note per account, on the row ColdWindow actually targets, and a different
// sentence for every state — because the states differ in what a reader should
// DO. "held" and "off" are the machine's own configuration; "backoff" is the
// only one that means something is wrong; "never" is the mechanism working
// exactly as designed on the account it deliberately refuses.
func warmupNote(w strategy.HoverWindow, facts warmupFacts) string {
	switch warmupState(w, facts) {
	case "impossible":
		return "  (no clock running; nothing here can start one — see below)"
	case "held":
		return "  (no clock running; warm-ups wait until ccdad can tell which account is live)"
	case "never":
		return "  (no clock running; the live account is never warmed — its own next turn starts it)"
	case "off":
		return "  (no clock running; probe_unknown is off, so nothing will start it)"
	case "credits":
		return "  (no clock running; a window here is spent, so a warm-up could be billed to credits)"
	case "sent":
		ago := facts.now.Sub(w.Warmup.LastAttemptAt)
		return fmt.Sprintf("  (warmed %s ago; reading due in %s)",
			short(ago), short(usage.ProbePollDelay-ago))
	case "judging":
		return fmt.Sprintf("  (warmed %s ago; waiting for the reading that says whether it worked)",
			short(facts.now.Sub(w.Warmup.LastAttemptAt)))
	case "backoff":
		at, _ := warmupAt(w, facts)
		return fmt.Sprintf("  (%d warm-up%s woke nothing; next at %s)",
			w.Warmup.Streak, plural(w.Warmup.Streak, "", "s"), clock(at))
	case "spent":
		return fmt.Sprintf("  (warmed at %s; the next one belongs to the next rollover)",
			clock(w.Warmup.LastAttemptAt))
	}
	at, _ := warmupAt(w, facts)
	return fmt.Sprintf("  (no clock running; warm-up at %s)", clock(at))
}

// short is a duration a person reads at a glance rather than one Go printed.
//
// The trailing "0s" is trimmed because Duration.String() writes "5m0s" for five
// minutes, and a note is a sentence rather than a field dump.
func short(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Round(time.Second)/time.Second))
	}
	return strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
}

// clock is a local wall time, because the reader is deciding whether to wait for
// it and does so on their own clock.
func clock(t time.Time) string { return t.Local().Format("15:04") }

// displayThreshold is the derived pace target as a HUMAN table prints it.
//
// The derived figure is unclamped -- HoverDisplayCap's own comment says why the
// clamp had to come out of the ranking -- but a percentage above 100 reads as a
// bug to a person who has not read that comment, so the column stops at 100. The
// SLACK column beside it is not clamped and must not be: it is the quantity the
// engine actually ordered on, and a table that showed a doctored version of the
// decision axis would be worse than one whose two columns do not subtract.
// thresholdCeilingFooter is what tells the reader when that has happened.
func displayThreshold(v float64) float64 {
	if v > strategy.HoverDisplayCap {
		return strategy.HoverDisplayCap
	}
	return v
}

// thresholdCeilingFooter names the rows whose printed threshold is the ceiling
// rather than the figure the ranking used, so `threshold - util = slack` failing
// to hold on those rows is explained rather than discovered.
func thresholdCeilingFooter(out io.Writer, plan strategy.HoverPlan) {
	n := 0
	for _, row := range plan.Windows {
		if row.Threshold > strategy.HoverDisplayCap {
			n++
		}
	}
	if n == 0 {
		return
	}
	fmt.Fprintf(out, "\n%d threshold(s) above show %.0f%% because their pace target ran past it: far\n",
		n, strategy.HoverDisplayCap)
	fmt.Fprintf(out, "enough through their own cycle that nothing is being held back. The engine\n")
	fmt.Fprintf(out, "ranks on the real figure; `ccdad hover status --json` carries it.\n")
}

// renderHoverStatus is the human table: one row per account, one cell per
// window, each cell the pair "what this window has used / what hover holds it
// to".
//
// It used to be one row per account per WINDOW, carrying ELAPSED, UTIL,
// THRESHOLD and SLACK. Four accounts holding three windows was twelve rows to
// read one fleet off, and none of the other three tables was shaped that way.
// What survives is the pair that answers the question this command exists for:
// what did hover choose, and against what. ELAPSED and SLACK move to --json,
// which carries the whole derivation -- and hover's justification, that an
// omakase mode is only acceptable if you can see what it chose, is met by the
// pair plus the footer stating the rule that produced it.
func renderHoverStatus(cmd *cobra.Command, on bool, plan strategy.HoverPlan,
	byUUID map[string]store.Account, facts warmupFacts) error {

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Hover:   %s\n", onOff(on))
	fmt.Fprintf(out, "Pool:    %d usable accounts, so each threshold is the share of its own window that\n", plan.Usable)
	fmt.Fprintf(out, "         has elapsed, plus 100/%d points. A window far enough through its own\n",
		max(plan.Usable, 1))
	fmt.Fprintf(out, "         cycle earns more than 100, which means no restraint -- there is nobody\n")
	fmt.Fprintf(out, "         to hand the work to. This column stops at %.0f; --json carries the rest.\n\n",
		strategy.HoverDisplayCap)

	if len(plan.Windows) == 0 {
		return nil
	}

	// The window each account's warm-up aims at, so that a SECOND stopped clock
	// on the same account can say what it is waiting behind rather than
	// repeating a promise that belongs to one row.
	aims := map[string]usage.WindowName{}
	for _, row := range plan.Windows {
		if row.Warmup.Target {
			aims[row.UUID] = row.Window
		}
	}

	// ONE ROW PER ACCOUNT, one cell per window, in the order and under the
	// headers view.ColumnsOf gives the other three tables. It used to be one
	// row per account per WINDOW -- four accounts carrying three windows was
	// twelve rows to read a fleet off -- and the four figures per row went with
	// it. UTIL and THRESHOLD stay, as one cell each: they are the pair that
	// answers "what did hover choose, and against what". ELAPSED moves to
	// --json, where the whole derivation still is; the footer below already
	// states the rule it feeds.
	names := make([]usage.WindowName, 0, len(plan.Windows))
	byAccount := map[string][]strategy.HoverWindow{}
	order := []string{}
	for _, row := range plan.Windows {
		names = append(names, row.Window)
		if _, seen := byAccount[row.UUID]; !seen {
			order = append(order, row.UUID)
		}
		byAccount[row.UUID] = append(byAccount[row.UUID], row)
	}
	cols := view.ColumnsOfNames(names)

	cells := make([][]string, 0, len(order))
	for _, uuid := range order {
		a := byUUID[uuid]
		marker := " "
		if facts.activeUUID != "" && uuid == facts.activeUUID {
			marker = "*"
		}
		byWindow := map[usage.WindowName]strategy.HoverWindow{}
		for _, row := range byAccount[uuid] {
			byWindow[row.Window] = row
		}
		cs := []string{fmt.Sprintf("%s %d", marker, a.Idx), a.Label()}
		for _, w := range cols.Windows {
			row, ok := byWindow[w.Name]
			if !ok {
				// This account carries no such window. "-" and never a pair of
				// zeroes, which would read as a window it holds and has not
				// touched.
				cs = append(cs, "-")
				continue
			}
			// util/threshold, both with their sign, because the used half has
			// to be the identical byte the other tables print for the same
			// window and the threshold half is the number it was judged by.
			cs = append(cs, fmt.Sprintf("%.0f%%/%.0f%%", row.Utilization, displayThreshold(row.Threshold)))
		}
		// The notes ride on the LAST cell rather than in a column of their own:
		// a note is up to eighty characters and a cell is six, so as a column
		// it would push every note to one fixed offset far to the right of what
		// it explains, and a row without one would leave a gap the eye has to
		// cross to find nothing.
		cs[len(cs)-1] += hoverNotes(byAccount[uuid], aims, facts)
		cells = append(cells, cs)
	}
	head := append([]string{"  IDX", "ACCOUNT"}, cols.Headers()...)
	if err := columns(out, head, cells, nil); err != nil {
		return err
	}
	if legend := cols.Legend(); legend != "" {
		fmt.Fprintln(out, legend)
	}
	fmt.Fprintln(out, "each cell is used/threshold; the threshold is the share of that window's own cycle that has elapsed plus the pool share.")
	thresholdCeilingFooter(out, plan)
	warmupFooters(out, plan, byUUID, facts)
	return nil
}

// hoverNotes is one account's notes, which used to ride on one row each and now
// have to share a cell.
//
// They are joined rather than reduced to the first, because they are about
// different WINDOWS: an account can have a warm-up aimed at one window and a
// stopped clock on another, and printing only the first would hide whichever
// one the reader came looking for, half the time. Each note already names its
// own window where the window matters.
func hoverNotes(rows []strategy.HoverWindow, aims map[string]usage.WindowName, facts warmupFacts) string {
	var out []string
	for _, row := range rows {
		switch {
		case row.Warmup.Target:
			out = append(out, strings.TrimSpace(warmupNote(row, facts)))
		case row.ProbeWanted:
			if aim, behind := aims[row.UUID]; behind {
				out = append(out, fmt.Sprintf("(%s has no clock running; the warm-up aims at %s first)", row.Window, aim))
				break
			}
			out = append(out, fmt.Sprintf("(%s is in use and still reported no reset time; a warm-up cannot fix that)", row.Window))
		case row.Credit:
			out = append(out, "(primary, metered in credits)")
		}
	}
	if len(out) == 0 {
		return ""
	}
	return "  " + strings.Join(out, " ")
}

// warmupFooters say the things that do not fit on a row: why nothing can be
// warmed at all, and what the last warm-up of an account that is backing off
// actually reported.
//
// The second one is the whole reason ProbeState.LastError was worth keeping. It
// had no reader anywhere in the binary, so an account whose warm-ups failed
// every cycle looked, in every command ccdad has, exactly like one waiting its
// turn.
func warmupFooters(out io.Writer, plan strategy.HoverPlan,
	byUUID map[string]store.Account, facts warmupFacts) {

	if facts.noClaude != nil {
		fmt.Fprintf(out, "\nNo Claude Code on this PATH, so no stopped clock above can be started: %v\n",
			facts.noClaude)
	}
	first := true
	for _, row := range plan.Windows {
		if !row.Warmup.Target || row.Warmup.Streak == 0 {
			continue
		}
		if first {
			fmt.Fprintln(out)
			first = false
		}
		a := byUUID[row.UUID]
		if row.Warmup.LastError != "" {
			fmt.Fprintf(out, "%d %s: last warm-up exited with: %s\n", a.Idx, a.Label(), row.Warmup.LastError)
			continue
		}
		// The state an exit code cannot express, and the reason the verdict is
		// taken from the window instead of from the child.
		fmt.Fprintf(out, "%d %s: last warm-up reported success and the clock stayed stopped\n",
			a.Idx, a.Label())
	}
}

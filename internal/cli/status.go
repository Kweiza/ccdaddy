package cli

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// timeNow is the clock the dashboard reads. It is a package var so a test can
// fix it: every relative figure here — uptime, reset-in, the age of a reading —
// is a difference against it.
var timeNow = time.Now

// observeDaemon is the liveness probe, as a seam. A filesystem where locks do
// not work is not something a test can arrange, and it is exactly the case this
// command must not fall over on.
var observeDaemon = daemon.Observe

// unreadable is what a value that could not be read renders as. Never "0%" —
// §7.2, and cswap's version of that bug parked its engine on the account that
// reset last, because one expired token made every account look empty.
const unreadable = "?"

func newStatusCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the engine dashboard",
		Long: "status renders what is already on disk: the accounts, the cached usage\n" +
			"readings and the daemon's own published state. It never fetches — the\n" +
			"usage endpoint allows roughly 28-30 requests per identity per rolling\n" +
			"hour on a sliding window, so a dashboard that polled would let one burst\n" +
			"saturate an account for a full hour.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a machine-readable object on stdout")
	return cmd
}

// runStatus is the dashboard, factored out of the command so bare `ccdad` can
// dispatch to exactly this and not to a near-copy of it (§9.2).
//
// It exits 0 for every answer it can render, including "no daemon". status is a
// dashboard, not a probe: exit 5 is `daemon status`'s, and a `ccdad status` that
// exited non-zero when the daemon happened to be down would make the obvious
// health-check script wrong. The only failure is one that leaves nothing to
// render at all — an unreadable account store.
func runStatus(cmd *cobra.Command, asJSON bool) error {
	now := timeNow()

	s, err := store.Open()
	if err != nil {
		return err
	}
	accounts := s.Accounts()

	// Tolerated, like `list` does: an unreadable live file costs the active
	// marker and nothing else, and status is what a user reaches for when
	// something is already wrong.
	live, _ := cclink.Load()
	active, hasActive := attributeLive(live, accounts, s.Credentials)

	report, probeErr := observeDaemon()
	if probeErr != nil {
		// A human notice, so a --json caller still receives exactly one document
		// on stdout (§9.4).
		fmt.Fprintf(cmd.ErrOrStderr(), "Cannot tell whether a daemon is running: %v\n", probeErr)
	}
	if report.StatusErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "The daemon's status file could not be read: %v\n", report.StatusErr)
	}

	// The cache is the authority for quota, for both this command and `list`.
	// §8.4's "can never disagree" is only true because neither of them has a
	// second source for a number — see daemon.Status's authority note.
	cache, err := usage.LoadCache()
	if err != nil {
		return err
	}
	if cerr := cache.LoadError(); cerr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "The usage cache could not be read: %v\n", cerr)
	}

	engine := map[string]daemon.AccountStatus{}
	for _, a := range report.Status.Accounts {
		engine[a.UUID] = a
	}

	rows := quotaRows(accounts, cache, active, hasActive, now)
	for i := range rows {
		rows[i].Engine = engine[rows[i].Account.UUID]
	}

	if asJSON {
		return writeJSON(cmd, statusPayload(report, probeErr, rows, active, hasActive, now))
	}
	return renderStatus(cmd, report, rows, now)
}

// quotaRows pairs every account with its cached reading.
//
// `list` builds its rows through this too, and that is what §8.4's "`ccdad
// list` and `ccdad status --json` can never disagree" actually rests on: one
// cache, read one way, into one shape. Two commands each deriving headroom for
// themselves would agree until the day one of them was changed.
//
// Engine state is deliberately NOT filled in here. It comes from status.json,
// which is the daemon's own document and no part of what `list` reports.
func quotaRows(accounts []store.Account, cache *usage.Cache,
	active store.Account, hasActive bool, now time.Time) []statusRow {

	rows := make([]statusRow, 0, len(accounts))
	for _, a := range accounts {
		row := statusRow{Account: a, Active: hasActive && a.UUID == active.UUID}
		if entry, ok := cache.Get(a.UUID); ok && entry.Snapshot != nil {
			row.Entry, row.HasEntry = entry, true
			row.Headroom = strategy.HeadroomOf(entry.Snapshot)
			row.Pace = entry.Snapshot.Pace(now)
		}
		rows = append(rows, row)
	}
	return rows
}

// statusRow is one account with everything the dashboard knows about it, from
// each field's one authoritative source.
type statusRow struct {
	Account  store.Account
	Active   bool
	Entry    usage.Entry
	HasEntry bool
	Headroom strategy.Headroom
	Pace     map[usage.WindowName]usage.Pace
	Engine   daemon.AccountStatus
}

// binding is the window that decides this account's headroom, together with
// when it rolls over. It is what both the USED and the RESETS IN columns read,
// so the two always describe the same window.
//
// The Known check is redundant TODAY and is kept deliberately: with no window
// reporting a utilization, strategy leaves Binding as the empty WindowName and
// the loop below matches nothing anyway. A mutation removing it survives for
// exactly that reason. It stays because the alternative is for this function's
// correctness to rest on an invariant of another package's zero value.
func (r statusRow) binding() (usage.NamedWindow, bool) {
	if !r.HasEntry || !r.Headroom.Known {
		return usage.NamedWindow{}, false
	}
	// AllWindows, not RateLimitWindows: the binding window can be a per-model or
	// per-surface weekly one out of limits[], and looking it up in the fixed five
	// alone would leave both columns blank for an account whose headroom is
	// perfectly well known.
	for _, w := range r.Entry.Snapshot.AllWindows() {
		if w.Name == r.Headroom.Binding {
			return w, true
		}
	}
	return usage.NamedWindow{}, false
}

func renderStatus(cmd *cobra.Command, report daemon.Report, rows []statusRow, now time.Time) error {
	out := cmd.OutOrStdout()

	switch report.State {
	case daemon.DaemonRunning:
		line := "Daemon:  running"
		if report.HasStatus && report.Status.PID != 0 {
			line += fmt.Sprintf("  pid %d", report.Status.PID)
		}
		if report.HasStatus && !report.Status.StartedAt.IsZero() {
			line += "  up " + humanDuration(now.Sub(report.Status.StartedAt))
		}
		fmt.Fprintln(out, line)
	case daemon.DaemonStopped:
		fmt.Fprintln(out, "Daemon:  not running  (start one with 'ccdad daemon start')")
	default:
		fmt.Fprintln(out, "Daemon:  unknown  (the lock could not be probed)")
	}

	if len(rows) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "No accounts yet. Run 'ccdad add' to log one in.")
		return nil
	}

	activeLabel := "none of the managed accounts"
	for _, r := range rows {
		if r.Active {
			activeLabel = r.Account.Label()
		}
	}
	fmt.Fprintf(out, "Active:  %s\n\n", activeLabel)

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  IDX\tACCOUNT\tTYPE\tUSED\tWINDOW\tRESETS IN\tPACE\tAGE")
	for _, r := range rows {
		marker := " "
		if r.Active {
			marker = "*"
		}
		used, windowName := unreadable, "-"
		if bw, ok := r.binding(); ok {
			if pct, ok := bw.Percent(); ok {
				used = fmt.Sprintf("%.0f%%", pct)
			}
			windowName = string(bw.Name)
		}
		resetsIn := r.resetsLabel(now)
		age := unreadable
		if r.HasEntry {
			if d, ok := r.Entry.Age(now); ok {
				age = humanDuration(d)
			}
		}
		suffix := ""
		if r.Account.Disabled {
			suffix = "  (disabled)"
		}
		fmt.Fprintf(w, "%s %d\t%s\t%s\t%s\t%s\t%s\t%s\t%s%s\n",
			marker, r.Account.Idx, r.Account.Label(), r.Account.Kind,
			used, windowName, resetsIn, r.paceLabel(), age, suffix)
	}
	return w.Flush()
}

// leftLabel is how much of the binding window is LEFT, which is the column
// `ccdad list` carries.
//
// It is the complement of status's USED column and deliberately so: `list` is
// where an account is chosen, and headroom is the quantity that choice is made
// on — it is what the engine itself ranks by. The two columns are labelled, so
// a reader is never asked to guess which way round a bare percentage runs.
//
// Never "0%" for an account that could not be read (§7.2).
func (r statusRow) leftLabel() string {
	if !r.Headroom.Known {
		return unreadable
	}
	return fmt.Sprintf("%.0f%%", r.Headroom.Pct)
}

// resetsLabel is when the binding window rolls over, as a span. Both tables
// render it from here so the two can never describe one reset two ways.
func (r statusRow) resetsLabel(now time.Time) string {
	bw, ok := r.binding()
	if !ok {
		return "-"
	}
	reset, ok := bw.Reset()
	if !ok {
		return "-"
	}
	return humanDuration(reset.Sub(now))
}

// paceLabel is §7.5's human half: how the binding window's consumption compares
// with the time elapsed in it.
//
// It reports the BINDING window's pace and no other, so the column describes the
// same window the two columns beside it do. Every window's pace is in --json.
//
// The projection is deliberately absent. §7.5 keeps projectedExhaustionAt and
// willLastToReset out of every human view, because a straight line through
// bursty real usage is too rough to present as fact — and the way that sticks is
// that nothing here can reach them: they are behind usage.Pace.Projection.
func (r statusRow) paceLabel() string {
	bw, ok := r.binding()
	if !ok {
		return "-"
	}
	p, ok := r.Pace[bw.Name]
	if !ok {
		// Either not a weekly window, or less than a day since its reset — in
		// which case elapsed time is tiny and almost any usage divides out as
		// "far ahead". Saying nothing is the specified answer.
		return "-"
	}
	if p.AheadOfPace {
		return "ahead"
	}
	return "on pace"
}

func statusPayload(report daemon.Report, probeErr error, rows []statusRow, active store.Account, hasActive bool, now time.Time) map[string]any {
	// The daemon half is daemonJSON's, and `ccdad daemon status --json` nests
	// the same object under the same key: two commands describing one daemon
	// must not describe it two ways.
	d := daemonJSON(report, probeErr)

	out := []map[string]any{}
	for _, r := range rows {
		row := accountJSON(r.Account)
		row["active"] = r.Active
		if u := usageJSON(r, now); u != nil {
			row["usage"] = u
		}
		if e := engineJSON(r.Engine); e != nil {
			row["engine"] = e
		}
		out = append(out, row)
	}

	payload := map[string]any{
		"schemaVersion": 1,
		"daemon":        d,
		"accounts":      out,
	}
	if hasActive {
		payload["activeUuid"] = active.UUID
	}
	return payload
}

// usageJSON is the quota half of a row, or nil when there is no reading.
//
// nil rather than an object of zeros, for the same reason the table prints "?":
// an account that could not be read is not an empty one, and a consumer that
// sees no `usage` key cannot mistake it for one at 0%.
func usageJSON(r statusRow, now time.Time) map[string]any {
	if !r.HasEntry {
		return nil
	}
	out := map[string]any{"fetchedAt": r.Entry.FetchedAt}
	if age, ok := r.Entry.Age(now); ok {
		out["ageSeconds"] = int(age.Seconds())
	}
	if r.Headroom.Known {
		out["headroomPct"] = r.Headroom.Pct
		out["bindingWindow"] = string(r.Headroom.Binding)
	}

	// AllWindows, so that bindingWindow above always names a key that is in
	// here: the window that binds can be a per-model or per-surface weekly one
	// out of limits[], and a consumer resolving the name against the fixed five
	// would find nothing for an account whose headroom is perfectly well known.
	windows := map[string]any{}
	for _, w := range r.Entry.Snapshot.AllWindows() {
		if !w.Present {
			continue
		}
		entry := map[string]any{}
		if pct, ok := w.Percent(); ok {
			entry["utilizationPct"] = pct
		}
		if reset, ok := w.Reset(); ok {
			entry["resetsAt"] = reset
		}
		windows[string(w.Name)] = entry
	}
	if len(windows) > 0 {
		out["windows"] = windows
	}

	pace := map[string]any{}
	for name, p := range r.Pace {
		entry := map[string]any{
			"expectedPct": p.ExpectedPct,
			"actualPct":   p.ActualPct,
			"aheadOfPace": p.AheadOfPace,
		}
		// §7.5's --json-only half. This is the one place in ccdad allowed to
		// reach through Pace.Projection, and the human renderer above must never
		// gain a second one.
		if proj, ok := p.Projection(); ok {
			entry["projectedExhaustionAt"] = proj.ExhaustionAt
			entry["willLastToReset"] = proj.WillLastToReset
		}
		pace[string(name)] = entry
	}
	if len(pace) > 0 {
		out["pace"] = pace
	}
	return out
}

// engineJSON is what the daemon published about this account, or nil when it
// published nothing — which is every account when no daemon has ever run.
func engineJSON(a daemon.AccountStatus) map[string]any {
	if a.UUID == "" {
		return nil
	}
	out := map[string]any{}
	if a.State != "" {
		out["state"] = string(a.State)
	}
	if !a.NextPollAt.IsZero() {
		out["nextPollAt"] = a.NextPollAt
	}
	if !a.LastPollAt.IsZero() {
		out["lastPollAt"] = a.LastPollAt
	}
	if a.LastPollError != "" {
		out["lastPollError"] = a.LastPollError
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// humanDuration renders a span at the scale a reader cares about. A reset
// already behind us is "due" rather than a negative number: the endpoint has not
// rolled the window over yet, which is a real state and not a clock error.
func humanDuration(d time.Duration) string {
	if d < 0 {
		return "due"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

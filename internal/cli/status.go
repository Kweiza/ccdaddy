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
// unknown is never read as zero, and cswap's version of that bug parked its
// engine on the account that reset last, because one expired token made every
// account look empty.
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
// dispatch to exactly this and not to a near-copy of it.
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
		// on stdout.
		fmt.Fprintf(cmd.ErrOrStderr(), "Cannot tell whether a daemon is running: %v\n", probeErr)
	}
	if report.StatusErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "The daemon's status file could not be read: %v\n", report.StatusErr)
	}

	// The cache is the authority for quota, for both this command and `list`.
	// That `ccdad list` and `ccdad status --json` can never disagree is only
	// true because neither of them has a second source for a number — see
	// daemon.Status's authority note.
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

	rows := quotaRows(accounts, cache, active, hasActive, now, rowThresholds(cmd, now))
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
// `list` builds its rows through this too, and that is what "`ccdad list` and
// `ccdad status --json` can never disagree" actually rests on: one cache, read
// one way, into one shape. Two commands each deriving headroom for themselves
// would agree until the day one of them was changed.
//
// Engine state is deliberately NOT filled in here. It comes from status.json,
// which is the daemon's own document and no part of what `list` reports.
func quotaRows(accounts []store.Account, cache *usage.Cache, active store.Account,
	hasActive bool, now time.Time, thresholds strategy.Thresholds) []statusRow {

	rows := make([]statusRow, 0, len(accounts))
	for _, a := range accounts {
		row := statusRow{Account: a, Active: hasActive && a.UUID == active.UUID}
		if entry, ok := cache.Get(a.UUID); ok && entry.Snapshot != nil {
			row.Entry, row.HasEntry = entry, true
			row.Headroom = strategy.HeadroomOf(entry.Snapshot, thresholds)
			row.Pace = entry.Snapshot.Pace(now)
		}
		rows = append(rows, row)
	}
	return rows
}

// rowThresholds is what `status` and `list` measure their rows against.
//
// It goes through RankOptions rather than reading the threshold keys itself, so
// the number a row is reported against is the number the engine ranked on. Two
// constructions of the same bundle would agree until the day one of them was
// changed.
//
// A config that cannot be used is a notice and not a failure: refusing to render
// a dashboard because a threshold was mistyped is a worse answer than rendering
// it against the documented defaults, which is the same call `ccdad auto` makes.
func rowThresholds(cmd *cobra.Command, now time.Time) strategy.Thresholds {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "note: %v; the rows are measured against the built-in thresholds\n", err)
		cfg = config.Defaults()
	}
	return cfg.RankOptions(now).Thresholds()
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

// reportedName is the window this account is REPORTED against, which is not
// always the one it is ordered on.
//
// A tripped WEEKLY cap wins, because it is the one that will not come back for
// days: an account whose five-hour window rolls over in eight minutes is still
// unusable until Friday, and naming the five-hour window would tell the user to
// wait eight minutes for it. Ordering still runs on Headroom.Slack, which is the
// tightest window whichever family it belongs to; this rule moves no account in
// the ranking.
func (r statusRow) reportedName() usage.WindowName {
	if r.Headroom.HasFloor {
		return r.Headroom.Floor
	}
	return r.Headroom.Binding
}

// reported resolves reportedName to the window itself, together with when it
// rolls over. It is what the USED, WINDOW, RESETS IN and PACE columns all read,
// so they always describe the same window.
//
// The Known check is redundant TODAY and is kept deliberately: with no window
// reporting a utilization, strategy leaves both names as the empty WindowName
// and the loop below matches nothing anyway. A mutation removing it survives for
// exactly that reason. It stays because the alternative is for this function's
// correctness to rest on an invariant of another package's zero value.
func (r statusRow) reported() (usage.NamedWindow, bool) {
	if !r.HasEntry || !r.Headroom.Known {
		return usage.NamedWindow{}, false
	}
	// AllWindows, not RateLimitWindows: the reported window can be a per-model or
	// per-surface weekly one out of limits[], and looking it up in the fixed five
	// alone would leave both columns blank for an account whose headroom is
	// perfectly well known.
	want := r.reportedName()
	for _, w := range r.Entry.Snapshot.AllWindows() {
		if w.Name == want {
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
		if bw, ok := r.reported(); ok {
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
// It stays on the ORDERING window while RESETS IN beside it names the reported
// one, so on an account with a tripped weekly cap the two describe different
// windows. That is the intended trade: LEFT has to keep meaning "how much of the
// tightest window is left" or it stops being the figure the ranking used, and
// RESETS IN has to name the cap that actually holds the account back or it tells
// a user to wait ten minutes for an account that is gone until Friday.
//
// Never "0%" for an account that could not be read.
func (r statusRow) leftLabel() string {
	if !r.Headroom.Known {
		return unreadable
	}
	return fmt.Sprintf("%.0f%%", r.Headroom.Pct)
}

// resetsLabel is when the reported window rolls over, as a span. Both tables
// render it from here so the two can never describe one reset two ways.
func (r statusRow) resetsLabel(now time.Time) string {
	bw, ok := r.reported()
	if !ok {
		return "-"
	}
	reset, ok := bw.Reset()
	if !ok {
		return "-"
	}
	return humanDuration(reset.Sub(now))
}

// paceLabel is the pace reading's human half: how the reported window's
// consumption compares with the time elapsed in it.
//
// It reports the REPORTED window's pace and no other, so the column describes
// the same window the two columns beside it do. Every window's pace is in
// --json.
//
// The projection is deliberately absent: projectedExhaustionAt and
// willLastToReset stay out of every human view, because a straight line through
// bursty real usage is too rough to present as fact — and the way that sticks is
// that nothing here can reach them: they are behind usage.Pace.Projection.
func (r statusRow) paceLabel() string {
	bw, ok := r.reported()
	if !ok {
		return "-"
	}
	p, ok := r.Pace[bw.Name]
	if !ok {
		// Either a window with no length to measure against, or less than a
		// seventh of the window since its reset — in which case elapsed time is
		// tiny and almost any usage divides out as "far ahead". Saying nothing
		// is the deliberate answer.
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
		// slack and windowThreshold are the pair the DECISION is made on: the
		// threshold the tightest window was given, and how far under it that
		// window is. headroomPct is 100 minus the same window's utilization, so a
		// script can see both the axis and the display figure rather than having
		// to infer one from the other.
		out["slack"] = r.Headroom.Slack
		out["windowThreshold"] = r.Headroom.Threshold
		// bindingWindow is the REPORTED window, which is a tripped weekly cap
		// when there is one. It is the name the WINDOW column prints, and it can
		// differ from the window slack was measured on: what is tightest right
		// now and what will still be tight in two days are two questions.
		out["bindingWindow"] = string(r.reportedName())
	}

	// AllWindows, so that bindingWindow above always names a key that is in
	// here: the window reported can be a per-model or per-surface weekly one
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
	// A weekly cap the wire gave nothing to name cannot be in `windows`: there
	// is no key to file it under, and none a threshold could be set on either.
	// The count is the only place it is visible at all, and without it the
	// reading would say an account has quota nobody can see it does not have.
	// It is emitted only when non-zero, so an ordinary payload does not carry a
	// field that is always 0.
	if n := r.Entry.Snapshot.UnnamableLimits(); n > 0 {
		out["unnamableWeeklyCaps"] = n
	}

	pace := map[string]any{}
	for name, p := range r.Pace {
		entry := map[string]any{
			"expectedPct": p.ExpectedPct,
			"actualPct":   p.ActualPct,
			"aheadOfPace": p.AheadOfPace,
		}
		// The projection is kept out of the human table. This is the only place
		// in the CLI allowed to reach through Pace.Projection, and the human
		// renderer above must never gain one.
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

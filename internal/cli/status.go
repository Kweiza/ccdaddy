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
	"github.com/Kweiza/ccdaddy/internal/switcher"
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

	// ONE evaluation, and both answers come out of it. The dashboard used to ask
	// the engine only when hover was on; naming the mode is a second question it
	// cannot answer from the cache, so the pass now always runs — and running it
	// twice, once for the thresholds and once for the mode, is how `status` would
	// acquire a second source for a number it already had.
	cfg := rowConfig(cmd)
	plan, decided, planErr := enginePlan(s, now)
	if planErr != nil {
		// Said out loud: the Mode line simply disappears otherwise, which reads
		// as "the engine has nothing to say" rather than "it could not be asked".
		fmt.Fprintf(cmd.ErrOrStderr(), "The engine could not be asked which mode it is ranking in: %v\n", planErr)
	}
	var mode strategy.Mode
	hasMode := false
	if decided {
		mode, hasMode = plan.Result.Mode, true
	}

	rows := quotaRows(accounts, cache, active, hasActive, now, thresholdsFrom(cmd, cfg, now, plan, planErr))
	for i := range rows {
		rows[i].Engine = engine[rows[i].Account.UUID]
	}

	if asJSON {
		return writeJSON(cmd, statusPayload(report, probeErr, rows, active, hasActive, mode, hasMode, now))
	}
	return renderStatus(cmd, report, rows, mode, hasMode, now)
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
	hasActive bool, now time.Time, thresholds func(uuid string) strategy.Thresholds) []statusRow {

	rows := make([]statusRow, 0, len(accounts))
	for _, a := range accounts {
		row := statusRow{Account: a, Active: hasActive && a.UUID == active.UUID}
		if entry, ok := cache.Get(a.UUID); ok && entry.Snapshot != nil {
			row.Entry, row.HasEntry = entry, true
			row.Headroom = strategy.HeadroomOf(entry.Snapshot, thresholds(a.UUID))
			row.Pace = entry.Snapshot.Pace(now)
		}
		rows = append(rows, row)
	}
	return rows
}

// enginePlan is the decision the engine would make right now, asked for rather
// than reconstructed.
//
// It is a package var because a test that wants a particular plan should not
// have to arrange a store, a cache and an on-disk cooldown to get one.
//
// The second return is switcher.Evaluation.Decided, and dropping it was a trap:
// a zero Plan does not stringify to nothing, it stringifies to plausible values.
// Mode(0) is "headroom", so a caller that read Mode off a pass that never ran
// would print a real answer nobody computed. Every caller here needs the bit,
// which is why it rides with the plan rather than being asked for separately.
var enginePlan = func(s *store.Store, now time.Time) (strategy.Plan, bool, error) {
	ev, err := switcher.Evaluate(s, switcher.EvalOptions{Now: now})
	if err != nil {
		return strategy.Plan{}, false, err
	}
	return ev.Plan, ev.Decided, nil
}

// modeLine is the ranking mode with the question it is asking. Recovery is the
// one a user needs told: the columns are identical to the ordinary case, so
// nothing else on the dashboard distinguishes "the engine is ranking by soonest
// reset because everything is spent" from "the engine has nothing to do".
//
// The label column is nine characters wide, matching the Daemon: and Active:
// lines above it. No branch may contain the substring "exhaust": the human table
// keeps the projection to --json, and TestTheProjectionIsJSONOnly fails on that
// word appearing anywhere in stdout.
func modeLine(m strategy.Mode) string {
	switch m {
	case strategy.ModeRecovery:
		return "Mode:    recovery  (every account is over its threshold; ranking by soonest reset inside an hour, by headroom past it)"
	case strategy.ModeConsumeFirst:
		return "Mode:    consume-first  (spending perishable weekly quota before it expires)"
	default:
		return "Mode:    headroom  (at least one account has room, or could not be read)"
	}
}

// rowThresholds is what `status` and `list` measure their rows against: one
// bundle per account, because under hover there is no single one.
//
// It goes through RankOptions rather than reading the threshold keys itself, so
// the number a row is reported against is the number the engine ranked on. Two
// constructions of the same bundle would agree until the day one of them was
// changed.
//
// Under hover that rule is what forces the whole engine to be asked. Hover
// derives a threshold per account from how far through its own window each one
// is AND from how many accounts are left to share the remainder with, and that
// pool is not the account list this command renders: it excludes an account
// whose credentials cannot be installed and one that is quarantined, neither of
// which is visible from here. A table built locally would divide the quota by a
// different count and print numbers wrong in a new way rather than in the old
// one, so the plan the engine actually made is what supplies them.
//
// A config that cannot be used is a notice and not a failure: refusing to render
// a dashboard because a threshold was mistyped is a worse answer than rendering
// it against the documented defaults, which is the same call `ccdad auto` makes.
// An engine that cannot be asked is the same kind of notice, and it falls back
// to the configured bundle because that is the last table anyone can name.
func rowThresholds(cmd *cobra.Command, s *store.Store, now time.Time) func(uuid string) strategy.Thresholds {
	cfg := rowConfig(cmd)
	if !cfg.RankOptions(now).Hover {
		// `list` asks the engine only when hover is on, because with hover off
		// the configured bundle answers every row and an evaluation would be a
		// ranking pass nothing reads. `status` is the one that always asks, and
		// it has a second question to put.
		return thresholdsFrom(cmd, cfg, now, strategy.Plan{}, nil)
	}
	plan, _, err := enginePlan(s, now)
	return thresholdsFrom(cmd, cfg, now, plan, err)
}

// rowConfig is the config the rows are measured against, with the one notice a
// config that cannot be used earns. A dashboard that refused to render because a
// threshold was mistyped is a worse answer than one rendered against the
// documented defaults, which is the same call `ccdad auto` makes.
func rowConfig(cmd *cobra.Command) config.Config {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "note: %v; the rows are measured against the built-in thresholds\n", err)
		return config.Defaults()
	}
	return cfg
}

// thresholdsFrom is rowThresholds without the fetch, so a caller that has
// already asked the engine does not ask it twice. The plan is read only when
// hover is on; with hover off it is ignored entirely, including its error.
func thresholdsFrom(cmd *cobra.Command, cfg config.Config, now time.Time,
	plan strategy.Plan, planErr error) func(uuid string) strategy.Thresholds {

	o := cfg.RankOptions(now)
	configured := func(string) strategy.Thresholds { return o.Thresholds() }
	if !o.Hover {
		return configured
	}
	if planErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"note: hover is on, but the thresholds it derived could not be read (%v); the rows are measured against the configured ones\n", planErr)
		return configured
	}
	if plan.Hover == nil {
		// Hover is on and the engine made no pass, which is what "nothing has
		// ever been polled" looks like from here: there was no pool to divide.
		// Every row is then unread and none of them reaches a threshold at all,
		// so what this answers is only what hover answers for an account it has
		// never seen.
		var none strategy.HoverPlan
		return none.For
	}
	return plan.Hover.For
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
// tightest window whichever family it belongs to, so this rule moves no account
// in the ordinary order. It is not inert in RECOVERY order: what an account has
// to wait out is the weekly floor, so the same field decides whether it counts
// as returning inside the horizon.
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

func renderStatus(cmd *cobra.Command, report daemon.Report, rows []statusRow,
	mode strategy.Mode, hasMode bool, now time.Time) error {
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
	fmt.Fprintf(out, "Active:  %s\n", activeLabel)
	if hasMode {
		fmt.Fprintln(out, modeLine(mode))
	}
	fmt.Fprintln(out)

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
	if r.Headroom.Known {
		return fmt.Sprintf("%.0f%%", r.Headroom.Pct)
	}
	// Headroom is computed from the five subscription windows alone (see
	// strategy.HeadroomFor) and is never Known for a seat with none, which is
	// every enterprise/pay-as-you-go account KindCredit names — not just an
	// account that failed to poll. Reading the credit axis instead of printing
	// "?" for the whole class is safe because it is read-only display: it is
	// the SAME extra_usage the credit gate prices a switch against
	// (internal/strategy/credit.go), never a second source for the number.
	if label, ok := r.creditLeftLabel(); ok {
		return label
	}
	return unreadable
}

// creditLeftLabel is leftLabel's credit-axis fallback: the remaining amount
// and, when the account reports both figures, the used/limit pair beside it —
// the two things a LEFT column showing nothing but "?" was hiding entirely.
func (r statusRow) creditLeftLabel() (string, bool) {
	if !r.HasEntry {
		return "", false
	}
	e := r.Entry.Snapshot.ExtraUsage
	if !e.Present {
		return "", false
	}
	used, hasUsed := e.UsedCredits()
	limit, hasLimit := e.MonthlyLimit()
	switch {
	case hasUsed && hasLimit:
		return fmt.Sprintf("%s/%s used, %s left (%s)",
			e.AmountString(used), e.AmountString(limit), e.AmountString(limit-used), e.CurrencyCode()), true
	case hasUsed:
		// Limit is nil, which means the ACCOUNT sets no cap of its own (the
		// credit gate then falls back to the configured ceiling) — there is no
		// account-side number to show a remainder against, so this says used
		// and stops rather than inventing a denominator.
		return fmt.Sprintf("%s used, no account limit (%s)", e.AmountString(used), e.CurrencyCode()), true
	}
	// Neither money figure was on the wire; extra_usage.utilization was the
	// only readable axis. Still worth a real number over "?".
	if pct, ok := e.Percent(); ok {
		return fmt.Sprintf("%.0f%% left", 100-pct), true
	}
	return "", false
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

func statusPayload(report daemon.Report, probeErr error, rows []statusRow,
	active store.Account, hasActive bool, mode strategy.Mode, hasMode bool,
	now time.Time) map[string]any {
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
	// Conditional, like every other key here that stands for a reading: absent
	// means no ranking ran, and a consumer that saw "headroom" could not tell that
	// from an engine with room to spare. auto.go publishes the same key behind the
	// same guard.
	if hasMode {
		payload["mode"] = mode.String()
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
	if credit := creditJSON(r.Entry.Snapshot.ExtraUsage); credit != nil {
		out["credit"] = credit
	}
	return out
}

// creditJSON is the credit axis a reading carried, or nil when the reading
// had none — which is every account whose organization never turned overage
// on, credit-metered or not. monthlyLimit and usedCredits are each omitted
// rather than written as zero when the wire did not report them, for the same
// reason the credit gate itself fails closed on a nil Used: an unreported cap
// is not a cap of zero, and an unreadable spend is not a spend of zero.
func creditJSON(e usage.ExtraUsage) map[string]any {
	if !e.Present {
		return nil
	}
	out := map[string]any{"state": e.State.String()}
	if e.Currency != "" {
		out["currency"] = e.Currency
	}
	if limit, ok := e.MonthlyLimit(); ok {
		out["monthlyLimit"] = limit
	}
	if used, ok := e.UsedCredits(); ok {
		out["usedCredits"] = used
	}
	if pct, ok := e.Percent(); ok {
		out["utilizationPct"] = pct
	}
	if e.DisabledReason != "" {
		out["disabledReason"] = e.DisabledReason
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

package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/buildinfo"
	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/switcher"
	"github.com/Kweiza/ccdaddy/internal/usage"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// timeNow is the clock the dashboard reads. It is a package var so a test can
// fix it: every relative figure here — uptime, reset-in, the age of a reading —
// is a difference against it.
var timeNow = time.Now

// observeDaemon is the liveness probe, as a seam. A filesystem where locks do
// not work is not something a test can arrange, and it is exactly the case this
// command must not fall over on.
var observeDaemon = daemon.Observe

// humanDuration keeps its name inside package cli: see internal/view/lines.go,
// which now owns it, and status_test.go's TestHumanDurationReadsAtEveryScale,
// which still calls it directly by that name.
var humanDuration = view.HumanDuration

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

	snap, probeErr, err := loadSnapshot(cmd, now)
	if err != nil {
		return err
	}

	if asJSON {
		return writeJSON(cmd, statusPayload(snap, probeErr))
	}
	return renderStatus(cmd, snap)
}

// loadSnapshot is one complete read of the documents a dashboard renders,
// factored out of runStatus so that the terminal dashboard reads them in the
// same order, from the same sources, with the same notices. A second read
// order would be a second chance to derive a number differently, and the only
// thing that makes `ccdad status` and the terminal dashboard agree is that
// there is one of these.
//
// The notices go to cmd.ErrOrStderr() here, exactly as they did before. The
// Snapshot carries its own copy so a caller that has no stderr -- a full-screen
// program owns the terminal -- can render them itself.
//
// probeErr is returned alongside the snapshot rather than folded into it: it is
// observeDaemon's own error, a soft failure that still renders a dashboard, and
// daemonJSON needs the error VALUE rather than a string already spent on a
// human sentence. err is the one failure loadSnapshot cannot render past — an
// unreadable account store or usage cache — and it is what runStatus returns
// directly.
func loadSnapshot(cmd *cobra.Command, now time.Time) (snap view.Snapshot, probeErr, err error) {
	s, err := store.Open()
	if err != nil {
		return view.Snapshot{}, nil, err
	}
	accounts := s.Accounts()

	// Tolerated, like `list` does: an unreadable live file costs the active
	// marker and nothing else, and status is what a user reaches for when
	// something is already wrong.
	live, _ := cclink.Load()
	active, hasActive := attributeLive(live, accounts, s.Credentials)

	report, probeErr := observeDaemon()
	var notices []string
	if probeErr != nil {
		// A human notice, so a --json caller still receives exactly one document
		// on stdout.
		n := fmt.Sprintf("Cannot tell whether a daemon is running: %v\n", probeErr)
		fmt.Fprint(cmd.ErrOrStderr(), n)
		notices = append(notices, n)
	}
	if report.StatusErr != nil {
		n := fmt.Sprintf("The daemon's status file could not be read: %v\n", report.StatusErr)
		fmt.Fprint(cmd.ErrOrStderr(), n)
		notices = append(notices, n)
	}

	// The cache is the authority for quota, for both this command and `list`.
	// That `ccdad list` and `ccdad status --json` can never disagree is only
	// true because neither of them has a second source for a number — see
	// daemon.Status's authority note.
	cache, err := usage.LoadCache()
	if err != nil {
		return view.Snapshot{}, probeErr, err
	}
	if cerr := cache.LoadError(); cerr != nil {
		n := fmt.Sprintf("The usage cache could not be read: %v\n", cerr)
		fmt.Fprint(cmd.ErrOrStderr(), n)
		notices = append(notices, n)
	}

	// The measurement is taken HERE, in the one place that already holds both
	// documents it needs, so `ccdad status`, `ccdad list` and the terminal
	// dashboard cannot measure one fleet three ways. It is the same call
	// `ccdad runway` makes, on the same two sources: the cache supplies every
	// level and the series supplies nothing but slopes.
	//
	// A series that cannot be read costs the rates and nothing else. Every row
	// below still renders, because a dashboard is what a user reaches for when
	// something is already wrong.
	fleet, historyNotice := fleetForecast(accounts, cache, now)
	if historyNotice != "" {
		fmt.Fprint(cmd.ErrOrStderr(), historyNotice)
		notices = append(notices, historyNotice)
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
	cfg, configNotice := configOrDefaults()
	if configNotice != "" {
		fmt.Fprint(cmd.ErrOrStderr(), configNotice)
		notices = append(notices, configNotice)
	}
	plan, decided, planErr := enginePlan(s, now)
	if planErr != nil {
		// Said out loud: the Mode line simply disappears otherwise, which reads
		// as "the engine has nothing to say" rather than "it could not be asked".
		n := fmt.Sprintf("The engine could not be asked which mode it is ranking in: %v\n", planErr)
		fmt.Fprint(cmd.ErrOrStderr(), n)
		notices = append(notices, n)
	}
	var mode strategy.Mode
	hasMode := false
	if decided {
		mode, hasMode = plan.Result.Mode, true
	}

	// Bypasses thresholdsFrom -- which only prints -- so the hover notice it
	// would otherwise swallow reaches Notices too, in the same position it
	// already printed in: after the plan notice, exactly where thresholdsFrom
	// evaluated as `view.Rows`'s argument would have run it.
	resolve, hoverNotices := view.ThresholdsFor(cfg, now, plan, planErr)
	for _, n := range hoverNotices {
		fmt.Fprint(cmd.ErrOrStderr(), n)
		notices = append(notices, n)
	}

	rows := view.Rows(accounts, cache, active, hasActive, now, resolve)
	for i := range rows {
		rows[i].Engine = engine[rows[i].Account.UUID]
	}

	activeLabel := "none of the managed accounts"
	for _, r := range rows {
		if r.Active {
			activeLabel = r.Account.Label()
		}
	}

	return view.Snapshot{
		Now:         now,
		Rows:        rows,
		Report:      report,
		ActiveLabel: activeLabel,
		Strategy:    cfg.Strategy.String(),
		Hover:       cfg.Hover,
		Mode:        mode,
		HasMode:     hasMode,
		Version:     buildinfo.Version,
		Notices:     notices,
		Forecast:    fleet,
		// Basis.Known is the whole test, and it is the same one view.RunwayLine
		// applies before it returns anything: a fleet nobody has enough
		// readings for has levels but no rate, and reporting that as a forecast
		// would publish an object whose every verdict is "unknown" beside a
		// burn of zero. It is also what keeps the dashboard's byte-compared
		// golden fixtures still on a machine that has recorded nothing.
		HasForecast: fleet.Basis.Known,
	}, probeErr, nil
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

// rowThresholds is what `list` measures its rows against: one bundle per
// account, because under hover there is no single one.
//
// It is `list`'s alone. `status` reads the same answer out of view.ThresholdsFor
// directly, because it has already asked the engine for the mode and must not
// ask a second time. cfg is a parameter rather than a read for the same reason:
// its caller has the config in hand to decide whether to say the numbers were
// derived, and a second rowConfig call would print the config notice twice.
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
func rowThresholds(cmd *cobra.Command, cfg config.Config, s *store.Store, now time.Time) func(uuid string) strategy.Thresholds {
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
	cfg, notice := configOrDefaults()
	if notice != "" {
		fmt.Fprint(cmd.ErrOrStderr(), notice)
	}
	return cfg
}

// configOrDefaults is rowConfig without the print, so that a caller building a
// Snapshot -- which has no stderr of its own to assume -- can carry the same
// sentence rather than lose it. rowConfig and loadSnapshot both go through
// this, so the config notice is spelled once regardless of which one asks.
func configOrDefaults() (cfg config.Config, notice string) {
	cfg, err := config.Load()
	if err != nil {
		return config.Defaults(), fmt.Sprintf("note: %v; the rows are measured against the built-in thresholds\n", err)
	}
	return cfg, ""
}

// thresholdsFrom is rowThresholds without the fetch, so a caller that has
// already asked the engine does not ask it twice. The plan is read only when
// hover is on; with hover off it is ignored entirely, including its error.
func thresholdsFrom(cmd *cobra.Command, cfg config.Config, now time.Time,
	plan strategy.Plan, planErr error) func(uuid string) strategy.Thresholds {

	resolve, notices := view.ThresholdsFor(cfg, now, plan, planErr)
	for _, n := range notices {
		fmt.Fprint(cmd.ErrOrStderr(), n)
	}
	return resolve
}

// renderStatus takes the whole Snapshot rather than a field per column. The
// argument list was already seven long and every entry on it was one of
// loadSnapshot's own: passing the document is what stops the next field it
// gains from being an eighth positional bool.
func renderStatus(cmd *cobra.Command, snap view.Snapshot) error {
	out, pal := renderTarget(cmd)
	rows, now := snap.Rows, snap.Now

	// Every line of this block goes through view.WrapLabeled at the width of
	// the terminal, and the width is zero for everything that is not one.
	// Measured on an 80-column terminal: Mode: 124 display columns, Hover: 100,
	// and the terminal folded both wherever its own right edge landed. The
	// runway line below has its own wrap, because its spaces are inside its
	// values and these are between words.
	//
	// THE WIDTH IS MEASURED ON cmd.OutOrStdout() AND NEVER ON out, at all five
	// sites below, and the distinction is the whole reason this paragraph
	// exists. out is renderTarget's writer -- the same destination wearing a
	// palette -- and outWidth answers by asserting *os.File. A wrapper fails
	// that assertion, so outWidth(out) is 0 on every terminal there is and the
	// fold just stops, on a change that never mentioned folding. That is the
	// worst shape a regression comes in: git merges the palette and the fold
	// without a word between them, every test in this package stubs the
	// outWidth seam and stays green, and the only reader who can see it is a
	// person at an 80-column window. TestWidthIsMeasuredOnTheFileAndNotThe
	// DecoratedWriter is what makes the next such merge red instead.
	//
	// The rejected alternative was giving colorWriter's type an
	// Unwrap() io.Writer and teaching outWidth to follow the chain. It is a
	// general mechanism -- every future decorator has to remember to implement
	// it, and one that genuinely does narrow the usable width would have the
	// fold silently re-enabled at the wrong number -- introduced for two call
	// sites in one package. A palette does not change how wide the terminal is;
	// the width comes from the thing that has one.
	fmt.Fprintln(out, view.WrapLabeled(view.DaemonLine(snap.Report, now), outWidth(cmd.OutOrStdout())))

	if len(rows) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "No accounts yet. Run 'ccdad add' to log one in.")
		return nil
	}

	// ActiveLabel is loadSnapshot's, not recomputed here: two loops over rows
	// producing the same "which account is active" sentence is the exact "one
	// value, two spellings" failure this task exists to remove.
	fmt.Fprintln(out, view.WrapLabeled("Active:  "+snap.ActiveLabel, outWidth(cmd.OutOrStdout())))
	// Above the Mode line, because it is what EXPLAINS it: under hover the mode
	// is headroom whatever the file says, hover having overridden the strategy
	// key. A reader who set consume-first and finds headroom here needs the
	// reason before the answer, not after it.
	if snap.Hover {
		fmt.Fprintln(out, view.WrapLabeled(view.HoverLine(), outWidth(cmd.OutOrStdout())))
	}
	if snap.HasMode {
		fmt.Fprintln(out, view.WrapLabeled(view.ModeLine(snap.Mode), outWidth(cmd.OutOrStdout())))
	}
	// The empty string is the gate: view.RunwayLine returns one when there is
	// no measurement, and that is how this renderer, `ccdad list` and the
	// dashboard all decline the line without each carrying its own idea of when
	// there is nothing to say.
	//
	// It belongs with the labels above and not with the table: the
	// nine-character label lines it up with Daemon:, Active: and Mode:, and the
	// blank line below is the separator between that block and the rows.
	//
	// The order this position produces is now the order it looks like.
	// columns() writes when it is called, so a line handed to out before it
	// comes out before it, and one handed to out after it comes out after. The
	// tabwriter this replaced buffered every row until Flush, so the same
	// ordering held for a different reason and the source order was free.
	// What position in this function still decides is which side of the blank
	// separator this lands on, which is the whole of it.
	//
	// time.Local, because a person is reading it. internal/forecast touches no
	// environment, so the caller nearest the reader chooses the zone, and
	// view.Timestamp always prints which one it was.
	//
	// The width is the terminal's -- read off cmd.OutOrStdout() for the reason
	// the block comment above gives -- and it is zero for everything that is
	// not a terminal. A line this long folds where the terminal decides
	// otherwise, and its clauses are separated by a middot, so a fold that
	// lands between two of them cannot be told from one that lands inside a
	// date.
	if line := view.RunwayLine(snap.Forecast, now, time.Local); line != "" {
		fmt.Fprintln(out, view.RunwayWrap("Runway:  ", line, outWidth(cmd.OutOrStdout())))
	}
	fmt.Fprintln(out)

	cells := make([][]string, 0, len(rows))
	for _, r := range rows {
		// StatusFlags rides on the AGE cell, which is what the trailing %s%s in
		// the format string this replaced was doing, and for the same reason
		// the flags ride on `list`'s last cell: a suffix that belongs to one
		// account reads better beside that account's own figure than at a
		// fixed offset far to its right.
		cells = append(cells, []string{
			fmt.Sprintf("%s %d", r.Marker(), r.Account.Idx), r.StatusLabel(), r.Account.Kind.String(),
			r.UsedLabel(), r.WindowLabel(), r.ResetsLabel(now), r.PaceLabel(),
			r.AgeLabel(now) + r.StatusFlags(),
		})
	}
	return columns(out, []string{"  IDX", "ACCOUNT", "TYPE", "USED", "WINDOW", "RESETS IN", "PACE", "AGE"},
		cells, quotaCellStyle(pal, rows, 3, view.Row.UsedLabel))
}

func statusPayload(snap view.Snapshot, probeErr error) map[string]any {
	// The daemon half is daemonJSON's, and `ccdad daemon status --json` nests
	// the same object under the same key: two commands describing one daemon
	// must not describe it two ways.
	d := daemonJSON(snap.Report, probeErr)

	out := []map[string]any{}
	activeUUID, hasActive := "", false
	for _, r := range snap.Rows {
		row := accountJSON(r.Account)
		row["active"] = r.Active
		if r.Active {
			activeUUID, hasActive = r.Account.UUID, true
		}
		if u := usageJSON(r, snap.Now); u != nil {
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
		payload["activeUuid"] = activeUUID
	}
	// Conditional, like every other key here that stands for a reading: absent
	// means no ranking ran, and a consumer that saw "headroom" could not tell that
	// from an engine with room to spare. auto.go publishes the same key behind the
	// same guard.
	if snap.HasMode {
		payload["mode"] = snap.Mode.String()
	}
	// Conditional for the reason unnamableWeeklyCaps is: an ordinary payload does
	// not carry a field that is always the boring default. Present means every
	// windowThreshold on the wire was DERIVED rather than read out of the file,
	// which is the one thing about these rows a script cannot infer from them.
	if snap.Hover {
		payload["hover"] = true
	}
	// Conditional for the same reason, and it is forecastJSON's object rather
	// than a second spelling of it: `ccdad runway --json` publishes the
	// identical document under the identical key, and two commands describing
	// one measurement must not describe it two ways. Absent means nothing was
	// measured; an object of zeros would read as a fleet burning nothing.
	if snap.HasForecast {
		payload["forecast"] = forecastJSON(snap.Forecast)
	}
	return payload
}

// usageJSON is the quota half of a row, or nil when there is no reading.
//
// nil rather than an object of zeros, for the same reason the table prints "?":
// an account that could not be read is not an empty one, and a consumer that
// sees no `usage` key cannot mistake it for one at 0%.
func usageJSON(r view.Row, now time.Time) map[string]any {
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
		out["bindingWindow"] = string(r.ReportedName())
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

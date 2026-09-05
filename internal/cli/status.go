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

// noActiveAccountLabel is spelled once and read by every surface that must say
// Claude Code is not attributed to a managed account -- `status`, the
// dashboard and `which`'s unattributed path -- so a machine with no Claude
// login cannot describe itself two ways depending which command asked.
const noActiveAccountLabel = "none of the managed accounts"

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
	var (
		asJSON  bool
		refresh bool
	)

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the engine dashboard",
		Long: "status is the one overview for the accounts, the selected strategy, cached\n" +
			"readings and the daemon's own published state. It never fetches — the\n" +
			"usage endpoint allows roughly 28-30 requests per identity per rolling\n" +
			"hour on a sliding window. --refresh asks once, while still respecting the\n" +
			"cache and rate-limit backoff.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd, asJSON, refresh)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a machine-readable object on stdout")
	cmd.Flags().BoolVar(&refresh, "refresh", false, "refresh usage before rendering when the cache permits it")
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
func runStatus(cmd *cobra.Command, asJSON, refresh bool) error {
	now := timeNow()

	snap, probeErr, err := loadSnapshot(cmd, now, refresh)
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
func loadSnapshot(cmd *cobra.Command, now time.Time, refresh bool) (snap view.Snapshot, probeErr, err error) {
	s, err := store.Open()
	if err != nil {
		return view.Snapshot{}, nil, err
	}
	accounts := s.Accounts()

	// An unreadable live file costs the active marker and nothing else. Status
	// is what a user reaches for when something is already wrong.
	live, _ := cclink.Load()
	active, hasActive := attributeLive(live, accounts, s.Credentials)
	if refresh {
		pollable := make([]store.Account, 0, len(accounts))
		for _, account := range accounts {
			if !account.Disabled {
				pollable = append(pollable, account)
			}
		}
		refreshUsage(cmd, s, pollable, active, hasActive, now)
	}

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

	// The cache is the authority for quota. Status has no second source for a
	// number — see daemon.Status's authority note.
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
	// documents it needs, so status and the terminal dashboard cannot measure
	// one fleet two ways. It is the same call
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

	// Threshold resolution returns its notices so the full-screen dashboard can
	// carry them without owning a cobra command or a second policy path.
	resolve, hoverNotices := view.ThresholdsFor(cfg, now, plan, planErr)
	for _, n := range hoverNotices {
		fmt.Fprint(cmd.ErrOrStderr(), n)
		notices = append(notices, n)
	}

	rows := view.Rows(accounts, cache, active, hasActive, now, resolve)
	for i := range rows {
		rows[i].Engine = engine[rows[i].Account.UUID]
	}

	activeLabel := noActiveAccountLabel
	for _, r := range rows {
		if r.Active {
			activeLabel = r.Account.Label()
		}
	}
	// The pointer, resolved through the one reader every surface uses. It is
	// read HERE, in the function that already holds the account list, so that
	// status and the dashboard cannot each answer it.
	codexLabel, codexUUID := "", ""
	if serving, ok := codexServingAccount(accounts); ok {
		codexLabel, codexUUID = serving.Label(), serving.UUID
	}

	return view.Snapshot{
		Now:               now,
		Rows:              rows,
		Report:            report,
		ActiveLabel:       activeLabel,
		CodexServingLabel: codexLabel,
		CodexServingUUID:  codexUUID,
		Strategy:          selectedStrategy(cfg),
		Hover:             cfg.Hover,
		HoverAccounts:     hoverAccounts(cfg, plan),
		Manual:            cfg.Manual,
		Mode:              mode,
		HasMode:           hasMode,
		Version:           buildinfo.Version,
		Notices:           notices,
		UnknownKeys:       cclink.UnknownKeys(live),
		Forecast:          fleet,
		// Basis.Known is the whole test, and it is the same one view.RunwayLine
		// applies before it returns anything: a fleet nobody has enough
		// readings for has levels but no rate, and reporting that as a forecast
		// would publish an object whose every verdict is "unknown" beside a
		// burn of zero. It is also what keeps the dashboard's byte-compared
		// golden fixtures still on a machine that has recorded nothing.
		HasForecast: fleet.Basis.Known || fleet.Credit.Known,
	}, probeErr, nil
}

// hoverAccounts is the per-account half of hover's derivation, or nothing.
//
// It is guarded on cfg.Hover as well as on the pass, and the redundancy is the
// point: view.ThresholdsFor already refuses to read a plan with hover off, and a
// snapshot that carried the shares anyway would let a renderer print a licence
// nothing was measured against.
func hoverAccounts(cfg config.Config, plan strategy.Plan) []strategy.HoverAccount {
	if !cfg.Hover || plan.Hover == nil {
		return nil
	}
	return plan.Hover.Accounts
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

// configOrDefaults returns the fallback notice instead of printing it, so a
// full-screen dashboard can carry the same message without owning stderr.
func configOrDefaults() (cfg config.Config, notice string) {
	cfg, err := config.Load()
	if err != nil {
		return config.Defaults(), fmt.Sprintf("note: %v; the rows are measured against the built-in thresholds\n", err)
	}
	return cfg, ""
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
	// Measured on an 80-column terminal, Current's recovery explanation exceeds
	// the width and the terminal otherwise folds it wherever its edge lands. The
	// runway line below has its own wrap, because its spaces are inside its
	// values and these are between words.
	//
	// THE WIDTH IS MEASURED ON cmd.OutOrStdout() AND NEVER ON out, at all eight
	// sites below, and the distinction is the whole reason this paragraph
	// exists. out is renderTarget's writer -- the same destination wearing a
	// palette -- and outWidth answers by asserting *os.File. A wrapper fails
	// that assertion, so outWidth(out) is 0 on every terminal there is and the
	// fold just stops, on a change that never mentioned folding. That is the
	// worst shape a regression comes in: git merges the palette and the fold
	// without a word between them, every test that stubs the outWidth seam
	// stays green, and the only reader who can see it is a person at an
	// 80-column window.
	//
	// Two tests hold this now, and they hold different halves of it.
	// TestTheFoldMeasuresTheFileAndNotTheWriterItPaintsThrough runs both
	// commands and asserts the writer that arrives to be measured IS the file
	// the root was given -- the stronger question, at the sites its fixture
	// executes. It does not execute all of them: Hover: and Update: are printed
	// only under a state it does not set up, and outWidth(out) at either was
	// green across this whole package. Manual: is a third line in that
	// position -- printed only when the mode is on, which no fixture there
	// sets up either. The other half is held by
	// TestEveryLineOfTheStatusBlockFoldsAtTheFilesWidth, which reads this
	// function's source rather than its output, so every site is covered
	// whatever a fixture happens to render and the count of eight below is
	// asserted there rather than only written here.
	//
	// Both names are written on one line each, deliberately. The name this
	// paragraph carried before was split across a line break and named nothing
	// that existed -- the test had been renamed, and no grep for the name in
	// the comment could find that out.
	//
	// The rejected alternative was giving colorWriter's type an
	// Unwrap() io.Writer and teaching outWidth to follow the chain. It is a
	// general mechanism -- every future decorator has to remember to implement
	// it, and one that genuinely does narrow the usable width would have the
	// fold silently re-enabled at the wrong number -- introduced for two call
	// sites in one package. A palette does not change how wide the terminal is;
	// the width comes from the thing that has one.
	fmt.Fprintln(out, view.WrapLabeled(view.DaemonLine(snap.Report, now), outWidth(cmd.OutOrStdout())))

	// Under the daemon line and ABOVE the no-accounts return, because a machine
	// with no accounts yet is still a machine that can be out of date -- and
	// because this is a fact about the daemon, which is what the line above it
	// describes. It is silent unless there is something to say, and it wraps the
	// way its five siblings do: the sentence is 95 columns on a release whose
	// versions are three digits each, which an 80-column terminal folds wherever
	// its own right edge lands.
	if line, ok := view.UpdateLine(snap.Report, snap.Version); ok {
		fmt.Fprintln(out, view.WrapLabeled(line, outWidth(cmd.OutOrStdout())))
	}

	// The proxy came up on a port other than the one it resolved, so every
	// codex session launched before this daemon is talking to nothing. Its own
	// symptom is codex's endless reconnect, which nothing else on the machine
	// would explain -- which is the whole reason this line exists.
	//
	// The state clause excludes DaemonStopped and NOTHING else, and its two
	// halves are separate decisions.
	//
	// It is there at all because a stopped daemon's status document stays
	// readable -- a pairing this package exercises deliberately elsewhere --
	// and the fallback flag inside it is a fact about a process that is gone.
	// Without the clause the dashboard would print `Daemon:  not running` and,
	// on the very next line, name a loopback port nothing is bound to and ask
	// for a relaunch, when the fix is to start a daemon. `ccdad doctor` refuses
	// that same reading: its codex check answers "no daemon is running" before
	// it looks at the flag at all, and two surfaces disagreeing about one
	// machine is worse than either answer alone.
	//
	// It is `!= DaemonStopped` and not `== DaemonRunning` because DaemonUnknown
	// means the lock could not be probed, and "cannot tell" is never folded
	// into "no" here: on a filesystem where locks do not work the daemon may
	// well be running with a proxy that fell back, and that is precisely the
	// reader this sentence is written for.
	if r := snap.Report; r.State != daemon.DaemonStopped && r.HasStatus && r.Status.CodexProxyFellBack {
		fmt.Fprintln(out, view.WrapLabeled(fmt.Sprintf("Codex:   the proxy is on 127.0.0.1:%d, not the port it resolved; codex sessions launched before this daemon must be relaunched", r.Status.CodexProxyPort), outWidth(cmd.OutOrStdout())))
	}

	// BOTH providers, and this is one of the few sentences where that is not a
	// preference. There is nothing in the store to infer a provider from, so
	// naming one would be a guess printed as advice.
	if len(rows) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "No accounts yet. Run 'ccdad add claude' or 'ccdad add codex' to log one in.")
		return nil
	}

	// The summary block is internal/view's, line for line, which is what makes
	// this table and the dashboard's header ONE block rather than two that
	// happen to agree. Every fact owns a row, and Active owns one PER PROVIDER:
	// the single `Claude: x · Codex: y` sentence this replaced put two accounts
	// on one line, where a long label is cut and takes the provider beside it
	// with it.
	//
	// The values come off the Snapshot rather than being recomputed here. Two
	// loops over rows producing the same "which account is active" sentence is
	// the exact "one value, two spellings" failure package view exists to
	// remove.
	//
	// The pair is joined back into a line before it is folded, because the fold
	// finds its own label: WrapLabeled hangs continuation lines under the value,
	// and it locates the label by looking for the colon and the padding after
	// it. Handing it Label and Value separately would be a second answer to a
	// question splitLabel already answers.
	for _, line := range snap.SummaryLines() {
		fmt.Fprintln(out, view.WrapLabeled(line.Label+line.Value, outWidth(cmd.OutOrStdout())))
	}
	// The empty string is the gate: view.RunwayLine returns one when there is
	// no measurement, and that is how this renderer and the dashboard both
	// decline the line without each carrying its own idea of when
	// there is nothing to say.
	//
	// It belongs with the labels above and not with the table: the
	// label keeps it in the same summary block as Daemon:, Active: and Current:, and the
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

	// The same constructor the dashboard calls, which is what makes both
	// surfaces name the same windows in the same order under the same headers.
	block := view.ColumnsOf(rows)
	cols := statusColumns(block)

	head := make([]string, 0, len(cols))
	// firstWindow is where the quota block starts and accountCol is where the
	// section headings' text goes, both asked for rather than written down: the
	// style function indexes block.Windows off the first and paints the second,
	// and a constant here would be a second statement of which column is which
	// -- true today and silently wrong the moment the list above changes.
	firstWindow, accountCol := len(cols), -1
	for i, c := range cols {
		if c.Kind == view.ColumnWindow && i < firstWindow {
			firstWindow = i
		}
		if c.Kind == view.ColumnAccount {
			accountCol = i
		}
		head = append(head, statusHeader(c))
	}

	// The lines this table draws: each provider's heading and that provider's
	// accounts under it, in internal/view's own grouping. The style function
	// below is handed the same list, so the integer it is asked about and the
	// integer that produced the cells are one integer rather than two slices
	// that happen to agree -- which is what the grouping makes load-bearing,
	// since it puts lines in the table that no account list has.
	display := view.ListRows(view.Sections(rows))

	cells := make([][]string, 0, len(display))
	for _, line := range display {
		// A heading is a TABLE ROW carrying its text in the ACCOUNT cell and
		// nothing anywhere else. A line printed above the table could not know
		// the column widths this table is about to measure, and one table per
		// section would size its columns independently -- so the two halves of
		// one fleet would come out under headings that do not line up.
		if line.Header != "" {
			row := make([]string, len(cols))
			for i, c := range cols {
				if c.Kind == view.ColumnAccount {
					row[i] = line.Header
				}
			}
			cells = append(cells, row)
			continue
		}
		r := line.Row
		row := make([]string, 0, len(cols))
		for _, c := range cols {
			cell := r.ListCell(c, block, now, snap.Hover)
			// StatusFlags rides on the AGE cell, which is what the trailing
			// %s%s in the format string this replaced was doing, and for the
			// same reason the flags ride on the last cell: a suffix that
			// belongs to one account reads better beside that account's own
			// figure than at a fixed offset far to its right.
			if c.Kind == view.ColumnAge {
				cell += r.StatusFlags()
			}
			row = append(row, cell)
		}
		cells = append(cells, row)
	}
	if err := columns(out, head, cells, windowCellStyle(pal, display, accountCol, firstWindow, block)); err != nil {
		return err
	}
	// Under the table, because each of these explains a column the reader is
	// already looking at, and in internal/view's order rather than in one
	// spelled out here: which sentence follows which is a fact about the TABLE,
	// so the surface that draws the table does not get its own answer. The
	// stranded sentence is handed in because it is a fact about the RANKING
	// rather than about these rows -- nothing on the table can explain why two
	// accounts at the same point of the same window carry different
	// thresholds, so this is the only place the reader can be told. PACE left
	// this table with the derived window it was read off -- `ccdad runway` is
	// the human answer to "how fast", and `--json` still carries every window's
	// pace including the projection.
	for _, line := range view.TrailerLines(rows, block, snap.Hover, snap.StrandedNote()) {
		fmt.Fprintln(out, view.WrapLabeled(line, outWidth(cmd.OutOrStdout())))
	}
	return nil
}

// statusColumns is the account table `ccdad status` prints: the shared column
// list, less the one column this surface does not draw.
//
// It reads view.ListColumns rather than restating an order, which is the whole
// point of there being a list: the columns below, and the order they come in,
// are one definition that the terminal dashboard reads too, so neither surface
// can grow a column the other has never heard of.
//
// STATE is now here, and AUTO is deliberately not. The two look like one
// decision and are not.
//
// STATE is what the engine last decided about an account, and this command
// already has the fact and already publishes it -- `--json` carries it under
// the engine key. The human table was the only surface hiding it, which left a
// reader running `ccdad status` unable to see the difference between an account
// the engine has quarantined and one it merely has not chosen, on the one
// surface people actually read.
//
// AUTO is the rotation policy, and this table already spells it: an account
// held out of rotation prints `(disabled)` in the flags that ride on the AGE
// cell. A yes/no column beside that would state the same fact twice on one row,
// and a reader would reasonably assume two columns saying the same thing must
// mean two different things.
func statusColumns(block view.Columns) []view.ListColumn {
	full := view.ListColumns(block)
	out := make([]view.ListColumn, 0, len(full))
	for _, c := range full {
		if c.Kind == view.ColumnAuto {
			continue
		}
		out = append(out, c)
	}
	return out
}

// statusHeader is one heading over that table: the column's own, and the IDX
// one shifted right past the marker in front of it.
//
// The two spaces are an ALIGNMENT and not a second name for the column. The
// IDX cell is Row.Marker and the index together -- "* 3" on the live account
// and "  3" on every other -- so a heading printed flush left would stand over
// the marker rather than over the number it names. Every one-shot table in
// this package has spelled it this way since the first one, and the shift is
// the surface's because the marker is: what shares that cell is a fact about
// the report being drawn.
func statusHeader(c view.ListColumn) string {
	if c.Kind == view.ColumnIdx {
		return "  " + c.Header
	}
	return c.Header
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
		if u := usageJSON(r, snap.Now, snap.Hover); u != nil {
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
		"strategy":      snap.StrategyLabel(),
	}
	if hasActive {
		payload["activeUuid"] = activeUUID
	}
	// A separate key from activeUuid because it describes the Codex lane's
	// pointer, not Claude Code's live credential.
	if snap.CodexServingUUID != "" {
		payload["codexServingUuid"] = snap.CodexServingUUID
	}
	// Conditional, like every other key here that stands for a reading: absent
	// means no ranking ran, and a consumer that saw "headroom" could not tell that
	// from an engine with room to spare. auto.go publishes the same key behind the
	// same guard.
	if snap.HasMode {
		payload["mode"] = snap.Mode.String()
	}
	if len(snap.UnknownKeys) > 0 {
		payload["unknownKeys"] = snap.UnknownKeys
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
func usageJSON(r view.Row, now time.Time, includeThresholds bool) map[string]any {
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
	for _, w := range r.CarriedWindows() {
		entry := map[string]any{}
		if pct, ok := w.Percent(); ok {
			entry["utilizationPct"] = pct
		}
		if reset, ok := w.Reset(); ok {
			entry["resetsAt"] = reset
		}
		if includeThresholds {
			threshold := r.WindowThreshold(w.Name)
			entry["thresholdPct"] = threshold
			if pct, ok := w.Percent(); ok {
				entry["slackPct"] = threshold - pct
			}
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

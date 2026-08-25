package cli

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/forecast"
	"github.com/Kweiza/ccdaddy/internal/history"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/usage"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// newRunwayCmd builds `ccdad runway`: how fast the fleet is spending its quota,
// and when that runs out.
//
// It is NOT on autoStartCommands, and the omission is deliberate rather than
// forgotten. runway reads what is already on disk, exactly as `ccdad hover
// status` and `ccdad doctor` do, and neither of those starts a daemon either. A
// command that spawned a poller to answer a question about the past would take
// a request against a rate-limited endpoint to tell the user something the
// request cannot change.
func newRunwayCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "runway",
		Short: "Measure how fast the fleet is spending its quota, and say when it runs out",
		Long: "runway reads the readings the daemon has already taken and measures the\n" +
			"fleet's burn rate over the last few hours. It never fetches: nothing here\n" +
			"costs a request against the usage endpoint.\n\n" +
			"The two window rows are verdicts — whether resets give quota back faster\n" +
			"than the fleet spends it. The credit row is a balance divided by a rate,\n" +
			"with nothing coming back.\n\n" +
			"A rate is per axis and the axes are never summed: one percentage point of\n" +
			"a five-hour window and one of a weekly window are different quantities.\n\n" +
			"Nothing is projected from a single reading. A machine that has been\n" +
			"recording for ten minutes is told so rather than given a number.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			now := timeNow()
			errw := cmd.ErrOrStderr()

			s, err := store.Open()
			if err != nil {
				return err
			}
			accounts := s.Accounts()

			// The cache is the authority for every LEVEL on this page, as it is
			// for `ccdad status` and `ccdad list`. An unreadable one is a notice
			// and not a refusal, for the same reason it is there: this is a
			// command a user reaches for when something is already wrong.
			cache, err := usage.LoadCache()
			if err != nil {
				return err
			}
			if cerr := cache.LoadError(); cerr != nil {
				fmt.Fprintf(errw, "The usage cache could not be read: %v\n", cerr)
			}

			f, notice := fleetForecast(accounts, cache, now)
			if notice != "" {
				fmt.Fprint(errw, notice)
			}
			if f.TierNotice != "" {
				fmt.Fprintf(errw, "note: %s\n", f.TierNotice)
			}
			switch {
			case len(accounts) == 0:
				fmt.Fprintln(errw, "No accounts yet. Run 'ccdad add' to log one in.")
			case !f.Basis.Known:
				// Said in front of a person on both paths, because the payload's
				// silence on this is easy to misread as a fleet that burns
				// nothing.
				fmt.Fprintln(errw, "Not enough history yet to measure a burn rate. The daemon appends a "+
					"reading on every poll,\nand a rate needs several of them spread over time. "+
					"Nothing is projected from one reading.")
			}

			if asJSON {
				return writeJSON(cmd, map[string]any{"schemaVersion": 1, "forecast": forecastJSON(f)})
			}
			return renderRunway(cmd, f, accounts, now)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a machine-readable object on stdout")
	return cmd
}

// fleetForecast measures the fleet from the two documents that carry it, and
// returns the one notice a series that could not be read earns.
//
// The split between them is the rule that keeps two commands from disagreeing
// about one number: the CACHE supplies every level — a percentage left, a
// credit balance, a window's reset — and the SERIES supplies nothing but
// slopes. history.json's newest sample merely duplicates the cache's current
// reading, and reading a level from it would give the store two authorities for
// one figure.
//
// A series that cannot be read leaves every account without a measured rate,
// which is exactly the answer a cold machine gets: no basis, no verdict, and a
// notice saying so. It is not a reason to refuse the whole command, because the
// levels are still readable and they are half of what a reader came for.
func fleetForecast(accounts []store.Account, cache *usage.Cache, now time.Time) (forecast.Fleet, string) {
	var (
		h      *history.History
		notice string
	)
	h, err := history.LoadHistory()
	switch {
	case err != nil:
		notice = fmt.Sprintf("The usage history could not be read: %v\nNo rate can be measured from it.\n", err)
		h = nil
	case h.LoadError() != nil:
		notice = fmt.Sprintf("The usage history could not be parsed: %v\nNo rate can be measured from it.\n", h.LoadError())
	}

	in := make([]forecast.Input, 0, len(accounts))
	for _, a := range accounts {
		// RateLimitTier, not Tier. Tier is organization_type, which is the same
		// string for two seats on plans of very different sizes, and the
		// question here is whether these accounts' percentage points are the
		// same size as each other.
		input := forecast.Input{UUID: a.UUID, Idx: a.Idx, Tier: a.RateLimitTier}
		if e, ok := cache.Get(a.UUID); ok {
			input.Snapshot = e.Snapshot
		}
		if h != nil {
			// AddedAt is passed rather than dropped: a sample older than it
			// belonged to a PREVIOUS account at the same uuid — removed, then
			// added again — and letting one through would hand a fresh login
			// the slope its predecessor's spending earned.
			input.Series = h.Series(a.UUID, a.AddedAt)
		}
		in = append(in, input)
	}
	return forecast.Of(in, now), notice
}

// renderRunway draws the human answer: the basis it was measured from, the
// fleet's remaining points, one row per axis and one row per account.
//
// The two blocks are separate tabwriters on purpose. They describe different
// things and share no column, so aligning them together would pad the axis rows
// out to the width of an email address.
func renderRunway(cmd *cobra.Command, f forecast.Fleet, accounts []store.Account, now time.Time) error {
	out := cmd.OutOrStdout()
	if len(accounts) == 0 {
		// Nothing on stdout. The advice is already on stderr, and an empty
		// store is a fact rather than a failure.
		return nil
	}

	fmt.Fprintln(out, runwayBasisLine(f))
	// The points are read off the CURRENT snapshots, so they are known on a
	// machine that has no series at all. Printing them under a basis that says
	// nothing was measured is the honest pairing: this much quota exists, and
	// how fast it is going is not yet known.
	if f.PointsTotal > 0 {
		fmt.Fprintf(out, "Fleet:   %.0f of %.0f points left on the weekly axis\n", f.PointsLeft, f.PointsTotal)
	}
	if !f.Basis.Known {
		return nil
	}

	// time.Local, because a person is reading it. internal/forecast cannot
	// supply a zone — it reads no environment — so the caller nearest the
	// reader chooses one, and view.Timestamp always prints which one it was.
	loc := time.Local

	fmt.Fprintln(out)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  AXIS\tBURN\tREPLENISHES\tVERDICT")
	fmt.Fprintf(w, "  5-hour\t%s\t%s\t%s\n",
		view.RunwayBurn(f.FiveHour.Burn), view.RunwayReplenish(f.FiveHour.Replenish),
		view.RunwayVerdict(f.FiveHour, now, loc))
	fmt.Fprintf(w, "  7-day\t%s\t%s\t%s\n",
		view.RunwayBurn(f.Weekly.Burn), view.RunwayReplenish(f.Weekly.Replenish),
		view.RunwayVerdict(f.Weekly, now, loc))
	fmt.Fprintf(w, "  Credits\t%s\t%s\t%s\n",
		view.RunwayCreditBurn(f.Credit), view.RunwayCreditReplenish(),
		view.RunwayCreditVerdict(f.Credit, now, loc))
	if err := w.Flush(); err != nil {
		return err
	}
	// The credit row means something different from the two above it, and a
	// table cannot say so on its own.
	fmt.Fprintln(out, "\n  The two window rows ask whether resets give quota back faster than the fleet\n"+
		"  spends it. Credits do not reset: that row is a balance divided by a rate,\n"+
		"  with nothing coming back.")

	if len(f.Rows) == 0 {
		return nil
	}
	fmt.Fprintln(out)
	labels := make(map[string]store.Account, len(accounts))
	for _, a := range accounts {
		labels[a.UUID] = a
	}
	rw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(rw, "  IDX\tACCOUNT\tWINDOW\tLEFT\tBURN\tEMPTY")
	for _, r := range f.Rows {
		// The forecast knows uuids and nothing else, so the label is resolved
		// here. An account the store cannot name is still rowed under its uuid:
		// dropping it would leave the rows above and below it claiming to sum
		// to a fleet figure that counted it.
		a, known := labels[r.UUID]
		idx, label := "?", r.UUID
		if known {
			idx, label = fmt.Sprintf("%d", a.Idx), a.Label()
		}
		fmt.Fprintf(rw, "  %s\t%s\t%s\t%s\t%s\t%s\n", idx, label, string(r.Window),
			view.RunwayLeft(r.Left), view.RunwayBurn(r.Burn), view.RunwayEmpty(r, now, loc))
	}
	return rw.Flush()
}

// runwayBasisLine is the evidence the answer rests on, printed above it so a
// reader can weigh the answer rather than take it. A four-hour rate is a
// speedometer: twenty minutes of evidence and four hours of it support very
// different claims.
func runwayBasisLine(f forecast.Fleet) string {
	span := "nothing measured yet"
	if f.Basis.Known {
		span = "the last " + view.HumanDuration(f.Basis.Observed)
	}
	return fmt.Sprintf("Basis:   %s  (%d %s, %d %s, %d unreadable)", span,
		f.Basis.Accounts, plural(f.Basis.Accounts, "account", "accounts"),
		f.Basis.Readings, plural(f.Basis.Readings, "reading", "readings"),
		f.Basis.Unreadable)
}

// forecastJSON is the `forecast` object, and it is the SAME object `ccdad
// status --json` and `ccdad list --json` publish under that key: three commands
// describing one measurement must not describe it three ways.
//
// Every key is omitted rather than zeroed when its value could not be read,
// following the rule the rest of these payloads keep — an unreported figure is
// not a figure of zero, and a consumer that saw one could not tell them apart.
//
// There is deliberately no top-level burn figure. The two axes' percentage
// points are different quantities, so a consumer that added them would get a
// number with no unit; a rate exists only inside an axis.
func forecastJSON(f forecast.Fleet) map[string]any {
	basis := map[string]any{
		"readings":   f.Basis.Readings,
		"accounts":   f.Basis.Accounts,
		"unmeasured": f.Basis.Unmeasured,
		"unreadable": f.Basis.Unreadable,
	}
	out := map[string]any{"basis": basis}
	if f.Basis.Known {
		// Both spans describe a measurement that happened. On a machine that
		// has recorded nothing they are absent rather than present as four
		// hours of nothing, which would describe evidence the store does not
		// hold.
		basis["windowSeconds"] = int(f.Basis.Window.Seconds())
		basis["observedSeconds"] = int(f.Basis.Observed.Seconds())
		out["axes"] = map[string]any{
			"five_hour": axisJSON(f.FiveHour),
			"weekly":    axisJSON(f.Weekly),
		}
	}

	fleet := map[string]any{}
	if f.PointsTotal > 0 {
		fleet["pointsLeft"] = f.PointsLeft
		fleet["pointsTotal"] = f.PointsTotal
	}
	// The fleet's own moment is the run with BOTH axes burning, which is the
	// question a user is asking when they ask how long the fleet lasts. It can
	// be earlier than either axis alone, because that run has more burn and
	// more ways to end.
	if f.Both.HasDryAt {
		fleet["dryAt"] = f.Both.DryAt
	}
	if len(fleet) > 0 {
		out["fleet"] = fleet
	}
	if !f.Credit.Known {
		// Money fails closed: a balance that could not be assembled is absent,
		// never zero. A zero spend rate published here would read as a fleet
		// whose credits last forever.
		return out
	}
	out["credit"] = map[string]any{
		"currency":     f.Credit.Currency,
		"spendPerHour": f.Credit.SpendPerHour,
		"dryAt":        f.Credit.DryAt,
	}
	return out
}

// axisJSON is one axis. `holds` is ABSENT rather than false when the two runs
// of the measured band disagreed: the evidence did not carry a claim, and
// `false` is a claim.
func axisJSON(a forecast.Axis) map[string]any {
	out := map[string]any{"replenishPpPerHour": a.Replenish}
	if a.Burn.Known {
		// The upper bound rides beside the figure because it is what a claim of
		// "holds" had to survive. A consumer given only the measured rate
		// cannot tell a wide band from a narrow one.
		out["burnPpPerHour"] = a.Burn.Low
		out["burnPpPerHourHigh"] = a.Burn.High
	}
	if a.Verdict != forecast.VerdictUnknown {
		out["holds"] = a.Verdict == forecast.VerdictHolds
	}
	if a.HasDryAt {
		out["dryAt"] = a.DryAt
	}
	return out
}

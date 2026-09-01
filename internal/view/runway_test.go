// This file is package view_test rather than package view, and it is the only
// one in this directory that is. Timestamp exists so that the packages that
// render a moment spell one absolute layout once, so the test that matters is
// the one those packages can write: through the exported surface, with nothing
// unexported in reach. Its siblings stay in package view because they reach
// unexported state on Row.
package view_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/forecast"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// The zone is part of the rendering, not part of the machine. A test that
// asserts a bare local string passes in the author's zone and prints a
// different hour in CI, where TZ is unset.
func TestTimestampAlwaysCarriesItsZone(t *testing.T) {
	// time.Local is pinned for the duration of this test, and without that pin
	// the nil row below rules nothing out. Nothing sets TZ in CI, so time.Local
	// is UTC there, and an implementation that resolved nil to time.Local would
	// render identically to one that resolves it to UTC -- measured: with that
	// substitution in place the nil assertion passes under TZ=UTC and fails
	// only on a machine whose zone happens not to be UTC. Pinning makes the row
	// decide the same thing everywhere. No test in this package calls
	// t.Parallel(), so the assignment is contained; Cleanup puts it back.
	saved := time.Local
	time.Local = time.FixedZone("XYZ", -7*3600)
	t.Cleanup(func() { time.Local = saved })

	at := time.Date(2026, 8, 27, 5, 10, 0, 0, time.UTC)
	kst := time.FixedZone("KST", 9*3600)
	if got, want := view.Timestamp(at, kst), "2026-08-27 14:10 KST"; got != want {
		t.Errorf("Timestamp(kst) = %q, want %q", got, want)
	}
	if got, want := view.Timestamp(at, time.UTC), "2026-08-27 05:10 UTC"; got != want {
		t.Errorf("Timestamp(utc) = %q, want %q", got, want)
	}
	// A nil location is the caller's bug, not a reason to print a wrong hour.
	if got, want := view.Timestamp(at, nil), "2026-08-27 05:10 UTC"; got != want {
		t.Errorf("Timestamp(nil) = %q, want %q", got, want)
	}
}

// The line is how status, list and the dashboard all agree on one wording, and
// the empty string is how all three know not to print anything at all. A
// machine with no history behind it must produce nothing here: a line saying
// "holds" on no evidence is the one output this whole feature exists to refuse.
func TestTheRunwayLineIsEmptyWithoutABasis(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	// Basis.Known false with two axes that both claim to hold: only the basis
	// may decide this, or a cold machine prints a promise.
	f := forecast.Fleet{
		FiveHour: forecast.Axis{Verdict: forecast.VerdictHolds},
		Weekly:   forecast.Axis{Verdict: forecast.VerdictHolds},
	}
	if got := view.RunwayLine(f, now, time.UTC); got != "" {
		t.Fatalf("RunwayLine = %q on a fleet with no basis; the empty string is how three renderers know not to print", got)
	}
}

// The line carries verdicts, not rates: a rate is per axis and two of them do
// not fit a line that also has to carry the answer. It carries no percentage
// either, because four existing tests forbid the substrings a percentage would
// bring with it into ccdad status's human output.
func TestTheRunwayLineNamesTheAxisThatRunsDryFirst(t *testing.T) {
	// Fifty-four hours before the dry moment, so the relative span in the line
	// is checkable by hand against the absolute one beside it.
	now := time.Date(2026, 8, 24, 23, 10, 0, 0, time.UTC)
	kst := time.FixedZone("KST", 9*3600)
	dry := time.Date(2026, 8, 27, 5, 10, 0, 0, time.UTC)
	f := forecast.Fleet{
		Basis:    forecast.Basis{Observed: 3*time.Hour + 51*time.Minute, Known: true},
		FiveHour: forecast.Axis{Burn: forecast.Band{Low: 61, Known: true}, Verdict: forecast.VerdictHolds},
		Weekly: forecast.Axis{
			Burn: forecast.Band{Low: 4.2, Known: true}, Verdict: forecast.VerdictRunsDry,
			DryAt: dry, HasDryAt: true,
		},
		// The fleet run burns both axes at once, so it can never outlast the
		// axis that empties first. Here it agrees with the weekly one, which is
		// the ordinary case and the one that must not add a third clause.
		Both: forecast.Axis{Verdict: forecast.VerdictRunsDry, DryAt: dry, HasDryAt: true},
	}
	got := view.RunwayLine(f, now, kst)
	want := "7d dry 2026-08-27 14:10 KST (2d6h)  ·  5h holds  ·  basis 3h51m"
	if got != want {
		t.Fatalf("RunwayLine =\n\t%q\nwant\n\t%q", got, want)
	}
	// A rate on this line would drag a percentage onto ccdad status's human
	// stdout, where four tests forbid one.
	if strings.ContainsAny(got, "%") || strings.Contains(got, "pp/h") {
		t.Errorf("RunwayLine = %q; the line carries verdicts, not rates", got)
	}
}

// Both axes holding is one claim, not two, and it says at what: "at this rate"
// is the whole qualification the measurement supports.
func TestTheRunwayLineSaysBothWhenBothHold(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	f := forecast.Fleet{
		Basis:    forecast.Basis{Observed: 3*time.Hour + 51*time.Minute, Known: true},
		FiveHour: forecast.Axis{Verdict: forecast.VerdictHolds},
		Weekly:   forecast.Axis{Verdict: forecast.VerdictHolds},
		Both:     forecast.Axis{Verdict: forecast.VerdictHolds},
	}
	if got, want := view.RunwayLine(f, now, time.UTC), "holds on both axes at this rate  ·  basis 3h51m"; got != want {
		t.Fatalf("RunwayLine = %q, want %q", got, want)
	}
}

// An axis whose two runs disagreed has no verdict, and the line has to say so
// rather than leave it out. Omitting it would let a reader take the axis that
// IS reported as covering the fleet, which is the fail-open reading of a
// tri-state that this repository does not permit anywhere else.
func TestAnUndecidedAxisIsPrintedRatherThanOmitted(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	f := forecast.Fleet{
		Basis:    forecast.Basis{Observed: 40 * time.Minute, Known: true},
		FiveHour: forecast.Axis{Verdict: forecast.VerdictUnknown},
		Weekly:   forecast.Axis{Verdict: forecast.VerdictHolds},
		Both:     forecast.Axis{Verdict: forecast.VerdictUnknown},
	}
	got := view.RunwayLine(f, now, time.UTC)
	if !strings.Contains(got, "5h ") {
		t.Fatalf("RunwayLine = %q, which says nothing about the five-hour axis; silence there reads as agreement with the axis that is named", got)
	}
	if strings.Contains(got, "5h holds") {
		t.Fatalf("RunwayLine = %q, which promises an axis whose two runs disagreed", got)
	}
}

// The fleet run burns both axes at once, so it has strictly more burn and
// strictly more ways to end than either axis alone: it can empty a fleet whose
// two axes each hold on their own. Letting "holds on both axes" stand over that
// fleet would be the one fail-open reading this line must not permit.
func TestAFleetThatEmptiesWithBothAxesBurningIsNotReportedAsHolding(t *testing.T) {
	now := time.Date(2026, 8, 24, 23, 10, 0, 0, time.UTC)
	dry := time.Date(2026, 8, 27, 5, 10, 0, 0, time.UTC)
	f := forecast.Fleet{
		Basis:    forecast.Basis{Observed: 4 * time.Hour, Known: true},
		FiveHour: forecast.Axis{Verdict: forecast.VerdictHolds},
		Weekly:   forecast.Axis{Verdict: forecast.VerdictHolds},
		Both:     forecast.Axis{Verdict: forecast.VerdictRunsDry, DryAt: dry, HasDryAt: true},
	}
	got := view.RunwayLine(f, now, time.UTC)
	if strings.Contains(got, "holds on both axes") {
		t.Fatalf("RunwayLine = %q; both axes held separately and the fleet still empties", got)
	}
	if !strings.Contains(got, "2026-08-27 05:10 UTC") {
		t.Fatalf("RunwayLine = %q, which names no moment for a fleet the arithmetic empties", got)
	}
}

// The three glyphs are three, and a renderer that reached for the wrong one
// would tell a reader the opposite of what happened. "?" is "nobody could read
// it, or nothing cleared the gates"; "-" is "there is no such quantity here";
// anything else is a measurement. The credit row is where they meet: its
// replenishment cell is "-" because credits do not reset, and rendering it as
// "?" would say the fleet's renewal rate merely could not be read.
func TestARunwayCellNeverSubstitutesOneGlyphForAnother(t *testing.T) {
	if got := view.RunwayBurn(forecast.Band{Low: 4.2}); got != view.Unreadable {
		t.Errorf("RunwayBurn(unmeasured) = %q, want %q: a band that cleared no gate is unknown, not a rate", got, view.Unreadable)
	}
	if got := view.RunwayCreditReplenish(); got != view.NoQuantity {
		t.Errorf("RunwayCreditReplenish() = %q, want %q: credits have no renewal boundary at all, "+
			"which is a different statement from one that could not be read", got, view.NoQuantity)
	}
	if view.NoQuantity == view.Unreadable {
		t.Fatal("the two glyphs are equal, so every cell above asserts nothing")
	}
}

// A measured zero is evidence: the account was up, it was polled, and it did
// not burn. One live login at a time means most rows read 0.0 pp/h, so the
// difference between that and "?" is the difference between a quiet fleet and
// an unmeasured one, on most rows of an ordinary table.
func TestAMeasuredZeroBurnIsAReadingAndNotAnAbsence(t *testing.T) {
	if got, want := view.RunwayBurn(forecast.Band{Known: true}), "0.0 pp/h"; got != want {
		t.Errorf("RunwayBurn(measured zero) = %q, want %q", got, want)
	}
	if got, want := view.RunwayBurn(forecast.Band{Low: 4.2, High: 5.4, Known: true}), "4.2 pp/h"; got != want {
		// Low is what is printed. High is what a claim of "holds" had to
		// survive, and it belongs to the verdict rather than to this cell.
		t.Errorf("RunwayBurn = %q, want %q", got, want)
	}
	if got, want := view.RunwayReplenish(2.976), "3.0 pp/h"; got != want {
		t.Errorf("RunwayReplenish = %q, want %q", got, want)
	}
}

// LEFT is bare, with no percent sign. Every one of `ccdad status`'s four
// substring guards forbids a figure belonging to a window its own table is not
// reporting, and this column reports a different window from that one.
func TestTheLeftCellCarriesNoPercentSign(t *testing.T) {
	got := view.RunwayLeft(15.4)
	if got != "15" {
		t.Errorf("RunwayLeft(15.4) = %q, want %q", got, "15")
	}
	if strings.Contains(got, "%") {
		t.Errorf("RunwayLeft = %q; the cell is headed LEFT and carries no unit", got)
	}
}

// An account with no weekly window gets "-" in all three weekly cells, not "?".
// The account was read; this quantity does not exist for it. Filling the cells
// from its five-hour window instead would put a five-hour room and a five-hour
// rate into a column summed into the weekly axis above, and the same prompt
// moves those two figures at very different speeds.
func TestTheWeeklyCellsSayNoQuantityRatherThanBorrowAnotherWindow(t *testing.T) {
	weekly := forecast.AccountRow{
		Window: "seven_day", HasWindow: true, Left: 52,
		Burn: forecast.Band{Low: 2, High: 2.5, Known: true},
	}
	w, l, b := view.RunwayRowCells(weekly)
	if w != "seven_day" || l != "52" || b != "2.0 pp/h" {
		t.Errorf("RunwayRowCells(weekly) = %q/%q/%q, want seven_day/52/2.0 pp/h", w, l, b)
	}
	// The same account minus its weekly window: the five-hour figures it does
	// have are deliberately present in the value and must not reach the row.
	none := forecast.AccountRow{Left: 60, Burn: forecast.Band{Low: 7.5, Known: true}}
	w, l, b = view.RunwayRowCells(none)
	if w != view.NoQuantity || l != view.NoQuantity || b != view.NoQuantity {
		t.Errorf("RunwayRowCells(no weekly window) = %q/%q/%q, want %q in all three", w, l, b, view.NoQuantity)
	}
}

// Three states, three renderings. An account already out is "now", which is a
// fact about this minute; an account the run found out later gets that moment;
// an account the run never found out gets "?", because the run deciding nothing
// is not the run promising the account survives.
func TestTheEmptyCellSeparatesAlreadyOutFromNeverOut(t *testing.T) {
	now := time.Date(2026, 8, 24, 23, 10, 0, 0, time.UTC)
	kst := time.FixedZone("KST", 9*3600)
	out := view.RunwayEmpty(forecast.AccountRow{OutNow: true}, now, kst)
	if out != "now" {
		t.Errorf("RunwayEmpty(out now) = %q, want %q", out, "now")
	}
	later := forecast.AccountRow{EmptyAt: time.Date(2026, 8, 27, 5, 10, 0, 0, time.UTC), HasEmpty: true}
	if got, want := view.RunwayEmpty(later, now, kst), "2026-08-27 14:10 KST"; got != want {
		t.Errorf("RunwayEmpty(found out) = %q, want %q", got, want)
	}
	if got := view.RunwayEmpty(forecast.AccountRow{}, now, kst); got != view.Unreadable {
		t.Errorf("RunwayEmpty(never out) = %q, want %q", got, view.Unreadable)
	}
	// An account that is out NOW and also carries the moment the run first saw
	// it out reads "now": the reader is being told about this minute, and a
	// date would invite them to wait for it.
	both := forecast.AccountRow{OutNow: true, EmptyAt: now.Add(time.Hour), HasEmpty: true}
	if got := view.RunwayEmpty(both, now, kst); got != "now" {
		t.Errorf("RunwayEmpty(out now, with a moment) = %q, want %q", got, "now")
	}
}

// A dry axis is dated in the reader's zone, and the span beside it is what
// makes the date usable without arithmetic. An axis that decided nothing reads
// "?" and never "holds": the two runs disagreeing is not a promise.
func TestTheVerdictCellDatesADryAxisAndRefusesAnUndecidedOne(t *testing.T) {
	now := time.Date(2026, 8, 24, 23, 10, 0, 0, time.UTC)
	kst := time.FixedZone("KST", 9*3600)
	dry := forecast.Axis{
		Verdict: forecast.VerdictRunsDry,
		DryAt:   time.Date(2026, 8, 27, 5, 10, 0, 0, time.UTC), HasDryAt: true,
	}
	if got, want := view.RunwayVerdict(dry, now, kst), "runs dry 2026-08-27 14:10 KST  (in 2d6h)"; got != want {
		t.Errorf("RunwayVerdict(dry) = %q, want %q", got, want)
	}
	if got, want := view.RunwayVerdict(forecast.Axis{Verdict: forecast.VerdictHolds}, now, kst), "holds"; got != want {
		t.Errorf("RunwayVerdict(holds) = %q, want %q", got, want)
	}
	if got := view.RunwayVerdict(forecast.Axis{}, now, kst); got != view.Unreadable {
		t.Errorf("RunwayVerdict(undecided) = %q, want %q", got, view.Unreadable)
	}
}

// Money fails closed. A credit figure that could not be assembled -- mixed
// currencies, an uncapped account that is spending, no measured spend at all --
// renders as unknown in both cells rather than as a fleet spending nothing.
func TestTheCreditCellsRefuseRatherThanDefault(t *testing.T) {
	now := time.Date(2026, 8, 24, 23, 10, 0, 0, time.UTC)
	kst := time.FixedZone("KST", 9*3600)
	if got := view.RunwayCreditBurn(forecast.CreditFleet{}); got != view.Unreadable {
		t.Errorf("RunwayCreditBurn(unknown) = %q, want %q", got, view.Unreadable)
	}
	if got := view.RunwayCreditVerdict(forecast.CreditFleet{}, now, kst); got != view.Unreadable {
		t.Errorf("RunwayCreditVerdict(unknown) = %q, want %q", got, view.Unreadable)
	}
	// A refused figure that still carried a rate and a date -- which is what a
	// mutation dropping the Known check would produce -- must not print either.
	refused := forecast.CreditFleet{Currency: "USD", SpendPerHour: 1.4, DryAt: now.Add(time.Hour)}
	if got := view.RunwayCreditBurn(refused); got != view.Unreadable {
		t.Errorf("RunwayCreditBurn(refused) = %q, want %q", got, view.Unreadable)
	}
	if got := view.RunwayCreditVerdict(refused, now, kst); got != view.Unreadable {
		t.Errorf("RunwayCreditVerdict(refused) = %q, want %q", got, view.Unreadable)
	}

	known := forecast.CreditFleet{
		Currency: "USD", SpendPerHour: 1.4,
		DryAt: time.Date(2026, 8, 27, 5, 10, 0, 0, time.UTC), Known: true,
	}
	// Two decimals, because the unit is money and the currency is named beside
	// it: two accounts' figures do not add up without the code.
	if got, want := view.RunwayCreditBurn(known), "1.40 USD/h"; got != want {
		t.Errorf("RunwayCreditBurn = %q, want %q", got, want)
	}
	if got, want := view.RunwayCreditVerdict(known, now, kst), "runs dry 2026-08-27 14:10 KST  (in 2d6h)"; got != want {
		t.Errorf("RunwayCreditVerdict = %q, want %q", got, want)
	}
}

// The accounts line has one form per state the search can end in, and the
// parenthetical is the actionable half: it is present exactly when there is
// something to act on and absent when the fleet already sits on the answer.
//
// The unknown count is "?" and never "-". The number exists -- every fleet has
// a smallest size that holds -- and what happened is that nobody could measure
// it. A reader shown "-" would be told the quantity does not exist here and
// would stop looking for the history that would produce it.
func TestTheAccountsLineHasOneFormPerState(t *testing.T) {
	for _, c := range []struct {
		name string
		f    forecast.Fleet
		want string
	}{
		{
			name: "short",
			f: forecast.Fleet{
				AccountsUsable: 5,
				AccountsNeeded: 9, HasNeeded: true,
			},
			want: "5 usable, 9 needed to hold at this rate  (4 more)",
		},
		{
			name: "holding with room",
			f: forecast.Fleet{
				AccountsUsable: 5,
				AccountsNeeded: 3, HasNeeded: true,
			},
			want: "5 usable, 3 needed to hold at this rate  (2 to spare)",
		},
		{
			name: "holding exactly",
			f: forecast.Fleet{
				AccountsUsable: 5,
				AccountsNeeded: 5, HasNeeded: true,
			},
			want: "5 usable, 5 needed to hold at this rate",
		},
		{
			// HasNeeded false is the only thing that decides this: the search
			// answers nothing rather than one when it has no rate to search
			// against, and a fleet told it needs one account on no evidence
			// would be told to cancel four.
			name: "no basis",
			f:    forecast.Fleet{AccountsUsable: 5},
			want: "5 usable, ? needed  (not enough history)",
		},
		{
			// The bound is not a count somebody can go and buy, so it is never
			// rendered as one. "256 needed  (251 more)" would name a purchase
			// that does not fix the fleet.
			name: "capped",
			f: forecast.Fleet{
				AccountsUsable: 5,
				AccountsNeeded: 256, HasNeeded: true, NeededCapped: true,
			},
			want: "5 usable, more than 256 needed to hold at this rate",
		},
		{
			// A fleet the search reached its ceiling on can be no smaller than
			// the ceiling and still not hold, and then the count equals the
			// usable one. "256 usable, 256 needed to hold at this rate" would
			// report a fleet that holds, which is the opposite of what the
			// search found.
			name: "capped at the fleet's own size",
			f: forecast.Fleet{
				AccountsUsable: 256,
				AccountsNeeded: 256, HasNeeded: true, NeededCapped: true,
			},
			want: "256 usable, more than 256 needed to hold at this rate",
		},
		{
			// Nothing the rotation can reach is nothing to report a seat count
			// about. The basis line above already carries how many accounts
			// there are and why none of them qualified.
			name: "no usable accounts",
			f:    forecast.Fleet{},
			want: "",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := view.RunwayAccounts(c.f); got != c.want {
				t.Errorf("RunwayAccounts =\n\t%q\nwant\n\t%q", got, c.want)
			}
		})
	}
	// Spelled against the constants as well as against the literals above, so
	// that swapping the two glyphs fails here even if somebody redefines one.
	noBasis := view.RunwayAccounts(forecast.Fleet{AccountsUsable: 5})
	if !strings.Contains(noBasis, view.Unreadable) {
		t.Errorf("RunwayAccounts(no basis) = %q, which does not carry %q", noBasis, view.Unreadable)
	}
	if strings.Contains(noBasis, view.NoQuantity) {
		t.Errorf("RunwayAccounts(no basis) = %q, which says the count does not exist; it exists and was not measured", noBasis)
	}
}

// The dashboard clause is carried only by a fleet that is SHORT. A fleet that
// holds already has its answer in the word "holds", and a summary line that
// also spent a clause on good news would be spending it on the line where the
// short case needs the room.
func TestTheSummaryClauseIsCarriedOnlyByAShortFleet(t *testing.T) {
	short := forecast.Fleet{AccountsUsable: 5, AccountsNeeded: 9, HasNeeded: true}
	if got, want := view.RunwayNeedSegment(short), "need 9 (4 more)"; got != want {
		t.Errorf("RunwayNeedSegment(short) = %q, want %q", got, want)
	}
	// A bound says how much is not enough, which the short fleet's reader still
	// needs, and it says it without naming a number of seats to buy.
	capped := forecast.Fleet{AccountsUsable: 5, AccountsNeeded: 256, HasNeeded: true, NeededCapped: true}
	if got, want := view.RunwayNeedSegment(capped), "need more than 256"; got != want {
		t.Errorf("RunwayNeedSegment(capped) = %q, want %q", got, want)
	}
	for _, c := range []struct {
		name string
		f    forecast.Fleet
	}{
		{"holding with room", forecast.Fleet{AccountsUsable: 5, AccountsNeeded: 3, HasNeeded: true}},
		{"holding exactly", forecast.Fleet{AccountsUsable: 5, AccountsNeeded: 5, HasNeeded: true}},
		{"no basis", forecast.Fleet{AccountsUsable: 5}},
		{"no usable accounts", forecast.Fleet{}},
	} {
		if got := view.RunwayNeedSegment(c.f); got != "" {
			t.Errorf("RunwayNeedSegment(%s) = %q, want %q", c.name, got, "")
		}
	}
}

// The clause reaches status, list and the dashboard because it is part of the
// one line all three render, and it sits LAST, after the basis.
//
// That order is about the frame rather than about reading: the dashboard cuts
// this line from the right, and at its 80-column design target a short fleet's
// line is too long to fit, so the last clause is the one that goes. It must not
// be the evidence -- a verdict with no basis beside it is the output this whole
// measurement refuses to produce, and the dashboard prints the span nowhere
// else. It is pinned there by TestTheRunwayLineKeepsItsBasisWhenTheFrameCutsIt
// in internal/tui, which renders the cut page; this asserts the bytes.
func TestTheRunwayLineCarriesTheNeedOfAShortFleet(t *testing.T) {
	now := time.Date(2026, 8, 24, 23, 10, 0, 0, time.UTC)
	kst := time.FixedZone("KST", 9*3600)
	dry := time.Date(2026, 8, 27, 5, 10, 0, 0, time.UTC)
	f := forecast.Fleet{
		Basis:    forecast.Basis{Observed: 3*time.Hour + 51*time.Minute, Known: true},
		FiveHour: forecast.Axis{Verdict: forecast.VerdictHolds},
		Weekly: forecast.Axis{
			Verdict: forecast.VerdictRunsDry, DryAt: dry, HasDryAt: true,
		},
		Both:           forecast.Axis{Verdict: forecast.VerdictRunsDry, DryAt: dry, HasDryAt: true},
		AccountsUsable: 5,
		AccountsNeeded: 9, HasNeeded: true,
	}
	got := view.RunwayLine(f, now, kst)
	want := "7d dry 2026-08-27 14:10 KST (2d6h)  ·  5h holds  ·  basis 3h51m  ·  need 9 (4 more)"
	if got != want {
		t.Fatalf("RunwayLine =\n\t%q\nwant\n\t%q", got, want)
	}

	// The same fleet holding, with slack to spare: the line says so and spends
	// no clause on the slack.
	holding := forecast.Fleet{
		Basis:          forecast.Basis{Observed: 3*time.Hour + 51*time.Minute, Known: true},
		FiveHour:       forecast.Axis{Verdict: forecast.VerdictHolds},
		Weekly:         forecast.Axis{Verdict: forecast.VerdictHolds},
		Both:           forecast.Axis{Verdict: forecast.VerdictHolds},
		AccountsUsable: 5,
		AccountsNeeded: 3, HasNeeded: true,
	}
	if got, want := view.RunwayLine(holding, now, kst), "holds on both axes at this rate  ·  basis 3h51m"; got != want {
		t.Fatalf("RunwayLine(holding) =\n\t%q\nwant\n\t%q", got, want)
	}
}

// The line is 139 display columns on a live fleet and the terminal that reads
// it is 80. Left alone the terminal folds it wherever the 80th column happens
// to land, which on that measurement was inside `2026-08-26 17:21 KST` -- and
// the clauses are separated by a middot, so a fold that lands between two of
// them is indistinguishable from one that lands inside one.
//
// What is asserted here is the invariant, not a golden block: every line fits,
// no clause is lost, and a reader can tell a continued line from a finished
// one. A fixture would pin the greedy packing as well, and the packing is the
// half that is allowed to change.
func TestTheRunwayLineFoldsAtItsOwnSeparatorsAndNowhereElse(t *testing.T) {
	const label = "Runway:  "
	const width = 80
	line := "5h+7d dry 2026-08-26 08:19 KST (11h21m)  ·  7d dry 2026-08-26 17:21 KST (20h23m)" +
		"  ·  5h holds  ·  basis 3h57m  ·  need 14 (8 more)"

	got := view.RunwayWrap(label, line, width)
	lines := strings.Split(got, "\n")
	if len(lines) < 2 {
		t.Fatalf("a %d-column line did not fold at %d columns:\n%s", ansi.StringWidth(label+line), width, got)
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w > width {
			t.Errorf("line %d is %d columns wide, over the %d it was given:\n%s", i, w, width, got)
		}
	}
	if !strings.HasPrefix(lines[0], label) {
		t.Errorf("the first line does not carry the label:\n%s", got)
	}
	// The label is nine columns and the clauses under it line up with the first
	// one, not with the left margin: a continuation flush against `Runway:`
	// reads as another labelled line of the block above it.
	for i, l := range lines[1:] {
		if !strings.HasPrefix(l, strings.Repeat(" ", len(label))) || strings.HasPrefix(l, strings.Repeat(" ", len(label)+1)) {
			t.Errorf("continuation line %d is not hung under the first clause: %q", i+1, l)
		}
	}
	// A line that continues says so. Ending a folded line on a clause boundary
	// with nothing there is the ambiguity this whole test exists about: the
	// reader cannot tell it from a line that simply ended.
	for i, l := range lines[:len(lines)-1] {
		if !strings.HasSuffix(l, "  ·") {
			t.Errorf("folded line %d does not end on the separator it broke at: %q", i, l)
		}
	}
	if strings.HasSuffix(lines[len(lines)-1], "·") {
		t.Errorf("the last line ends on a separator, promising a clause that is not there: %q", lines[len(lines)-1])
	}
	if got, want := wrapClauses(t, got, label), strings.Split(line, "  ·  "); !slices.Equal(got, want) {
		t.Errorf("the fold changed the clauses:\ngot  %q\nwant %q", got, want)
	}
}

// The empty width is what every non-terminal writer reports -- a pipe, a
// redirect, the buffer a test renders into -- and there the line has to come
// out exactly as it did before this function existed. It is also what a
// terminal too narrow to hold the label reports, and folding into no room at
// all produces a column of single characters.
func TestAnUnknownWidthLeavesTheRunwayLineExactlyAsItWas(t *testing.T) {
	const label = "Runway:  "
	line := "5h+7d dry 2026-08-26 08:19 KST (11h21m)  ·  basis 3h57m  ·  need 14 (8 more)"
	for _, width := range []int{0, -1, 9, 3} {
		if got, want := view.RunwayWrap(label, line, width), label+line; got != want {
			t.Errorf("RunwayWrap(width=%d) =\n\t%q\nwant\n\t%q", width, got, want)
		}
	}
}

// A clause is atomic. The line ends in an absolute moment and a span, and a cut
// through either reads as a shorter moment rather than as a line that did not
// fit -- `2026-08-26 08:1` is a date. Overflowing is visible and honest;
// cutting is neither, which is why the dashboard, which must cut, appends "..".
func TestAClauseWiderThanTheTerminalOverflowsRatherThanBeingCut(t *testing.T) {
	const label = "Runway:  "
	const long = "5h+7d dry 2026-08-26 08:19 KST (11h21m)"
	got := view.RunwayWrap(label, long+"  ·  basis 3h57m", 24)
	if !strings.Contains(got, long) {
		t.Fatalf("the clause was cut rather than allowed to overflow:\n%s", got)
	}
	for _, l := range strings.Split(got, "\n") {
		if strings.Contains(l, "..") {
			t.Errorf("a truncation cue leaked into the CLI rendering: %q", l)
		}
	}
}

// The separator is U+00B7: one display column, two bytes. Every width here is
// therefore a column count, and a byte count is wrong by exactly the number of
// separators on the line -- which is the number that grows as the line gets
// longer. This width is one the line fits by columns and does not fit by
// bytes, so it fails against len() and passes against a display width.
func TestTheFoldCountsDisplayColumnsAndNotBytes(t *testing.T) {
	const label = "Runway:  "
	line := "AAAA  ·  BBBB"
	width := len(label) + ansi.StringWidth(line)
	if len(line) == ansi.StringWidth(line) {
		t.Fatalf("the fixture carries no multi-byte separator, so it rules nothing out: %q", line)
	}
	if got, want := view.RunwayWrap(label, line, width), label+line; got != want {
		t.Errorf("a line that fits by columns was folded:\ngot  %q\nwant %q", got, want)
	}
}

// wrapClauses recovers the clauses from a wrapped block: the label off the
// first line, the hanging indent off the rest, and the separator off both
// forms it takes.
func wrapClauses(t *testing.T, block, label string) []string {
	t.Helper()
	var out []string
	for i, l := range strings.Split(block, "\n") {
		if i == 0 {
			l = strings.TrimPrefix(l, label)
		} else {
			l = strings.TrimPrefix(l, strings.Repeat(" ", len(label)))
		}
		l = strings.TrimSuffix(l, "  ·")
		out = append(out, strings.Split(l, "  ·  ")...)
	}
	return out
}

// The "  ·" a folded line ends on is three columns the line did not have when
// the decision to break was taken -- unless the measurement carries it. A fit
// test that weighs a clause and appends the marker afterwards decides on five
// columns and spends eight, so a line that "just fit" comes out up to three
// columns over and folds again in the terminal, which is the whole defect.
//
// This width is inside that band: the two leading clauses fit by the cheap
// arithmetic and do not fit by the honest one.
func TestTheSeparatorAFoldedLineEndsOnIsInsideTheMeasurement(t *testing.T) {
	const label = "Runway:  " // nine columns, so the room below is 21
	const width = 30
	line := strings.Join([]string{"AAAAAAAAAA", "BBBBB", "CC"}, "  ·  ")

	got := view.RunwayWrap(label, line, width)
	for i, l := range strings.Split(got, "\n") {
		if w := ansi.StringWidth(l); w > width {
			t.Errorf("line %d is %d columns, over the %d it was given -- the break marker was not weighed:\n%s", i, w, width, got)
		}
	}
	if c := wrapClauses(t, got, label); !slices.Equal(c, []string{"AAAAAAAAAA", "BBBBB", "CC"}) {
		t.Errorf("the fold changed the clauses: %q", c)
	}
}

// The runway line is not the only one in that block too wide for the terminal
// it is read on. Measured on the same 80-column run that filed the runway
// defect: Mode: 124 display columns, Hover: 100. They fold in the terminal for
// the same reason and read badly for a different one -- these are prose, so a
// fold lands mid-sentence rather than mid-value.
//
// The two wraps are separate functions on purpose and this test is where that
// shows: this one breaks at spaces, which is right for a sentence and would be
// wrong for the runway line, where the space inside `2026-08-26 08:19 KST` is
// not a place a break may land.
func TestALabelledLineWrapsUnderItsOwnLabel(t *testing.T) {
	const width = 80
	line := "Mode:    recovery  (every account is over its threshold; empty accounts last, " +
		"then soonest reset inside an hour, then slack)"

	got := view.WrapLabeled(line, width)
	lines := strings.Split(got, "\n")
	if len(lines) < 2 {
		t.Fatalf("a %d-column line did not wrap at %d:\n%s", ansi.StringWidth(line), width, got)
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w > width {
			t.Errorf("line %d is %d columns, over the %d it was given:\n%s", i, w, width, got)
		}
		if strings.HasSuffix(l, " ") {
			t.Errorf("line %d ends in whitespace, which is a fold the reader pays for twice: %q", i, l)
		}
	}
	if !strings.HasPrefix(lines[0], "Mode:    ") {
		t.Errorf("the label did not stay on the first line:\n%s", got)
	}
	// Nine columns: the label field Daemon:, Active:, Hover: and Mode: are all
	// padded to. A continuation flush left reads as another label.
	for i, l := range lines[1:] {
		if !strings.HasPrefix(l, "         ") || strings.HasPrefix(l, "          ") {
			t.Errorf("continuation %d is not hung under the value: %q", i+1, l)
		}
	}
	// Nothing added, nothing dropped, nothing reordered. The words are the
	// sentence; where the breaks fall is this function's business and may move.
	if got, want := strings.Join(strings.Fields(got), " "), strings.Join(strings.Fields(line), " "); got != want {
		t.Errorf("the wrap changed the words:\ngot  %q\nwant %q", got, want)
	}
}

// The spacing inside a labelled line is not decoration: `recovery  (every ...`
// separates the answer from the parenthetical that explains it, exactly as the
// runway line's separator does. A wrapper that normalises whitespace loses that
// distinction everywhere the line still fits.
func TestALabelledWrapKeepsTheSpacingInsideALine(t *testing.T) {
	line := "Mode:    consume-first  (spending perishable weekly quota before it expires)"
	if got := view.WrapLabeled(line, 200); got != line {
		t.Errorf("a line that fits was rewritten:\n\t%q", got)
	}
	// And when it does wrap, the run that stays inside a line stays intact.
	got := view.WrapLabeled(line, 60)
	if !strings.Contains(got, "consume-first  (spending") {
		t.Errorf("the double space between the mode and its reason was normalised:\n%s", got)
	}
}

// A word wider than the room gets its own line and overflows, for the reason
// the runway line's clauses do: a cut through a value produces a shorter value
// rather than a visible signal that something did not fit. A path, a URL and an
// account label are all one word and all longer than a narrow terminal.
func TestAWordWiderThanTheTerminalOverflowsRatherThanBeingCut(t *testing.T) {
	const word = "/home/somebody/.local/share/ccdad/an-account-label-nobody-expected"
	got := view.WrapLabeled("Active:  "+word+" (work)", 30)
	if !strings.Contains(got, word) {
		t.Fatalf("the word was cut:\n%s", got)
	}
	if strings.Contains(got, "..") && !strings.Contains(word, "..") {
		t.Errorf("a truncation cue leaked in:\n%s", got)
	}
}

// Every writer that is not a terminal reports no width, and there the line has
// to come out exactly as its builder spelled it. So does a terminal too narrow
// to hold the label, where wrapping into no room produces a column of words.
func TestAnUnknownWidthLeavesALabelledLineExactlyAsItWas(t *testing.T) {
	line := "Hover:   on  (every threshold derived per account; 'ccdad hover status' prints the numbers in force)"
	for _, width := range []int{0, -1, 9, 4} {
		if got := view.WrapLabeled(line, width); got != line {
			t.Errorf("WrapLabeled(width=%d) =\n\t%q\nwant\n\t%q", width, got, line)
		}
	}
	// A line with no label at all is not something this block produces, but a
	// wrap that mangled one would be a silent way to find out.
	if got := view.WrapLabeled("no label here", 0); got != "no label here" {
		t.Errorf("an unlabelled line at width 0 = %q", got)
	}
}

// Columns, not bytes. Nothing these three builders spell is multi-byte today,
// which is exactly why this is pinned here rather than left to be discovered:
// the account label on the Active: line is whoever's mail address it is, and
// the notice text is whatever the notice said.
func TestALabelledWrapCountsDisplayColumnsAndNotBytes(t *testing.T) {
	// Six characters, twelve bytes, six columns: the label field's own width.
	const value = "ÄÖÜäöü"
	line := "Active:  " + value + " x"
	width := len("Active:  ") + ansi.StringWidth(value+" x")
	if len(line) == ansi.StringWidth(line) {
		t.Fatalf("the fixture is pure ASCII, so it rules nothing out: %q", line)
	}
	if got := view.WrapLabeled(line, width); got != line {
		t.Errorf("a line that fits by columns was wrapped:\n\t%q", got)
	}
}

// A rate under half a cent an hour is still spending, and this cell has to say
// so. It is the case no fixture reached: every credit figure written before it
// was a comfortable one, and the first rate ever measured against a live
// balance landed here -- an enterprise seat four hours past its billing
// rollover, two cents spent, 0.0026 USD/h.
//
// "%.2f" renders that "0.00", which puts a fleet reported spending nothing in
// the same row as a verdict naming the date it runs dry, and RunwayCreditBurn's
// own contract is that this cell never says a fleet spends nothing. The cell
// has to be readable as a rate at the width the rate needs.
func TestACreditRateUnderACentAnHourDoesNotReadAsZero(t *testing.T) {
	// The live measurement, written as what it was: one cent of spend over the
	// span between the readings that carried it.
	live := 0.01 / (3*time.Hour + 50*time.Minute + 52*time.Second).Hours()
	if got, want := view.RunwayCreditBurn(forecast.CreditFleet{
		Currency: "USD", SpendPerHour: live, Known: true,
	}), "0.0026 USD/h"; got != want {
		t.Errorf("RunwayCreditBurn(live sub-cent rate) = %q, want %q", got, want)
	}

	// The invariant behind that one figure, across the magnitudes a rate can
	// reach: a rate that got this far was measured and is positive, so the cell
	// must always carry a digit that says so.
	for _, rate := range []float64{live, 0.005, 0.0001, 1e-7} {
		got := view.RunwayCreditBurn(forecast.CreditFleet{
			Currency: "USD", SpendPerHour: rate, Known: true,
		})
		if !strings.ContainsAny(strings.TrimSuffix(got, " USD/h"), "123456789") {
			t.Errorf("RunwayCreditBurn(%v) = %q -- a measured rate rendered as zero", rate, got)
		}
	}

	// And the width money is written in is unchanged for every rate that fits
	// it, which is what keeps this from being a licence to widen the column.
	for _, c := range []struct {
		rate float64
		want string
	}{
		{1.4, "1.40 USD/h"},
		{0.01, "0.01 USD/h"},
		{12.345, "12.35 USD/h"},
	} {
		if got := view.RunwayCreditBurn(forecast.CreditFleet{
			Currency: "USD", SpendPerHour: c.rate, Known: true,
		}); got != c.want {
			t.Errorf("RunwayCreditBurn(%v) = %q, want %q", c.rate, got, c.want)
		}
	}
}

// A dry date a decade out is printed as a year, and the reason is the one this
// package can state: no rate here is measured over more than four hours, so a
// moment named to the minute a decade away claims a precision nothing produced
// it has. Measured 2026-09-01 on one live account: three readings ninety
// minutes apart put the credit dry date at 2046, 2037-02-26 09:49 and
// 2036-04-19 09:49, every one of them to the minute.
func TestADryDateBeyondAYearIsPrintedAsAYear(t *testing.T) {
	kst := time.FixedZone("KST", 9*60*60)
	now := time.Date(2026, 9, 1, 14, 14, 0, 0, kst)

	far := forecast.CreditFleet{
		Currency: "USD", SpendPerHour: 0.0051,
		DryAt: time.Date(2037, 2, 26, 4, 48, 0, 0, kst), Known: true,
	}
	if got, want := view.RunwayCreditVerdict(far, now, kst), "runs dry 2037  (in about 10 years)"; got != want {
		t.Errorf("RunwayCreditVerdict(far) = %q, want %q", got, want)
	}

	// Inside a year nothing changes, and that is most of what this repository
	// prints: forecast's horizon is fourteen days, so no window-axis verdict
	// has ever been a year out and none of them moves.
	near := forecast.CreditFleet{
		Currency: "USD", SpendPerHour: 1.4,
		DryAt: time.Date(2026, 9, 4, 9, 30, 0, 0, kst), Known: true,
	}
	if got, want := view.RunwayCreditVerdict(near, now, kst), "runs dry 2026-09-04 09:30 KST  (in 2d19h)"; got != want {
		t.Errorf("RunwayCreditVerdict(near) = %q, want %q", got, want)
	}

	// The threshold is a span of 365 days, and it is the same wording either
	// side of it: one function answers for both rows, so a date just inside it
	// still names its minute and a date just past it names its year.
	justInside := now.Add(364 * 24 * time.Hour)
	if got := view.RunwayCreditVerdict(forecast.CreditFleet{
		Currency: "USD", SpendPerHour: 1, DryAt: justInside, Known: true,
	}, now, kst); !strings.Contains(got, view.Timestamp(justInside, kst)) {
		t.Errorf("a date 364 days out = %q, want the full timestamp %q", got, view.Timestamp(justInside, kst))
	}
	justPast := now.Add(366 * 24 * time.Hour)
	if got, want := view.RunwayCreditVerdict(forecast.CreditFleet{
		Currency: "USD", SpendPerHour: 1, DryAt: justPast, Known: true,
	}, now, kst), "runs dry 2027  (in about a year)"; got != want {
		t.Errorf("a date 366 days out = %q, want %q", got, want)
	}
}

// The credit row's basis: what the figures above it were measured from. It is
// the sentence that lets a reader tell a rate divided out of six dollars from
// the same rate divided out of two cents, which no other cell on the page can
// say and which the window axes have always had.
func TestTheCreditBasisSaysWhatTheFigureWasMeasuredFrom(t *testing.T) {
	live := forecast.CreditFleet{
		Currency: "USD", SpendPerHour: 0.0051,
		Spent: 0.02, Observed: 3*time.Hour + 56*time.Minute + 45*time.Second, Readings: 27,
		Known: true,
	}
	if got, want := view.RunwayCreditBasis(live), "Measured from 0.02 USD spent over 3h56m, across 27 readings."; got != want {
		t.Errorf("RunwayCreditBasis = %q, want %q", got, want)
	}
	// A refused figure has no basis, and the caller prints nothing rather than
	// a sentence about a measurement that produced no answer.
	if got := view.RunwayCreditBasis(forecast.CreditFleet{}); got != "" {
		t.Errorf("RunwayCreditBasis(unknown) = %q, want the empty string", got)
	}
	// Not the zero value either: a refused figure that still carried a basis is
	// what a mutation dropping the Known check would produce.
	refused := forecast.CreditFleet{Currency: "USD", Spent: 6, Observed: 4 * time.Hour, Readings: 3}
	if got := view.RunwayCreditBasis(refused); got != "" {
		t.Errorf("RunwayCreditBasis(refused) = %q, want the empty string", got)
	}
}

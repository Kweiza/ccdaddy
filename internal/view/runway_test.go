// This file is package view_test rather than package view, and it is the only
// one in this directory that is. Timestamp exists so that the packages that
// render a moment spell one absolute layout once, so the test that matters is
// the one those packages can write: through the exported surface, with nothing
// unexported in reach. Its siblings stay in package view because they reach
// unexported state on Row.
package view_test

import (
	"strings"
	"testing"
	"time"

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
// one line all three render, and it sits between the verdicts and the basis:
// after what happened, before the evidence, which is the order the rest of the
// line is already in.
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
	want := "7d dry 2026-08-27 14:10 KST (2d6h)  ·  5h holds  ·  need 9 (4 more)  ·  basis 3h51m"
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

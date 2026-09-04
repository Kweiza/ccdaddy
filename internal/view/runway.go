package view

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/forecast"
)

// Timestamp is the one absolute-time rendering in this repository, and the zone
// is always in it. The two human-facing absolute formats that came before it
// carry neither a date nor a zone: internal/cli/hover.go's clock is "15:04",
// and internal/switcher/evaluate.go's Explain uses time.Kitchen. Both are fine
// for a moment inside the hour and useless for a runway that names one days
// out. Every remaining .Format in the tree is RFC 3339 or the daemon log's
// millisecond variant of it, which are machine formats and not an answer here.
// It lives in internal/view so the runway command, status, list and the
// dashboard spell one layout once rather than four times.
//
// The zone is not optional, and it is not read here. The arithmetic that
// produces these moments must not touch the environment, so the location
// arrives as a parameter. A nil location falls back to UTC rather than to
// time.Local: a caller that passed none has not told us its zone, and
// time.Local would print a confidently wrong hour on any machine whose TZ is
// not the reader's -- including CI, where nothing sets it.
func Timestamp(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("2006-01-02 15:04 MST")
}

// CompactTimestamp is the dashboard's narrow absolute date: YYMMDD hh:mm.
// The dashboard is local and interactive, so its space budget wins over the
// zone suffix retained by the command-line and JSON forms.
func CompactTimestamp(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("060102 15:04")
}

// CompactRunwayLines is the terminal dashboard's vertical runway summary. The
// command-line status keeps RunwayLine's compact horizontal form; a full-screen
// page gives each axis and each supporting fact a row of its own.
func CompactRunwayLines(f forecast.Fleet, now time.Time, loc *time.Location) []string {
	if !f.Basis.Known {
		return nil
	}
	parts := make([]string, 0, 5)
	if seg, ok := compactFleetSegment(f, now, loc); ok {
		parts = append(parts, seg)
	}
	parts = append(parts,
		compactAxisSegment("5h", f.FiveHour, now, loc),
		compactAxisSegment("7d", f.Weekly, now, loc),
		runwayBasis(f),
	)
	if seg := RunwayNeedSegment(f); seg != "" {
		parts = append(parts, seg)
	}
	return parts
}

func compactAxisSegment(label string, a forecast.Axis, now time.Time, loc *time.Location) string {
	switch a.Verdict {
	case forecast.VerdictRunsDry:
		if !a.HasDryAt {
			return label + " dry"
		}
		return fmt.Sprintf("%s dry %s (%s)", label, CompactTimestamp(a.DryAt, loc), HumanDuration(a.DryAt.Sub(now)))
	case forecast.VerdictHolds:
		return label + " holds"
	default:
		return label + " cannot tell yet"
	}
}

func compactFleetSegment(f forecast.Fleet, now time.Time, loc *time.Location) (string, bool) {
	holds := f.FiveHour.Verdict == forecast.VerdictHolds && f.Weekly.Verdict == forecast.VerdictHolds
	switch f.Both.Verdict {
	case forecast.VerdictRunsDry:
		if !f.Both.HasDryAt {
			return "", false
		}
		for _, a := range []forecast.Axis{f.FiveHour, f.Weekly} {
			if a.Verdict == forecast.VerdictRunsDry && a.HasDryAt && !f.Both.DryAt.Before(a.DryAt) {
				return "", false
			}
		}
		return compactAxisSegment("5h+7d", f.Both, now, loc), true
	case forecast.VerdictUnknown:
		if holds {
			return compactAxisSegment("5h+7d", f.Both, now, loc), true
		}
	}
	return "", false
}

// runwaySep is what separates the clauses of the runway line. The spaces are
// part of it: a middot with none reads as a decimal point at a glance, and this
// line sits under Daemon:, Active: and Current:, which are read at a glance.
const runwaySep = "  ·  "

// RunwayLine is the one-line summary `ccdad status` renders. The dashboard
// uses the same verdict helpers in CompactRunwayLines.
//
// It returns "" when there is nothing to say, and that empty string is load
// bearing: it is how both renderers know not to print a line at all, which
// is what keeps the dashboard's byte-compared golden fixtures from moving on a
// machine with no history behind it. A fleet with no basis gets no line, not a
// line saying it holds -- a promise resting on no reading is the one output
// this whole measurement exists to refuse.
//
// The line carries VERDICTS, not rates. A rate is per axis, two of them do not
// fit a line that also has to carry the answer, and the number a dashboard owes
// a reader is when rather than how fast. It also carries no percentage, which
// is not a style choice: `ccdad status`'s human output is asserted to contain
// no "0%", "20%", "30m", "ahead" or "five_hour" anywhere, because each is a
// figure belonging to a window its table is not reporting.
//
// The fleet run -- both axes burning at once -- appears only when it
// CONTRADICTS the two axes, which it can: it burns more windows and has more
// ways to end, so it can run dry, or run out of agreement between its two
// bands, while each axis alone holds. Printing it on every line would put a
// third verdict on a summary that already carries two and that agrees with them
// on every ordinary fleet; leaving it out when it disagrees would let "holds on
// both axes" stand over a fleet the arithmetic says empties.
//
// A fleet that cannot hold also carries how many accounts would -- see
// RunwayNeedSegment for why only that fleet does.
func RunwayLine(f forecast.Fleet, now time.Time, loc *time.Location) string {
	if !f.Basis.Known {
		return ""
	}
	fleetAgrees := f.Both.Verdict == forecast.VerdictHolds
	if fleetAgrees && f.FiveHour.Verdict == forecast.VerdictHolds && f.Weekly.Verdict == forecast.VerdictHolds {
		return "holds on both axes at this rate" + runwaySep + runwayBasis(f)
	}

	axes := []struct {
		label string
		axis  forecast.Axis
	}{{"5h", f.FiveHour}, {"7d", f.Weekly}}
	// The answer leads: a dry axis before one that holds, the earlier of two dry
	// ones first, and an axis that decided nothing last. A reader who stops
	// after the first clause has then read the worst of it.
	slices.SortStableFunc(axes, func(x, y struct {
		label string
		axis  forecast.Axis
	}) int {
		if r := verdictRank(x.axis.Verdict) - verdictRank(y.axis.Verdict); r != 0 {
			return r
		}
		if x.axis.HasDryAt && y.axis.HasDryAt {
			return x.axis.DryAt.Compare(y.axis.DryAt)
		}
		return 0
	})

	// Five: the fleet run, two axes, the basis and the seat count.
	parts := make([]string, 0, 5)
	if seg, ok := fleetSegment(f, now, loc); ok {
		parts = append(parts, seg)
	}
	for _, a := range axes {
		parts = append(parts, axisSegment(a.label, a.axis, now, loc))
	}
	// The basis first and the seat count LAST, which is not a reading order --
	// it is which clause the frame is allowed to eat. The dashboard cuts this
	// line from the right at its own width, and at the 80-column design target a
	// short fleet's line does not fit, so whatever is last is the casualty. The
	// evidence must not be: a verdict with no basis beside it is the one output
	// this measurement refuses to produce, and the dashboard prints the span
	// nowhere else on the page. The seat count is recoverable -- `ccdad runway`
	// prints it in full, with the spare case this clause never carries.
	parts = append(parts, runwayBasis(f))
	if seg := RunwayNeedSegment(f); seg != "" {
		parts = append(parts, seg)
	}
	return strings.Join(parts, runwaySep)
}

// runwayBreak is what a folded line ends on, and runwayGap is what rejoins two
// clauses that stayed together. Both are derived from runwaySep rather than
// spelled again: the separator's spaces are load bearing -- a middot with none
// reads as a decimal point -- and a copy of them here would go stale the first
// time that constant was tuned, silently, because the joined line and the
// folded one would still each look right on their own.
var (
	runwayBreak = strings.TrimRight(runwaySep, " ")
	runwayGap   = runwaySep[len(runwayBreak):]
)

// RunwayWrap folds the runway line onto width display columns, breaking only
// at its own separators and hanging every line after the first under the first
// clause.
//
// A width of zero means the caller does not know one -- a pipe, a redirect, the
// buffer a test renders into -- and returns the line exactly as it was joined,
// byte for byte. That is the case every non-terminal writer takes, and it is
// what keeps this function out of the way of everything that reads the output
// as data.
//
// The width arrives as a PARAMETER for the reason the zone does: nothing in
// this package may read the environment, and the terminal is environment. The
// caller nearest the reader measures it.
//
// Clauses are atomic. A clause wider than the room gets its own line and is
// allowed to overflow rather than be cut, because this line ends in an absolute
// moment and a span: `2026-08-26 08:1` reads as a shorter date, not as a line
// that did not fit. The dashboard cuts because a page has a right edge it
// cannot spend; a scrollback has one it can. That is the whole of why these two
// renderers of one wording answer this differently, and it is not drift.
//
// Every width here is a display column count. The separator is U+00B7 -- one
// column, two bytes -- so a byte count is wrong by the number of separators on
// the line, which is the number that grows as the line gets longer.
func RunwayWrap(label, line string, width int) string {
	clauses := strings.Split(line, runwaySep)
	room := width - ansi.StringWidth(label)
	if room <= 0 || len(clauses) < 2 {
		return label + line
	}

	// Each clause carries the separator that FOLLOWS it, so that the trailing
	// "  ·" of a line that breaks is inside the measurement that decided to
	// break there. Reserving it afterwards would let a line that just fit
	// overrun by exactly the marker that says it continues.
	tokens := make([]string, 0, len(clauses))
	for i, c := range clauses {
		if i < len(clauses)-1 {
			c += runwayBreak
		}
		tokens = append(tokens, c)
	}

	folded := []string{tokens[0]}
	for _, tok := range tokens[1:] {
		last := len(folded) - 1
		if ansi.StringWidth(folded[last])+ansi.StringWidth(runwayGap)+ansi.StringWidth(tok) <= room {
			folded[last] += runwayGap + tok
			continue
		}
		folded = append(folded, tok)
	}

	return hang(label, folded)
}

// fleetSegment is the both-axes-at-once answer, and it is reported only when
// the two axes do not already carry it: when nothing else says the fleet
// empties, or when the fleet empties before the earliest axis that does.
func fleetSegment(f forecast.Fleet, now time.Time, loc *time.Location) (string, bool) {
	holds := f.FiveHour.Verdict == forecast.VerdictHolds && f.Weekly.Verdict == forecast.VerdictHolds
	switch f.Both.Verdict {
	case forecast.VerdictRunsDry:
		if !f.Both.HasDryAt {
			return "", false
		}
		for _, a := range []forecast.Axis{f.FiveHour, f.Weekly} {
			if a.Verdict == forecast.VerdictRunsDry && a.HasDryAt && !f.Both.DryAt.Before(a.DryAt) {
				return "", false
			}
		}
		return axisSegment("5h+7d", f.Both, now, loc), true
	case forecast.VerdictUnknown:
		// Undecided is worth saying only against two axes that both claim to
		// hold. Beside an axis that already decided nothing it would repeat the
		// same doubt in a second place.
		if holds {
			return axisSegment("5h+7d", f.Both, now, loc), true
		}
	}
	return "", false
}

// verdictRank orders the three verdicts by how much a reader needs to see them
// first. It is a rendering order and deliberately not a method on Verdict:
// nothing outside this line has an opinion about which verdict outranks which.
func verdictRank(v forecast.Verdict) int {
	switch v {
	case forecast.VerdictRunsDry:
		return 0
	case forecast.VerdictHolds:
		return 1
	}
	return 2
}

// axisSegment is one axis's clause: its label and what the runs decided.
//
// A dry verdict with no moment cannot happen through forecast.Of, which sets
// both together, but it is rendered as the bare verdict rather than as a
// timestamp of the zero time -- a fleet dated to the year 1 is a worse answer
// than an undated one.
func axisSegment(label string, a forecast.Axis, now time.Time, loc *time.Location) string {
	switch a.Verdict {
	case forecast.VerdictRunsDry:
		if !a.HasDryAt {
			return label + " dry"
		}
		return fmt.Sprintf("%s dry %s (%s)", label, Timestamp(a.DryAt, loc), HumanDuration(a.DryAt.Sub(now)))
	case forecast.VerdictHolds:
		return label + " holds"
	default:
		return label + " cannot tell yet"
	}
}

// runwayBasis is the span the rates were measured over, which every surface
// prints beside the answer. A four-hour rate is a speedometer: twenty minutes
// of evidence and four hours of it support very different claims, and the
// reader is the one who has to weigh that.
func runwayBasis(f forecast.Fleet) string { return "basis " + HumanDuration(f.Basis.Observed) }

// The runway cells. `ccdad runway` draws two blocks of them and the axis block
// is read against the per-account block below it, so every glyph in both has to
// mean the same thing in both. They live here, beside RunwayLine, so a renderer
// that grows a third block later cannot spell "could not be read" a second way.
//
// Rates are one decimal place. The endpoint reports whole percents, so the
// second decimal of a rate derived from them is noise -- and a band whose ends
// differ by more than a tenth is already reported as a band, by the verdict
// rather than by the cell.

// RunwayBurn is a measured consumption rate, in percentage points per hour.
//
// Low is what is printed and High is not: the upper bound is what a claim of
// "holds" had to survive, so it belongs to the verdict cell rather than to this
// one, and printing an interval in a column that is meant to be added up would
// make the column unaddable.
//
// A band that cleared no gate is Unreadable and never 0.0. A measured zero, on
// the other hand, IS a reading -- the account was up, was polled, and did not
// burn -- and one live login at a time means most per-account rows carry one.
func RunwayBurn(b forecast.Band) string {
	if !b.Known {
		return Unreadable
	}
	return fmt.Sprintf("%.1f pp/h", b.Low)
}

// RunwayReplenish is what an axis gives back, in percentage points per hour. It
// always exists for a window axis: an axis with no accounts replenishes at
// zero, which is a true statement about an empty fleet rather than a reading
// nobody could take.
func RunwayReplenish(perHour float64) string { return fmt.Sprintf("%.1f pp/h", perHour) }

// RunwayCreditReplenish is the credit row's replenishment cell, and it is a
// function taking nothing because the answer never varies: the endpoint reports
// no renewal boundary for paid usage, so there is no quantity here to read.
// That is NoQuantity and not Unreadable -- a reader told "?" would go looking
// for a renewal rate that does not exist.
func RunwayCreditReplenish() string { return NoQuantity }

// RunwayVerdict is an axis's answer as a cell: whether the fleet holds at the
// measured rate, and when it does not, the moment it empties.
//
// An undecided axis renders Unreadable rather than either answer. The two runs
// of the band disagreeing means the evidence does not carry a claim, and both
// "holds" and a date would be claims.
func RunwayVerdict(a forecast.Axis, now time.Time, loc *time.Location) string {
	switch a.Verdict {
	case forecast.VerdictRunsDry:
		if !a.HasDryAt {
			// Unreachable through forecast.Of, which sets the verdict and the
			// moment together, and rendered as the bare verdict anyway: a fleet
			// dated to the year 1 is a worse answer than an undated one.
			return "runs dry"
		}
		return runsDryAt(a.DryAt, now, loc)
	case forecast.VerdictHolds:
		return "holds"
	default:
		return Unreadable
	}
}

// RunwayLeft is how much of an account's binding window is left, in percentage
// points and bare. No percent sign: the column is headed LEFT, it is summed
// into the fleet's points on the line above the table, and a percentage there
// would be read against the wrong window -- the one the account is REPORTED on
// elsewhere is not always this one.
func RunwayLeft(pct float64) string { return fmt.Sprintf("%.0f", pct) }

// RunwayRowCells is one account row's three weekly cells -- the window its
// points are counted on, the room left in it, and this account's own measured
// burn -- rendered together because they stand or fall together.
//
// An account whose response carried no weekly window at all gets NoQuantity in
// all three and not Unreadable. Nothing failed to be read: the account was
// read, and this quantity does not exist for it. Borrowing its five-hour window
// instead would put a five-hour room and a five-hour rate into a column that is
// summed into the weekly axis above it, and those are two different quantities.
func RunwayRowCells(r forecast.AccountRow) (window, left, burn string) {
	if !r.HasWindow {
		return NoQuantity, NoQuantity, NoQuantity
	}
	return string(r.Window), RunwayLeft(r.Left), RunwayBurn(r.Burn)
}

// RunwayEmpty is when the simulation first found one account out.
//
// "now" comes first, ahead of any recorded moment, because it is a fact about
// this minute rather than a projection: an account that is out already must not
// be rendered as a date the reader could decide to wait for.
//
// An account the run never found out is Unreadable. The run declining to empty
// an account is not the run promising it survives -- the axis may simply have
// held, or the band may have decided nothing.
func RunwayEmpty(r forecast.AccountRow, now time.Time, loc *time.Location) string {
	switch {
	case r.OutNow:
		return "now"
	case r.HasEmpty:
		return Timestamp(r.EmptyAt, loc)
	default:
		return Unreadable
	}
}

// RunwayCreditBurn is the fleet's paid spend, in the currency's major unit per
// hour with the code beside it. Money's own width, because the unit is money;
// the code, because amounts in two currencies do not add and a bare figure
// invites a reader to add them anyway. creditRate is where the width is
// decided, and why it is not always two decimals.
//
// A figure that could not be assembled is Unreadable, never zero. Every way it
// can fail -- mixed currencies, an uncapped account that is spending, no
// measured spend at all -- is a reason to say nothing rather than to report a
// fleet spending nothing, which would lengthen a runway made of money.
func RunwayCreditBurn(c forecast.CreditFleet) string {
	if !c.Known {
		return Unreadable
	}
	return fmt.Sprintf("%s %s/h", creditRate(c.SpendPerHour), c.Currency)
}

// creditRate writes a measured spend rate so that it cannot read as zero.
//
// Two decimals is the width money is written in and the right width for every
// rate a person would call a rate. It is not the right width at the low end,
// and the low end is not hypothetical: the first spend rate this repository
// ever measured against a live balance was 0.0026 USD/h -- an enterprise seat
// four hours past its billing rollover, two cents spent. "%.2f" prints that as
// "0.00", which is the one figure RunwayCreditBurn's contract says this cell
// must never carry: a fleet reported spending nothing, in the same row as a
// verdict naming the date it runs dry. The format string was producing the
// reading the comment above it forbids.
//
// A rate that reaches here was MEASURED and is positive -- creditFleet refuses
// a summed rate of zero or below before a CreditFleet is ever marked Known --
// so the fallback is reached only by a rate under half a minor unit an hour,
// and it is written with two significant digits. Not a bound and not an
// approximation sign: the figure is known exactly, and only its width was ever
// in question.
//
// The seam this leaves is deliberate. 0.0026 takes four decimals and 0.0053
// takes two, so two readings either side of half a cent an hour change the
// cell's width, and the narrower figure is the larger one. Both are correctly
// rounded at the width they use. Widening every rate to match would put four
// decimals on a figure in dollars an hour, which is a worse cell for every
// fleet that has one.
func creditRate(perHour float64) string {
	if s := strconv.FormatFloat(perHour, 'f', 2, 64); strings.ContainsAny(s, "123456789") {
		return s
	}
	return strconv.FormatFloat(perHour, 'g', 2, 64)
}

// RunwayCreditBasis is what the credit row was measured from, as a sentence:
// the money, the span it was spent over, and how many readings carried it.
//
// The window axes have had a basis line since they existed and the credit row
// has never had one, which was survivable only while no credit rate had ever
// been measured. The first one that was is the reason this exists: a dry date a
// decade out, correctly computed, from two cents. A reader cannot weigh that
// from the date, and the row has room for the one sentence that lets them.
//
// It is empty for a refused figure, and the caller prints nothing rather than a
// sentence about a measurement that did not happen. There is no singular arm
// for the reading count because minSamples is three and creditSpend refuses
// below it, so "1 reading" is a state no figure reaching here can be in.
func RunwayCreditBasis(c forecast.CreditFleet) string {
	if !c.Known {
		return ""
	}
	return fmt.Sprintf("Measured from %.2f %s spent over %s, across %d readings.",
		c.Spent, c.Currency, HumanDuration(c.Observed), c.Readings)
}

// RunwayCreditVerdict is when the fleet's paid balance reaches zero, or
// Unreadable when the figure was refused.
//
// It means something different from the two window rows above it and the
// command says so in prose beside the block: those ask whether resets replenish
// faster than the fleet spends, and this is a balance divided by a rate with
// nothing coming back.
func RunwayCreditVerdict(c forecast.CreditFleet, now time.Time, loc *time.Location) string {
	if !c.Known {
		return Unreadable
	}
	return runsDryAt(c.DryAt, now, loc)
}

// aYearOut is where a dry date stops being a moment and becomes a year.
//
// 365 days and not a calendar year: the comparison is against a SPAN, so a
// calendar year would put the threshold on a different side of itself depending
// on which side of a leap day now fell, and nothing being decided here is
// precise enough for that to be a distinction worth carrying.
const aYearOut = 365 * 24 * time.Hour

// runsDryAt is the one wording for "empties at this moment", so the axis rows
// and the credit row under them cannot phrase one answer two ways. The span in
// parentheses is what makes the date usable without arithmetic.
//
// Past aYearOut it names a YEAR instead of a moment. No rate this repository
// measures supports naming a minute a year away: rates are measured over
// history.MeasuredSpan, four hours, and the credit rate's numerator is whole
// minor units, so a fleet a few cents into its billing month divides two cents
// and one cent moves the answer by half. Measured 2026-09-01 on one live
// account, three readings ninety minutes apart: 2046, then 2037-02-26 09:49,
// then 2036-04-19 09:49. All three were printed to the minute, and the minute
// was the only part a reader could be sure of, because it came from `now`.
//
// This does not fork the wording, and it does not add a second absolute layout
// beside Timestamp -- see its comment, which says there is one. Naming a year
// is DECLINING to name a moment, which is a different act from naming one in
// another format, and the zone still decides which year it is, so loc is still
// read and still not taken from the environment.
//
// It also does not, in practice, reach the window rows: forecast's horizon is
// fourteen days and a simulated run stops there, so no axis verdict has ever
// been a year out. The rule is written for both anyway, because a rule that
// asks which row is calling it is how two rows start phrasing one answer two
// ways.
func runsDryAt(at, now time.Time, loc *time.Location) string {
	d := at.Sub(now)
	if d < aYearOut {
		return fmt.Sprintf("runs dry %s  (in %s)", Timestamp(at, loc), HumanDuration(d))
	}
	if loc == nil {
		loc = time.UTC
	}
	return fmt.Sprintf("runs dry %d  (in about %s)", at.In(loc).Year(), approxYears(d))
}

// approxYears is the parenthetical for a date past aYearOut. The hedge is the
// caller's word; this returns the quantity it hedges.
//
// It rounds the SPAN and not the difference between the two years, which is
// worth knowing because the two can look inconsistent side by side: from
// September 2026 a date in February 2037 prints as "2037  (in about 10 years)",
// and a reader adding ten to 2026 lands a year short. The span is right --
// 10.49 years -- and the year is right, and the line hands the reader the year
// so that nobody has to add anything. Rounding the year difference instead
// would trade this for a worse fault: a date 12 months out that happens to
// cross a new year would read "about 2 years".
func approxYears(d time.Duration) string {
	if y := int(math.Round(d.Hours() / (365 * 24))); y != 1 {
		return fmt.Sprintf("%d years", y)
	}
	return "a year"
}

// RunwayAccounts is the seat-count line under the axis block: how many accounts
// the run had to work with, how many it would take to survive the horizon at
// the measured rate, and the difference between the two.
//
// It answers the axis block's question from the other end. The rows above say
// how much is left and when it runs out; this says how many seats it would take
// for it not to. Both come from the same runs, so a reader can never be told
// "runs dry" and "you have enough accounts" on adjacent lines.
//
// The parenthetical is the actionable half and is present exactly when there is
// something to act on: a fleet already sitting on the smallest count that holds
// gets the count and nothing after it. The gap before it is two spaces, matching
// the dry moments in the block above, which a reader scans down the same column.
//
// An unmeasured count is Unreadable and never NoQuantity. Every fleet has a
// smallest size that holds -- supply is linear in the seat count and burn is not
// a function of the fleet -- so the number exists and what failed was the
// measuring. NoQuantity would say there is no such number and stop a reader
// looking for the history that produces it.
//
// A fleet with no usable accounts gets no line at all. "0 usable, ? needed" is a
// sentence about a fleet the rotation cannot reach, and the basis line above it
// already carries how many accounts there are and why none of them counted.
func RunwayAccounts(f forecast.Fleet) string {
	if f.AccountsUsable == 0 {
		return ""
	}
	head := fmt.Sprintf("%d usable, ", f.AccountsUsable)
	switch {
	case !f.HasNeeded:
		return head + Unreadable + " needed  (not enough history)"
	case f.NeededCapped:
		// The search reached its ceiling without finding a count that holds, so
		// the figure is a bound and is worded as one. Rendering it as a count --
		// with a parenthetical naming that many seats to buy -- would name a
		// purchase that does not fix the fleet, and a fleet already at or above
		// the ceiling would read as one that holds.
		return head + fmt.Sprintf("more than %d needed to hold at this rate", f.AccountsNeeded)
	}
	line := head + fmt.Sprintf("%d needed to hold at this rate", f.AccountsNeeded)
	switch {
	case f.AccountsNeeded > f.AccountsUsable:
		line += fmt.Sprintf("  (%d more)", f.AccountsNeeded-f.AccountsUsable)
	case f.AccountsNeeded < f.AccountsUsable:
		line += fmt.Sprintf("  (%d to spare)", f.AccountsUsable-f.AccountsNeeded)
	}
	return line
}

// RunwayNeedSegment is the seat count as one clause of RunwayLine, and it is
// carried only by a fleet that is SHORT.
//
// A fleet that holds already has its answer in the word "holds", and the slack
// it has on top of that is a block-level figure rather than a dashboard one: the
// summary line is read at a glance beside Daemon:, Active: and Current:, and a
// clause spent on good news is a clause the short case needed. The full form,
// spare count and all, is in RunwayAccounts.
//
// The parenthetical here is one space, not the two RunwayAccounts uses. The
// clauses of this line are already held apart by their own separator, and a
// second wide gap inside one of them would read as a third clause.
//
// RunwayLine's both-axes-hold branch does not call this, and it does not need
// to: a fleet whose runs all hold was answered by a search counting downward, so
// its count is at most its usable seats. A fleet that claimed both -- holding
// and short -- would be a contradiction the run cannot produce, and printing
// both halves of it on one line is not an improvement on printing one.
func RunwayNeedSegment(f forecast.Fleet) string {
	if !f.HasNeeded {
		return ""
	}
	if f.NeededCapped {
		// A bound still tells a short fleet's reader that the shortfall is not
		// one they can count, which is the thing this clause exists to say.
		return fmt.Sprintf("need more than %d", f.AccountsNeeded)
	}
	if f.AccountsNeeded <= f.AccountsUsable {
		return ""
	}
	return fmt.Sprintf("need %d (%d more)", f.AccountsNeeded, f.AccountsNeeded-f.AccountsUsable)
}

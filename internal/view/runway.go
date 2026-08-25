package view

import (
	"fmt"
	"slices"
	"strings"
	"time"

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

// runwaySep is what separates the clauses of the runway line. The spaces are
// part of it: a middot with none reads as a decimal point at a glance, and this
// line sits under Daemon:, Active: and Mode:, which are read at a glance.
const runwaySep = "  ·  "

// RunwayLine is the one-line summary `ccdad status`, `ccdad list` and the
// dashboard share, so one wording is spelled once rather than three times.
//
// It returns "" when there is nothing to say, and that empty string is load
// bearing: it is how all three callers know not to print a line at all, which
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

	parts := make([]string, 0, 4)
	if seg, ok := fleetSegment(f, now, loc); ok {
		parts = append(parts, seg)
	}
	for _, a := range axes {
		parts = append(parts, axisSegment(a.label, a.axis, now, loc))
	}
	parts = append(parts, runwayBasis(f))
	return strings.Join(parts, runwaySep)
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
// hour with the code beside it. Two decimals, because the unit is money; the
// code, because amounts in two currencies do not add and a bare figure invites
// a reader to add them anyway.
//
// A figure that could not be assembled is Unreadable, never zero. Every way it
// can fail -- mixed currencies, an uncapped account that is spending, no
// measured spend at all -- is a reason to say nothing rather than to report a
// fleet spending nothing, which would lengthen a runway made of money.
func RunwayCreditBurn(c forecast.CreditFleet) string {
	if !c.Known {
		return Unreadable
	}
	return fmt.Sprintf("%.2f %s/h", c.SpendPerHour, c.Currency)
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

// runsDryAt is the one wording for "empties at this moment", so the axis rows
// and the credit row under them cannot phrase one answer two ways. The span in
// parentheses is what makes the date usable without arithmetic.
func runsDryAt(at, now time.Time, loc *time.Location) string {
	return fmt.Sprintf("runs dry %s  (in %s)", Timestamp(at, loc), HumanDuration(at.Sub(now)))
}

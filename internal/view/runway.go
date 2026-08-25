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

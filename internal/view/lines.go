package view

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/release"
	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// HumanDuration renders a span at the scale a reader cares about. A reset
// already behind us is "due" rather than a negative number: the endpoint has not
// rolled the window over yet, which is a real state and not a clock error.
func HumanDuration(d time.Duration) string {
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

// DaemonLine is the liveness line `ccdad status` prints at the top of the
// dashboard. It is one of TWO wordings this repository ships, and the other is
// DescribeRunning below. They are not merged: this one leads a nine-column
// label field that the Active: and Mode: lines below it line up with, and the
// other is a fragment that reads as the tail of a sentence.
//
// The default arm is DaemonUnknown and every future value. "Cannot tell" is
// never folded into "no": a supervisor gating on that folding respawns forever
// on a filesystem where locks do not work.
func DaemonLine(report daemon.Report, now time.Time) string {
	switch report.State {
	case daemon.DaemonRunning:
		line := "Daemon:  running"
		if report.HasStatus && report.Status.PID != 0 {
			line += fmt.Sprintf("  pid %d", report.Status.PID)
		}
		if report.HasStatus && !report.Status.StartedAt.IsZero() {
			line += "  up " + HumanDuration(now.Sub(report.Status.StartedAt))
		}
		return line
	case daemon.DaemonStopped:
		return "Daemon:  not running  (start one with 'ccdad daemon start')"
	default:
		return "Daemon:  unknown  (the lock could not be probed)"
	}
}

// UpdateLine is the release line `ccdad status` prints under the daemon line
// and the terminal dashboard prints under its liveness block.
//
// The daemon publishes what it SAW -- a version string -- and never a verdict,
// so the comparison happens here, against the version of the binary doing the
// reading. That is not a preference: upgrading replaces the binary while the
// old daemon keeps publishing, so a boolean computed by a 0.6.1 daemon would
// tell a 0.7.0 CLI to upgrade to what it is already running.
//
// It is SILENT -- ok false -- for every reading it cannot turn into that
// sentence: no published document, nothing recorded, a recorded release that
// does not parse, a running version that does not parse (every `dev` build, so
// every build a developer makes), or a release that is not newer. A line that
// says nothing useful under a dashboard people read every day is worse than no
// line.
func UpdateLine(report daemon.Report, running string) (string, bool) {
	// No second guard for an empty UpdateLatest. ParseTag refuses it one line
	// down, so a clause for it would be one no test could ever turn red -- and
	// an untestable guard is how a reader is told a case is handled when it is
	// really handled somewhere else.
	if !report.HasStatus {
		return "", false
	}
	latest, ok := release.ParseTag(report.Status.UpdateLatest)
	if !ok {
		return "", false
	}
	current, ok := release.ParseTag(running)
	if !ok {
		return "", false
	}
	// Equal is silent as well as older. "You are on the newest release" is a
	// fact a dashboard does not need to repeat every time it is drawn.
	if latest.Compare(current) <= 0 {
		return "", false
	}
	// The releases page rather than a download URL, and through the same base
	// the rest of ccdad resolves releases against, so a machine pointed at a
	// mirror is not sent to the public one.
	return fmt.Sprintf("Update:  ccdad %s is out; this is %s  (%s)",
		latest, current, release.BaseURL()+"/latest"), true
}

// DescribeRunning is the one-line human form of a live daemon. now is a
// parameter rather than a call because nothing in this package reads a clock;
// package cli passes timeNow(), which is what the old body called.
func DescribeRunning(report daemon.Report, now time.Time) string {
	line := "running"
	if report.HasStatus && report.Status.PID != 0 {
		line += fmt.Sprintf(" (pid %d", report.Status.PID)
		if !report.Status.StartedAt.IsZero() {
			line += ", up " + HumanDuration(now.Sub(report.Status.StartedAt))
		}
		line += ")"
	}
	return line
}

// ModeLine is the ranking mode with the question it is asking. Recovery is the
// one a user needs told: the columns are identical to the ordinary case, so
// nothing else on the dashboard distinguishes "the engine is ranking by soonest
// reset because everything is spent" from "the engine has nothing to do".
//
// The label column is nine characters wide, matching the Daemon: and Active:
// lines above it. No branch may contain the substring "exhaust": the human table
// keeps the projection to --json, and TestTheProjectionIsJSONOnly fails on that
// word appearing anywhere in stdout.
func ModeLine(m strategy.Mode) string {
	switch m {
	case strategy.ModeRecovery:
		return "Mode:    recovery  (every account is over its threshold; empty accounts last, then soonest reset inside an hour, then slack)"
	case strategy.ModeConsumeFirst:
		return "Mode:    consume-first  (spending perishable weekly quota before it expires)"
	default:
		return "Mode:    headroom  (at least one account has room, or could not be read)"
	}
}

// HoverLine is the dashboard's one line about the fully automatic mode, and it
// is printed only when hover is ON.
//
// Absence is unambiguous here, which is what separates it from ModeLine: hover
// off is the default and the configured numbers are then the ones in force,
// whereas a missing Mode would have been defaulted to "headroom" -- a plausible
// answer nobody computed. What it must not be is silent while hover IS on. The
// Mode line reads "headroom" under hover because hover forced headroom, so a
// reader who configured consume-first would otherwise see a mode they never
// asked for with no reason for it anywhere on the page.
//
// It names 'ccdad hover status' rather than printing a number, because there is
// no single number to print: hover derives one per account and per window. The
// label column is nine characters wide, matching the Daemon:, Active: and Mode:
// lines it stacks with.
func HoverLine() string {
	return "Hover:   on  (every threshold derived per account; 'ccdad hover status' prints the numbers in force)"
}

// WrapLabeled folds one line of the labelled block -- Daemon:, Active:, Hover:,
// Mode: -- onto width display columns, breaking at spaces and hanging every
// line after the first under the value.
//
// Measured on an 80-column terminal against a live fleet: Mode: is 124 display
// columns in recovery and Hover: is 100 whenever hover is on, so the terminal
// folded both wherever its own right edge fell -- mid-word, mid-clause,
// sometimes inside a quoted command a reader was meant to type.
//
// It is a SEPARATE function from RunwayWrap, and the difference is not
// duplication. These lines are prose and break at spaces; the runway line is a
// row of values whose spaces are inside them, and a break at the one in
// `2026-08-26 08:19 KST` would produce a date that is not a date. Each knows
// where its own line may be cut, and neither may be pointed at the other's.
// What they share is the hanging alignment, which is `hang` below.
//
// The label is found rather than passed, because these lines arrive with it
// already spelled: the block's contract is a name, a colon, and padding to nine
// columns, and that is what this looks for. A line with no colon has no label
// and no indent, which is the honest rendering of a line this block did not
// produce.
//
// A width of zero -- every writer that is not a terminal -- returns the line
// exactly as its builder spelled it, and so does a terminal too narrow to hold
// the label, where there is no room to hang anything in.
func WrapLabeled(line string, width int) string {
	label, value := splitLabel(line)
	room := width - ansi.StringWidth(label)
	if room <= 0 || value == "" {
		return line
	}
	return hang(label, wrapWords(value, room))
}

// splitLabel divides a labelled line at the first colon and the padding that
// follows it. The padding goes with the LABEL: it is what the value is aligned
// to, so a continuation line is indented by exactly what the first line spent
// before its own first word.
func splitLabel(line string) (label, value string) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return "", line
	}
	j := i + 1
	for j < len(line) && line[j] == ' ' {
		j++
	}
	return line[:j], line[j:]
}

// wrapWords packs s onto lines of at most room display columns, breaking only
// between words.
//
// The whitespace between two words travels WITH the following word rather than
// being normalised, because these lines use it: `recovery  (every account...`
// separates the answer from the parenthetical explaining it, the same job the
// runway line gives its separator. A wrapper that rewrote runs of spaces as one
// would erase that distinction on every line that still fits, which is most of
// them.
//
// A word wider than the room gets its own line and overflows. Cutting it would
// turn a path, a URL or an account label into a shorter one that looks just as
// real -- the same reason the runway line's clauses are atomic.
func wrapWords(s string, room int) []string {
	var lines []string
	cur := ""
	for i := 0; i < len(s); {
		j := i
		for j < len(s) && s[j] == ' ' {
			j++
		}
		k := j
		for k < len(s) && s[k] != ' ' {
			k++
		}
		gap, word := s[i:j], s[j:k]
		i = k
		switch {
		case word == "":
			// Trailing whitespace belongs to no word, and a line ending in it
			// is a fold the reader pays for and cannot see.
		case cur == "":
			cur = word
		case ansi.StringWidth(cur)+ansi.StringWidth(gap)+ansi.StringWidth(word) <= room:
			cur += gap + word
		default:
			lines = append(lines, cur)
			cur = word
		}
	}
	return append(lines, cur)
}

// hang puts label on the first line and indents every line after it by the
// label's own display width, so the block reads as one value under one name
// rather than as several unnamed lines.
//
// It is shared by the two wraps in this package because the ALIGNMENT is the
// half they genuinely have in common; where each of them may break its line is
// the half that must stay apart.
func hang(label string, lines []string) string {
	indent := strings.Repeat(" ", ansi.StringWidth(label))
	for i := range lines {
		if i == 0 {
			lines[i] = label + lines[i]
			continue
		}
		lines[i] = indent + lines[i]
	}
	return strings.Join(lines, "\n")
}

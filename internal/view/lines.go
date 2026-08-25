package view

import (
	"fmt"
	"time"

	"github.com/Kweiza/ccdaddy/internal/daemon"
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

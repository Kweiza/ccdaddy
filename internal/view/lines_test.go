package view

import (
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// DaemonUnknown is the zero value by design and it is the DEFAULT arm rather
// than a case, so a DaemonState this binary has never heard of reads as
// "cannot tell" and never as "no".
func TestADaemonStateThisBinaryDoesNotKnowRendersAsUnknownAndNeverAsStopped(t *testing.T) {
	line := DaemonLine(daemon.Report{State: daemon.DaemonState(99)}, time.Time{})
	if !strings.Contains(line, "unknown") {
		t.Fatalf("DaemonLine(99) = %q, want it to read as unknown", line)
	}
	if strings.Contains(line, "not running") {
		t.Fatalf("DaemonLine(99) = %q: 'cannot tell' folded into 'no' makes a supervisor respawn forever", line)
	}
}

// The two wordings are two on purpose, and a future edit that merges them
// would silently change one of the two commands that print them.
func TestTheTwoDaemonWordingsStayTwo(t *testing.T) {
	report := daemon.Report{
		State:     daemon.DaemonRunning,
		HasStatus: true,
		Status: daemon.Status{
			PID:       4242,
			StartedAt: time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC),
		},
	}
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	dl := DaemonLine(report, now)
	dr := DescribeRunning(report, now)
	if dl == dr {
		t.Fatalf("DaemonLine and DescribeRunning render identically (%q); they are two wordings on purpose", dl)
	}
	if !strings.HasPrefix(dl, "Daemon:  running") {
		t.Errorf("DaemonLine = %q, want the nine-column label field status leads with", dl)
	}
	if strings.HasPrefix(dr, "Daemon:") {
		t.Errorf("DescribeRunning = %q, want a fragment with no label field", dr)
	}
	if !strings.Contains(dl, "4242") || !strings.Contains(dr, "4242") {
		t.Errorf("both wordings should still name the pid: DaemonLine=%q DescribeRunning=%q", dl, dr)
	}
}

// The label column is nine characters wide across every line the dashboard
// stacks, and Hover: joins Daemon:, Active: and Mode: in it. It is measured
// rather than asserted as a literal, because a line that sets its own width is
// the failure: one value out of the column reads as a different table.
func TestTheHoverLineStandsInTheSameLabelColumnAsTheRest(t *testing.T) {
	lines := map[string]string{
		"Daemon": DaemonLine(daemon.Report{State: daemon.DaemonStopped}, time.Time{}),
		"Mode":   ModeLine(strategy.ModeHeadroom),
		"Hover":  HoverLine(),
	}
	want := valueColumn(lines["Daemon"])
	for _, name := range []string{"Mode", "Hover"} {
		if got := valueColumn(lines[name]); got != want {
			t.Errorf("%s's value starts at column %d, want %d to match Daemon's: %q", name, got, want, lines[name])
		}
	}
}

// valueColumn is where a dashboard line's VALUE begins: past the colon, and past
// the padding that lines the column up.
func valueColumn(line string) int {
	colon := strings.Index(line, ":")
	if colon < 0 {
		return -1
	}
	rest := line[colon+1:]
	return colon + 1 + len(rest) - len(strings.TrimLeft(rest, " "))
}

// The line says where the derived numbers can be read. "on" by itself leaves a
// reader who has just seen a threshold they never set with nowhere to go, and
// the whole argument for handing the wheel over is that nothing is hidden.
func TestTheHoverLineSaysWhereTheDerivedNumbersCanBeRead(t *testing.T) {
	line := HoverLine()
	if !strings.Contains(line, "ccdad hover status") {
		t.Errorf("HoverLine() = %q, want it to name the command that prints the numbers in force", line)
	}
}

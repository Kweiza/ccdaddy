package view

import (
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/daemon"
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

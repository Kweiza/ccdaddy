package view

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
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

// The daemon publishes an observation and never a verdict, so the comparison
// lives here -- once, against THIS binary's version. A daemon-side boolean
// would be a 0.6.1 daemon telling a 0.7.0 CLI to upgrade to what it is already
// running, which is exactly the skew of an upgrade day.
func TestTheUpdateLineComparesAgainstTheRunningBinary(t *testing.T) {
	published := func(latest string) daemon.Report {
		return daemon.Report{
			State:     daemon.DaemonRunning,
			HasStatus: true,
			Status:    daemon.Status{SchemaVersion: 1, UpdateLatest: latest},
		}
	}
	for _, tc := range []struct {
		name    string
		report  daemon.Report
		running string
		want    bool
	}{
		{"a newer release is worth a line", published("0.7.0"), "0.6.1", true},
		{"the same version is not", published("0.7.0"), "0.7.0", false},
		{"an older recorded release is not", published("0.6.0"), "0.7.0", false},
		{"a dev build cannot be compared", published("0.7.0"), "dev", false},
		{"a recorded release that is not a version is not", published("latest"), "0.6.1", false},
		{"nothing recorded is silent", published(""), "0.6.1", false},
		// The Status is deliberately NOT the zero value. A report with no
		// document can still carry a stale struct beside HasStatus false, and
		// a fixture whose Status was empty would pass with the HasStatus guard
		// deleted -- the reading it fell back on would be unparseable anyway.
		{"no published document at all is silent", daemon.Report{
			State:  daemon.DaemonStopped,
			Status: daemon.Status{UpdateLatest: "0.7.0"},
		}, "0.6.1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line, ok := UpdateLine(tc.report, tc.running)
			if ok != tc.want {
				t.Fatalf("UpdateLine() ok = %v, want %v (line %q)", ok, tc.want, line)
			}
			if !ok {
				if line != "" {
					t.Errorf("a silent verdict still produced %q", line)
				}
				return
			}
			// Directional. Bare "0.7.0" and "0.6.1" are both still present if
			// the two versions are interpolated the wrong way round, which
			// inverts the sentence into telling the user to install what they
			// are already on.
			for _, want := range []string{"0.7.0 is out", "this is 0.6.1", "releases/latest"} {
				if !strings.Contains(line, want) {
					t.Errorf("UpdateLine() = %q, want it to name %q", line, want)
				}
			}
			// The nine-column label field the Daemon:, Active: and Mode: lines
			// lead with, so all four line up on one dashboard.
			if !strings.HasPrefix(line, "Update:  ") {
				t.Errorf("UpdateLine() = %q, want the nine-column label field", line)
			}
		})
	}
}

// "exhausted" is a value on the JSON wire -- daemon.StateExhausted is that
// spelling -- and the human table's rule is that the projection is where it
// stays. A Mode line using the word would put it back on the page it was kept
// off. That rule was held by a paragraph in lines.go and by whoever remembered
// it: the paragraph named a test that existed under no name at all, so adding
// the word changed nothing anywhere.
//
// It is asked TWICE, and neither question contains the other. The runtime half
// can only reach branches some Mode value selects, so a case added for a fourth
// constant is invisible to it until that constant is passed in. The source half
// reads every branch whether anything selects it or not, and cannot see a word
// that arrives through a helper rather than a literal.
//
// Case-insensitively, because "Exhausted" at the start of a clause is the same
// word on the same page.
func TestNoModeLineBranchSaysExhausted(t *testing.T) {
	const (
		forbidden    = "exhaust"
		file         = "lines.go"
		fn           = "ModeLine"
		wantBranches = 3
	)

	// Every mode this binary declares, plus one it has never heard of, which is
	// the default arm and the one a newer daemon's value would land on.
	for _, m := range []strategy.Mode{
		strategy.ModeHeadroom,
		strategy.ModeRecovery,
		strategy.ModeConsumeFirst,
		strategy.Mode(99),
	} {
		if line := ModeLine(m); strings.Contains(strings.ToLower(line), forbidden) {
			t.Errorf("ModeLine(%v) = %q; the human table keeps that word to the --json projection", m, line)
		}
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body *ast.BlockStmt
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == fn {
			body = fd.Body
		}
	}
	if body == nil {
		t.Fatalf("no func %s in %s, so this test now guards nothing", fn, file)
	}

	branches := 0
	ast.Inspect(body, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		branches++
		text, err := strconv.Unquote(lit.Value)
		if err != nil {
			text = lit.Value
		}
		if strings.Contains(strings.ToLower(text), forbidden) {
			t.Errorf("%s:%d: a %s branch says %q", file, fset.Position(lit.Pos()).Line, fn, text)
		}
		return true
	})

	// The count is pinned rather than floored, and that is the half of this
	// test that has to be argued for. It is not a fact about the code worth
	// recording; it is a tripwire. The way this rule came to be held by prose
	// in the first place was a branch added to a block without the test
	// covering that block growing with it, and a fourth mode arriving to a
	// silent green is that same commit again.
	if branches != wantBranches {
		t.Fatalf("%s has %d string branches, want %d: read the new one against the rule above, then move this count",
			fn, branches, wantBranches)
	}
}

package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// mixedFleet seeds one Claude account and one Codex one, each carrying only its
// own provider's windows. That is the shape the union used to spend columns on:
// four cells per row that could say nothing but "-".
func mixedFleet(t *testing.T) {
	t.Helper()
	seedAccount(t, "uuid-a", "work@example.com")
	seedUsageEntry(t, "uuid-a", usage.Entry{
		FetchedAt: statusNow,
		Snapshot: &usage.Snapshot{
			FiveHour: window(60, statusNow.Add(2*time.Hour)),
			SevenDay: window(30, statusNow.Add(48*time.Hour)),
		},
	})
	seedCodexAccount(t, "uuid-c", "cx@example.com")
	seedUsageEntry(t, "uuid-c", usage.Entry{
		FetchedAt: statusNow,
		Snapshot: &usage.Snapshot{
			CodexPrimary: window(20, statusNow.Add(3*time.Hour)),
		},
	})
}

// sectionOf is the lines of one section: the column names under its heading and
// every row after them, up to the next heading or the end of the table.
func sectionOf(t *testing.T, stdout, header string) []string {
	t.Helper()
	var out []string
	in := false
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 1 && (fields[0] == view.ClaudeSection || fields[0] == view.CodexSection) {
			in = fields[0] == header
			continue
		}
		if !in {
			continue
		}
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "windows ") {
			break
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		t.Fatalf("no %s section in:\n%s", header, stdout)
	}
	return out
}

// Each provider's half of the table draws its OWN windows. The union gave every
// Claude row a codex cell it could only fill with "-" and every Codex row a
// five-hour one, which is columns of table saying nothing.
func TestEachSectionDrawsOnlyItsOwnProvidersWindows(t *testing.T) {
	mixedFleet(t)
	code, stdout, _, top := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0 (%s)\n%s", code, top, stdout)
	}

	claude := sectionOf(t, stdout, view.ClaudeSection)
	if !strings.Contains(claude[0], "5H") || !strings.Contains(claude[0], "7D") {
		t.Errorf("the CLAUDE column names are %q, want its own windows", claude[0])
	}
	if strings.Contains(claude[0], "CX 1") {
		t.Errorf("the CLAUDE column names are %q; a codex window has no cell to fill there", claude[0])
	}

	codex := sectionOf(t, stdout, view.CodexSection)
	if !strings.Contains(codex[0], "CX 1") {
		t.Errorf("the CODEX column names are %q, want its own window", codex[0])
	}
	if strings.Contains(codex[0], "5H") || strings.Contains(codex[0], "7D") {
		t.Errorf("the CODEX column names are %q; a Claude window has no cell to fill there", codex[0])
	}
}

// STATE and AGE stay under one another across the seam. The two halves draw
// different numbers of quota columns, and a table that let the columns after the
// block slide would be two tables sharing a page.
func TestTheColumnsAfterTheQuotaBlockLineUpAcrossTheSeam(t *testing.T) {
	mixedFleet(t)
	code, stdout, _, top := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0 (%s)\n%s", code, top, stdout)
	}
	claude := sectionOf(t, stdout, view.ClaudeSection)[0]
	codex := sectionOf(t, stdout, view.CodexSection)[0]

	for _, name := range []string{view.StateHeader, view.AgeHeader, view.AccountHeader, view.TierHeader} {
		a, b := strings.Index(claude, name), strings.Index(codex, name)
		if a < 0 || b < 0 {
			t.Fatalf("%s is missing from one of the two heading rows:\n%s\n%s", name, claude, codex)
		}
		if a != b {
			t.Errorf("%s is at column %d under CLAUDE and %d under CODEX", name, a, b)
		}
	}
}

// One legend per section, each naming the provider it explains -- and naming it
// in lowercase, so a grep for the all-caps heading still finds a heading and
// nothing else. Three tests in this package assert the ABSENCE of a string by
// searching for it.
func TestEachSectionCarriesItsOwnLegend(t *testing.T) {
	mixedFleet(t)
	code, stdout, _, top := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0 (%s)\n%s", code, top, stdout)
	}

	var legends []string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "windows ") {
			legends = append(legends, line)
		}
	}
	if len(legends) != 2 {
		t.Fatalf("the report carries %d legends, want one per section:\n%s", len(legends), stdout)
	}
	if !strings.HasPrefix(legends[0], "windows claude: ") || !strings.Contains(legends[0], "5H = five_hour") {
		t.Errorf("the first legend is %q, want the Claude section's own windows", legends[0])
	}
	if !strings.HasPrefix(legends[1], "windows codex: ") || !strings.Contains(legends[1], "CX 1 = codex_primary") {
		t.Errorf("the second legend is %q, want the Codex section's own windows", legends[1])
	}
	if strings.Contains(legends[0], "codex_primary") {
		t.Errorf("the Claude legend %q names a window its section does not draw", legends[0])
	}
}

package cli

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"text/tabwriter"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
)

// tabwritten is the output text/tabwriter would have produced for the same
// table, under the exact configuration all seven call sites shared:
// NewWriter(w, 0, 0, 2, ' ', 0). It stays in this file after the last caller is
// gone, because "byte-identical to what shipped" is a claim that needs the
// thing it is compared against kept alive.
func tabwritten(headers []string, rows [][]string) string {
	var b bytes.Buffer
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	if len(headers) > 0 {
		fmt.Fprintln(w, strings.Join(headers, "\t"))
	}
	for _, r := range rows {
		fmt.Fprintln(w, strings.Join(r, "\t"))
	}
	if err := w.Flush(); err != nil {
		panic(err)
	}
	return b.String()
}

func rightTrimmed(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n")
}

// The five real column shapes, plus the seven that were argued about.
//
// padded marks a shape where tabwriter ends a line in spaces and columns does
// not -- the ONLY divergence between the two, and it is invisible on every
// terminal and to every assertion in this package. It is asserted in both
// directions: that tabwriter really does pad there (so the fixture cannot rot
// into a duplicate of the equal cases) and that columns is that output
// right-trimmed and nothing else.
//
// The empty-last-cell shapes are here because the claim that they could not
// arise was void: doctor's check literals are POSITIONAL, so there is no
// Detail: token a grep could have looked for, and a check{} whose third field
// is "" is a compiling row with an empty last cell.
func TestTheColumnHelperMatchesTheTabwriterItReplaces(t *testing.T) {
	cases := []struct {
		name    string
		headers []string
		rows    [][]string
		padded  bool
	}{
		{name: "list", headers: []string{"  IDX", "ACCOUNT", "TYPE", "TIER", "LEFT", "RESETS IN"}, rows: [][]string{
			{"* 1", "work@example.com (work)", "subscription", "max", "18%", "1h14m"},
			{"  2", "personal@example.com", "subscription", "pro", "83%", "4d3h"},
			{"  3", "ci@example.org (ci)", "api-key", "-", "?", "-  (disabled)"},
		}},
		{name: "status", headers: []string{"  IDX", "ACCOUNT", "TYPE", "USED", "WINDOW", "RESETS IN", "PACE", "AGE"}, rows: [][]string{
			{"* 1", "work@example.com (work)", "subscription", "82%", "five_hour", "1h14m", "ahead", "41s"},
			{"  2", "personal@example.com", "subscription", "17%", "seven_day", "4d3h", "on pace", "2m"},
			{"  3", "ci@example.org (ci)", "api-key", "?", "-", "-", "-", "?  (disabled)"},
		}},
		{name: "doctor", rows: [][]string{
			{"ok", "store", "/home/u/.ccdad"},
			{"warn", "keychain", "a stale item is still in the login keychain"},
			{"fail", "path", "ccdad is not on PATH"},
			{"skipped", "keychain", "there is no macOS Keychain on this platform"},
		}},
		{name: "config", headers: []string{"KEY", "VALUE", "SOURCE", "HOVER"}, rows: [][]string{
			{"threshold", "80", "default", "overriding"},
			{"cooldown", "5m0s", "file", "honoured"},
		}},
		{name: "hover", headers: []string{"  IDX", "ACCOUNT", "WINDOW", "ELAPSED", "UTIL", "THRESHOLD", "SLACK"}, rows: [][]string{
			{"* 1", "work@example.com", "five_hour", "85%", "98%", "100%", "+4"},
			{"  2", "personal@example.com", "seven_day", "-", "40%", "108%", "+69  (primary, metered in credits)"},
		}},
		{name: "runway axes", headers: []string{"  AXIS", "BURN", "REPLENISHES", "VERDICT"}, rows: [][]string{
			{"  5-hour", "12 points/h", "20 points/h", "holding"},
			{"  7-day", "3 points/h", "1 point/h", "empty Fri 14:00 KST"},
			{"  Credits", "-", "nothing comes back", "-"},
		}},
		{name: "runway rows", headers: []string{"  IDX", "ACCOUNT", "WINDOW", "LEFT", "BURN", "EMPTY"}, rows: [][]string{
			{"  1", "work@example.com (work)", "five_hour", "18%", "12 points/h", "Fri 14:00 KST"},
			{"  ?", "1e0f4b3a-0000-0000-0000-000000000000", "-", "?", "-", "-"},
		}},
		{name: "a 322-character cell", headers: []string{"KEY", "VALUE"}, rows: [][]string{
			{"detail", strings.Repeat("z", 322)},
			{"k", "v"},
		}},
		{name: "header only", headers: []string{"KEY", "VALUE", "SOURCE"}},
		{name: "one row", headers: []string{"KEY", "VALUE", "SOURCE"}, rows: [][]string{{"threshold", "80", "default"}}},
		{name: "empty middle cell", headers: []string{"KEY", "VALUE", "SOURCE"}, rows: [][]string{
			{"threshold", "", "default"},
			{"cooldown", "5m0s", "file"},
		}},
		{name: "empty last cell", headers: []string{"KEY", "VALUE", "SOURCE"}, rows: [][]string{
			{"threshold", "80", ""},
			{"cooldown", "5m0s", "file"},
		}, padded: true},
		{name: "every last cell empty", headers: []string{"KEY", "VALUE", "SOURCE"}, rows: [][]string{
			{"threshold", "80", ""},
			{"cooldown", "5m0s", ""},
		}, padded: true},
		{name: "leading and trailing spaces", headers: []string{"KEY", "VALUE"}, rows: [][]string{
			{"  indented", "trailing  "},
			{"k", "v"},
		}, padded: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got bytes.Buffer
			if err := columns(&got, c.headers, c.rows, nil); err != nil {
				t.Fatal(err)
			}
			want := tabwritten(c.headers, c.rows)
			if !c.padded {
				if got.String() != want {
					t.Fatalf("columns and tabwriter disagree:\n got %q\nwant %q", got.String(), want)
				}
				return
			}
			if got.String() == want {
				t.Fatalf("this shape is marked as one where tabwriter ends a line in "+
					"spaces, and it does not: %q", want)
			}
			if got.String() != rightTrimmed(want) {
				t.Fatalf("columns is not the tabwriter's output right-trimmed:\n got %q\nwant %q",
					got.String(), rightTrimmed(want))
			}
		})
	}
}

// The sanitizer, asserted rather than assumed. Without it the doctor row below
// is two lines tall and the second one carries no LEVEL, which reads as a
// second check that passed.
func TestTheColumnHelperFlattensEveryControlCharacterInACell(t *testing.T) {
	var got bytes.Buffer
	if err := columns(&got, []string{"LEVEL", "DETAIL"}, [][]string{
		{"fail", "open /x: no\nsuch\tfile\r"},
		{"ok", "store"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(got.String(), "\n"); lines != 3 {
		t.Fatalf("a cell with a newline in it made %d lines, want 3 (header and two rows):\n%q",
			lines, got.String())
	}
	if strings.ContainsAny(strings.TrimSuffix(got.String(), "\n"), "\t\r") {
		t.Fatalf("a tab or a carriage return survived into the layout: %q", got.String())
	}
	// Flattened, not truncated. Wrap(false) would have kept "open /x: no" and
	// thrown the rest away without saying so.
	if !strings.Contains(got.String(), "open /x: no such file") {
		t.Fatalf("the sanitizer discarded content instead of flattening it: %q", got.String())
	}
}

// The header arrives as table.HeaderRow and data rows are numbered from zero,
// which is the contract every StyleFunc in this package indexes its own slice
// by. A header prepended as row 0 would number the data rows from one and the
// last callback would read one past the end.
func TestTheColumnHelperNumbersDataRowsFromZeroWithTheHeaderApart(t *testing.T) {
	var seen []int
	if err := columns(&bytes.Buffer{}, []string{"A", "B"},
		[][]string{{"r0", "x"}, {"r1", "y"}},
		func(row, col int) lipgloss.Style {
			if col == 0 {
				seen = append(seen, row)
			}
			return lipgloss.NewStyle()
		}); err != nil {
		t.Fatal(err)
	}
	if len(seen) == 0 {
		t.Fatal("the style callback was never called")
	}
	for _, row := range seen {
		if row != table.HeaderRow && (row < 0 || row > 1) {
			t.Fatalf("the callback was handed row %d for a two-row table; rows seen: %v", row, seen)
		}
	}
}

func TestAnEmptyTableWritesNothingAtAll(t *testing.T) {
	var got bytes.Buffer
	if err := columns(&got, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got.Len() != 0 {
		t.Fatalf("an empty table wrote %q, and tabwriter wrote no byte at all", got.String())
	}
}

// The helper is the only table layout in this package. A leftover tabwriter is
// not a style question: it is an ANSI-blind measurer sitting next to a coloured
// one, and the next table written beside it inherits the 17-versus-38 failure
// columns exists to remove.
//
// The needle carries its quotes, so what is banned is the IMPORT and not the
// word. columns.go's own doc comment names text/tabwriter half a dozen times --
// it has to, because "byte-identical to the writer it replaces" is the whole
// argument for the file and the argument is not statable without naming the
// thing. A bare substring search would have made the rationale illegal to
// write down, which is the opposite of what this repository wants from a
// comment. columns_test.go is exempt outright because it genuinely imports the
// package: tabwritten() above is the comparison this claim rests on.
func TestNoTabwriterSurvivesInThisPackage(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || e.Name() == "columns_test.go" {
			continue
		}
		src, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(src, []byte(`"text/tabwriter"`)) {
			t.Errorf("%s still imports text/tabwriter; the table helper is columns()", e.Name())
		}
	}
}

package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Every decorative block is a hand-written constant and every one of them is
// 7-bit ASCII. The mockup's tagline is Korean and measures 26 runes but 37
// display columns; a terminal whose font lacks CJK double-width metrics
// misaligns the whole header block, and repository artifacts are English
// anyway.
func TestEveryChromeBlockIsAsciiAndRectangular(t *testing.T) {
	for name, block := range map[string][]string{"wordmark": wordmark, "tagline": tagline, "figures": figures} {
		if len(block) == 0 {
			t.Fatalf("%s is empty", name)
		}
		w := ansi.StringWidth(block[0])
		for i, line := range block {
			for _, r := range line {
				if r > 127 {
					t.Fatalf("%s row %d carries %q (U+%04X)", name, i, r, r)
				}
			}
			if got := ansi.StringWidth(line); got != w {
				t.Fatalf("%s row %d is %d columns wide, row 0 is %d: a ragged block breaks the frame", name, i, got, w)
			}
		}
	}
}

// The three measured sizes the height ladder is arithmetic over. If a block
// changes size, the ladder's row budget is wrong and nothing else would say so.
func TestTheChromeBlocksAreTheSizesTheLadderBudgetsFor(t *testing.T) {
	for _, tc := range []struct {
		name       string
		block      []string
		rows, cols int
	}{
		{"wordmark", wordmark, 5, 37},
		{"tagline", tagline, 2, 39},
		{"figures", figures, 6, 48},
	} {
		if len(tc.block) != tc.rows {
			t.Errorf("%s is %d rows, the ladder budgets %d", tc.name, len(tc.block), tc.rows)
		}
		if got := ansi.StringWidth(tc.block[0]); got != tc.cols {
			t.Errorf("%s is %d columns, the ladder budgets %d", tc.name, got, tc.cols)
		}
	}
}

// A bordered lipgloss box SOFT-WRAPS overlong content rather than truncating
// it: 40 characters inside Width(20) render as five rows. One footer a column
// too wide therefore costs a row and blows the height budget with no error.
func TestALineTooWideForTheFrameIsTruncatedAndNeverWrapped(t *testing.T) {
	got := truncate(strings.Repeat("x", 40), 20)
	if ansi.StringWidth(got) != 20 {
		t.Fatalf("truncate(40 chars, 20) is %d columns: %q", ansi.StringWidth(got), got)
	}
	if strings.Contains(got, "\n") {
		t.Fatal("truncate wrapped instead of cutting")
	}
}

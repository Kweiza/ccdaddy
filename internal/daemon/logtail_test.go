package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tailHome points the store at a fresh directory and returns the log path
// inside it, so every case below reads a file it wrote itself.
func tailHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CCDAD_HOME", home)
	return filepath.Join(home, LogFileName)
}

func writeLog(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A dashboard reading the log every ten seconds must not read an unbounded
// file. The bound is a seek, not a line count applied after the fact -- the
// difference shows up on a file far bigger than the window, where a reader
// that took the whole thing and then kept ten lines would have paid for all of
// it.
func TestTailLogReadsAtMostTheLastFewKilobytes(t *testing.T) {
	path := tailHome(t)

	var b strings.Builder
	// Well past the window, so anything the reader returns from the first half
	// of the file could only have come from reading the first half of it.
	for i := range 4000 {
		fmt.Fprintf(&b, "line %04d padded out to a realistic length for a daemon log entry\n", i)
	}
	writeLog(t, path, b.String())

	lines, err := TailLog(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 10 {
		t.Fatalf("TailLog(10) returned %d lines", len(lines))
	}
	if got, want := lines[9], "line 3999"; !strings.HasPrefix(got, want) {
		t.Fatalf("the last line is %q, want one starting %q", got, want)
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "line 0") {
			t.Fatalf("a line from the start of the file came back: %q -- the read is not bounded", line)
		}
	}
}

// The daemon rotates by renaming, so a reader that holds its handle goes on
// reading the renamed inode and loses every line after the first rotation --
// silently, forever. On Windows the held handle also blocks the rename and
// wedges rotation, because os.Open asks for FILE_SHARE_READ and
// FILE_SHARE_WRITE and not FILE_SHARE_DELETE.
func TestTailLogClosesTheFileAndSurvivesARotation(t *testing.T) {
	path := tailHome(t)
	writeLog(t, path, "before the rotation\n")

	if lines, err := TailLog(10); err != nil || len(lines) != 1 || lines[0] != "before the rotation" {
		t.Fatalf("first read = %v, %v", lines, err)
	}

	// The rotation itself: rename the file out of the way and start a new one.
	// A reader still holding the old handle would answer from the renamed
	// inode below, and would also have made this rename fail on Windows.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatalf("rotation failed, which is what a held handle does on Windows: %v", err)
	}
	writeLog(t, path, "after the rotation\n")

	lines, err := TailLog(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "after the rotation" {
		t.Fatalf("after a rotation TailLog read %v, want the new file's line", lines)
	}
}

// A machine where no daemon has ever run has no log, and that is the ordinary
// state of a fresh install rather than a failure.
func TestNoLogYetIsNotAnError(t *testing.T) {
	tailHome(t)
	lines, err := TailLog(10)
	if err != nil {
		t.Fatalf("TailLog on a machine with no log = %v, want nil", err)
	}
	if len(lines) != 0 {
		t.Fatalf("TailLog returned %d lines from a log that does not exist", len(lines))
	}
}

// An empty file is not a line. A reader that split it naively would report one
// blank entry, which reads on the screen as a log line nobody wrote.
func TestAnEmptyLogIsNoLinesRatherThanOneBlankOne(t *testing.T) {
	writeLog(t, tailHome(t), "")
	lines, err := TailLog(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Fatalf("an empty log produced %d lines: %q", len(lines), lines)
	}
}

// The bound truncates the FIRST line it lands in the middle of, and drops it
// rather than presenting half a line as a whole one.
func TestAPartialFirstLineIsDroppedRatherThanShownAsWhole(t *testing.T) {
	path := tailHome(t)

	// One line longer than the window, then a handful of ordinary ones. The
	// window opens somewhere inside the long line, so whatever the reader
	// keeps of it is a fragment.
	long := strings.Repeat("x", tailWindow+500)
	writeLog(t, path, long+"\nfirst\nsecond\nthird\n")

	lines, err := TailLog(10)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "x") {
			t.Fatalf("a fragment of the overlong line survived as a whole line: %q", line[:20])
		}
	}
	if got := strings.Join(lines, "|"); got != "first|second|third" {
		t.Fatalf("TailLog kept %q, want the three whole lines after the fragment", got)
	}
}

// A window that lands mid-file with no newline in it at all has nothing whole
// in it, and reports nothing rather than the fragment.
func TestAWindowWithNoWholeLineInItReportsNothing(t *testing.T) {
	writeLog(t, tailHome(t), strings.Repeat("y", tailWindow*2))
	lines, err := TailLog(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Fatalf("a window with no line break in it produced %d lines", len(lines))
	}
}

// A file that fits inside the window keeps its first line: the drop above is
// correct only because the window started mid-file, and applying it at offset
// zero would eat a real line every time.
func TestAShortLogKeepsItsFirstLine(t *testing.T) {
	writeLog(t, tailHome(t), "first\nsecond\n")
	lines, err := TailLog(10)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(lines, "|"); got != "first|second" {
		t.Fatalf("TailLog kept %q, want both lines", got)
	}
}

// Asking for nothing reads nothing. A screen that has no room for the block
// should not pay for the open.
func TestAskingForNoLinesReadsNothing(t *testing.T) {
	writeLog(t, tailHome(t), "first\n")
	if lines, err := TailLog(0); err != nil || lines != nil {
		t.Fatalf("TailLog(0) = %v, %v, want no lines and no error", lines, err)
	}
}

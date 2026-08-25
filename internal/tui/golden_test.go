package tui

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The seven pages, by the size that produced each. The size is in the name
// because it is the first thing a reader needs and the only thing that makes
// two of them different.
const (
	goldenFullPage     = "full-page-113x26.txt"
	goldenDesignTarget = "design-target-80x24.txt"
	goldenShort        = "short-80x13.txt"
	goldenNarrow       = "narrow-56x10.txt"
	goldenCollapsed    = "collapsed-43x9.txt"
	goldenNotice       = "notice-80x20.txt"
	goldenZeroAccounts = "zero-accounts-80x13.txt"
)

// update rewrites the pages under testdata from what the renderer produced.
//
// Regeneration had no mechanism before this: the seven pages were raw string
// literals in a test file, column-aligned by hand, and the only way to change
// one was to print the page and paste it back between two backticks. That is
// not a procedure, it is a dare -- and the change that motivated this one
// touches all seven at once.
//
//	go test ./internal/tui -run TestThePage -update -count=1
//
// The flag WRITES WHATEVER THE RENDERER SAID, including a page that is wrong.
// It is a transcription tool and never an oracle, and the discipline that makes
// it safe is reading the diff it leaves: a regeneration is reviewed like any
// other change to a file, because that is what it is.
var update = flag.Bool("update", false, "rewrite the golden pages under testdata from what the renderer produced")

// wroteGolden is what -update has already put in each file during this run.
//
// Two tests render the same page and compare it against the same file -- the
// ladder test and the one that proves an unclaimed forecast moves no golden --
// and without this the second would silently overwrite the first. Then a
// renderer that drew those two cases differently would leave a green suite and
// one file holding whichever page happened to be written last, which is the
// exact failure the second test exists to catch.
var wroteGolden = map[string]string{}

// checkGolden compares a rendered page against the file that holds it.
//
// The page is stripped of escape sequences before the comparison, and today
// that is a no-op: nothing in this package emits one yet, which the two
// escape-byte tests still assert directly. It is here because the colours
// arrive next and a fixture that carried SGR bytes would pin the exact
// truecolor spelling of every role, so that a palette change nobody meant to
// review would arrive as seven unreadable diffs. Stripping is what lets the
// goldens keep answering the question they were written for -- where does every
// character sit -- and leaves the question of which role was painted on which
// cell to the tests that ask it directly.
func checkGolden(t *testing.T, name, page string) {
	t.Helper()
	got := ansi.Strip(page)
	want := goldenWant(t, name, got)
	if got != want {
		t.Fatalf("%s:\ngot:\n%s\nwant:\n%s", name, got, want)
	}
}

// readGolden is the page on disk, for the one test that builds its expectation
// out of another page rather than out of a render.
func readGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading the golden page: %v", err)
	}
	return strings.TrimSuffix(string(b), "\n")
}

// goldenWant is the file's contents, or -- under -update -- the page that is
// about to become them.
//
// The trailing newline is added on write and taken off on read so the files are
// ordinary text a terminal can cat without eating the last line. Nothing else
// about the bytes is touched: several of these pages end their lines in
// SIGNIFICANT trailing spaces, because a page with no frame pads its rows to
// the terminal width, and an editor or a hook that trims whitespace on save
// will redden this suite. The line endings are the repository's own, held to LF
// on every platform by the top-level gitattributes rule, which is what keeps a
// Windows checkout comparing the same bytes a Linux one does.
func goldenWant(t *testing.T, name, got string) string {
	t.Helper()
	if !*update {
		return readGolden(t, name)
	}
	if before, seen := wroteGolden[name]; seen && before != got {
		t.Fatalf("two renders of %s disagree, so -update would keep only the last:\nfirst:\n%s\nsecond:\n%s",
			name, before, got)
	}
	wroteGolden[name] = got
	if err := os.WriteFile(filepath.Join("testdata", name), []byte(got+"\n"), 0o644); err != nil {
		t.Fatalf("rewriting the golden page: %v", err)
	}
	return got
}

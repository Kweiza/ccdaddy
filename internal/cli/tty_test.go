package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Everything that is not a terminal has no width, and the fold is off for all
// of it. Three shapes reach this in production and they fail the question in
// three different places: the buffer a test renders into is not a file at all,
// a redirect IS a file and is not a terminal, and a nil writer is neither.
//
// This is the half of outWidth that can be arranged anywhere. The half that
// reads a real terminal is exercised in tty_unix_test.go, on a pty, and on
// Windows it is not exercised at all -- there is no way to arrange one there
// that is cheaper than the thing being tested.
func TestOutWidthReportsNoWidthForEverythingThatIsNotATerminal(t *testing.T) {
	if got := outWidth(&bytes.Buffer{}); got != 0 {
		t.Errorf("outWidth(buffer) = %d, want 0", got)
	}
	// A redirect: `ccdad status > out`. os.Stdout IS this file, so a width read
	// off the process's terminal rather than off the destination would fold a
	// line into a file whose reader is somewhere else entirely.
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	if got := outWidth(f); got != 0 {
		t.Errorf("outWidth(regular file) = %d, want 0", got)
	}
	if got := outWidth(nil); got != 0 {
		t.Errorf("outWidth(nil) = %d, want 0", got)
	}
	if got := outWidth((*os.File)(nil)); got != 0 {
		t.Errorf("outWidth(typed nil file) = %d, want 0", got)
	}
}

// The width is measured on the FILE, and never on the writer a renderer paints
// through. This test exists to make the next person who decorates that writer
// fail here instead of shipping a fold that has quietly stopped happening.
//
// `ccdad status` and `ccdad list` both open with renderTarget, which hands back
// colorWriter's *colorprofile.Writer wrapped around cmd.OutOrStdout(). outWidth
// answers by asserting *os.File -- deliberately, see its own header -- and a
// wrapper is not one. Feed it the wrapper and the assertion fails, outWidth
// returns 0, and every labelled line goes out unfolded on a real 80-column
// terminal. Nothing else says so: there is no error, no changed byte in any
// fixture, and no failing test anywhere, because every other test of the fold
// stubs this seam and none of them ever looked at what it was handed. Measured
// against the shape a clean three-way merge of the two changes produces:
// `ccdad status` measured its width on *colorprofile.Writer at all five of its
// sites and `ccdad list` at its one, and `go test ./...` was green.
//
// IDENTITY with the file the root was given is what is asserted, not the
// weaker "it is some *os.File". The weaker question passes for a width read off
// os.Stdout while the command renders into a redirect, which is the other bug
// in this family and precisely the one outWidth's header argues it must not
// have.
//
// The seam is stubbed rather than left real because a temp file is not a
// terminal and the real seam correctly answers 0 for it. The question here is
// not what the width is, it is which writer arrives to be measured, and a stub
// that records its argument is the only way to see one.
//
// The rejected alternative was an Unwrap() io.Writer on colorWriter's type with
// outWidth following the chain, which would have made outWidth(out) correct and
// this test unnecessary. It is a general mechanism -- one every future
// decorator has to remember to implement, and one that would silently re-enable
// the fold at the wrong number for a wrapper that really does narrow the usable
// width -- introduced for two call sites in one package. A palette does not
// change how wide the terminal is.
func TestTheFoldMeasuresTheFileAndNotTheWriterItPaintsThrough(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedBurningFleet(t)

	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	var seen []io.Writer
	saved := outWidth
	t.Cleanup(func() { outWidth = saved })
	// A width, not 0. Zero is the answer that means "do not fold", and a
	// renderer is free to skip work behind it; 60 keeps every call site on the
	// path it takes on a real terminal.
	outWidth = func(w io.Writer) int {
		seen = append(seen, w)
		return 60
	}

	// Both commands, because they fold through two different helpers and the
	// collision this guards against reached each of them separately.
	for _, name := range []string{"status", "list"} {
		seen = nil
		root := NewRootCmd()
		root.SetOut(f)
		root.SetErr(io.Discard)
		root.SetArgs(explicitArgs([]string{name}))
		if code := ExecuteWith(root, io.Discard); code != ExitOK {
			t.Fatalf("ccdad %s exited %v", name, code)
		}
		if len(seen) == 0 {
			t.Fatalf("ccdad %s measured no width at all, so either the fold is gone or it "+
				"is no longer reached from here; this test has stopped guarding anything", name)
		}
		for i, w := range seen {
			if w != io.Writer(f) {
				t.Errorf("ccdad %s measured width %d of %d on a %T, want the *os.File the root was "+
					"given. outWidth asserts *os.File and returns 0 for everything else, so this "+
					"width is 0 on every terminal there is and the line goes out unfolded",
					name, i+1, len(seen), w)
			}
		}
	}
}

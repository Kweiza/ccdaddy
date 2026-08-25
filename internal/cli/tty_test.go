package cli

import (
	"bytes"
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

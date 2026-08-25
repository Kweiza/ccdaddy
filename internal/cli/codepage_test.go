package cli

import (
	"os"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/daemon"
)

// Which invocations consult the console output code page, through the seam --
// and startup does not, which is what keeps it out of the daemon.
//
// Nothing in the tree reads consoleUTF8 yet but this file. The glyph set that
// resolves `auto` against it lands next, and it will read it from tuiOptions,
// per render, behind the stdout-and-stdin gate. This test is the fence that
// goes up first, because the tempting place to put a second capability probe
// is beside the first one -- enableConsoleVT(os.Args[1:]) in Execute -- and
// that line runs for every invocation this binary has, the daemon's included.
//
// A daemon reading the code page would be a process with a redirected stdout
// and no glyph to draw asking a question about a window it does not own, and
// on the spawn path it would get a false and mean nothing by it. The zero
// asserted here is what stops that from being wired by accident.
//
// The consoleVT stub is not decoration: without it this test would widen the
// mode of whatever console `go test` was launched from.
func TestStartupNeverReadsTheConsoleOutputCodePage(t *testing.T) {
	var reads int
	savedCP := consoleUTF8
	t.Cleanup(func() { consoleUTF8 = savedCP })
	consoleUTF8 = func() bool { reads++; return true }

	savedVT := consoleVT
	t.Cleanup(func() { consoleVT = savedVT })
	consoleVT = func(*os.File) error { return nil }

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"bare ccdad", nil},
		{"ccdad status", []string{"status"}},
		{"the daemon", []string{daemon.RunArg}},
	} {
		reads = 0
		enableConsoleVT(tc.args)
		if reads != 0 {
			t.Errorf("%s read the console output code page %d times at startup, want 0: the code page is "+
				"a per-render question, and the daemon has no console to ask it about", tc.name, reads)
		}
	}
}

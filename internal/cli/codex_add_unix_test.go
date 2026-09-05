//go:build !windows

package cli

import (
	"strings"
	"testing"
)

// TestACshUserGetsTheLinesTheRefusalTellsThemToPaste is why the installer
// captures the sink's STDOUT as well as its stderr. ccdad does not write csh
// startup files — they share no syntax with the block it manages — so the
// refusal prints the lines to paste instead, and it prints them on stdout while
// the sentence pointing at them goes to stderr.
//
// Capture one and not the other and the sentence survives with nothing above
// it: "Add the lines above to ~/.cshrc." is then a remedy naming lines the user
// cannot see, printed to somebody whose codex is unrouted.
//
// It lives in a !windows file rather than skipping at run time like the other
// shim tests, because cshLine -- which is how it spells what the user must be
// shown, rather than writing that syntax out a second time here -- is compiled
// only where a csh startup file can exist.
func TestACshUserGetsTheLinesTheRefusalTellsThemToPaste(t *testing.T) {
	isolate(t)
	realShimAutoInstall(t)
	codexShimEnvironment(t)
	t.Setenv("SHELL", "/bin/csh")
	stubCodexDevice(t, ownerPayload, nil)

	code, _, stderr, top := runRoot(t, "add", "codex")
	if code != ExitOK {
		t.Fatalf("the csh refusal failed the add: exit %d, want %d\n%s%s", code, ExitOK, stderr, top)
	}
	line := cshLine(shimDir())
	pointer := "Add the lines above to ~/.cshrc."
	at, says := strings.Index(stderr, line), strings.Index(stderr, pointer)
	if says < 0 {
		t.Fatalf("the csh refusal never reached the user at all:\n%s", stderr)
	}
	if at < 0 {
		t.Fatalf("%q names lines the user was never shown; the line that registers %s is missing:\n%s",
			pointer, shimDir(), stderr)
	}
	// ABOVE, because that is the word the sentence uses. A line printed after it
	// is one the reader has already been told to look back for.
	if at > says {
		t.Errorf("the line to paste comes after the sentence that calls it \"above\":\n%s", stderr)
	}
}

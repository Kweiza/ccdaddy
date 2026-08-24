package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"
)

// lipgloss v2's Render() emits truecolor whatever the destination is: the v1
// global renderer that stripped it off a pipe is gone. One forgotten writer
// therefore puts escape bytes into every redirected invocation and every CI
// log, and nothing else in this repository would notice -- there is not one ESC
// byte in the tree today.
func TestAColouredRenderReachesABufferWithNoEscapeBytes(t *testing.T) {
	isolate(t)
	var buf bytes.Buffer
	w := colorWriter(&buf)
	if _, err := w.Write([]byte("\x1b[38;2;255;0;0mred\x1b[0m")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); strings.ContainsRune(got, 0x1b) {
		t.Fatalf("the writer passed an escape byte through to a buffer: %q", got)
	}
	if !strings.Contains(buf.String(), "red") {
		t.Fatalf("the writer ate the text along with the colour: %q", buf.String())
	}
}

// NO_COLOR is a user's explicit instruction and it is honoured on a terminal
// too, not only off one. TTY_FORCE simulates a terminal destination
// (colorprofile's own escape hatch for exactly this, since bytes.Buffer can
// never satisfy term.File) so the profile this resolves to would otherwise be
// colour-capable, and NO_COLOR must still floor it to ASCII.
//
// The control case exists because "profile ends up <= ASCII" is ambiguous by
// itself: it is equally what NO_COLOR flooring a colour-capable profile looks
// like AND what TTY_FORCE silently failing and falling back to NoTTY looks
// like. Running the identical TTY_FORCE+TERM setup with NO_COLOR genuinely
// unset first, and requiring a colour-capable profile there, is what makes the
// NO_COLOR=1 assertion below mean something rather than passing either way.
func TestNoColorStripsEvenWhereColourWouldOtherwiseBeAllowed(t *testing.T) {
	isolate(t)
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TTY_FORCE", "1")
	t.Setenv("CLICOLOR_FORCE", "")
	// COLORTERM=truecolor (exported by most modern terminal emulators, tmux
	// with Tc/RGB, and some IDE-integrated shells) would push the control
	// case's profile to TrueColor instead of ANSI256, so it is pinned
	// explicitly rather than left to whatever machine happens to run this.
	t.Setenv("COLORTERM", "")

	// Control: the same destination, with NO_COLOR genuinely unset (empty,
	// which colorprofile's strconv.ParseBool treats as false rather than as
	// NO_COLOR's own "present regardless of value" rule -- t.Setenv has no
	// unset, and empty is the value that actually reads as "not set" to this
	// library). Measured: this resolves to ANSI256 with COLORTERM pinned
	// above, but the property that actually matters is "colour-capable at
	// all", so the assertion checks that rather than the specific profile.
	t.Setenv("NO_COLOR", "")
	var control bytes.Buffer
	wc, ok := colorWriter(&control).(*colorprofile.Writer)
	if !ok {
		t.Fatalf("colorWriter did not return a *colorprofile.Writer: %T", colorWriter(&control))
	}
	if wc.Profile <= colorprofile.ASCII {
		t.Fatalf("control case (NO_COLOR unset): profile is %v, which carries no colour -- "+
			"TTY_FORCE did not force a colour-capable profile, so the NO_COLOR=1 "+
			"assertion below would pass even if NO_COLOR did nothing at all", wc.Profile)
	}

	// The same destination, with NO_COLOR=1, must be floored to ASCII.
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	w, ok := colorWriter(&buf).(*colorprofile.Writer)
	if !ok {
		t.Fatalf("colorWriter did not return a *colorprofile.Writer: %T", colorWriter(&buf))
	}
	if w.Profile > colorprofile.ASCII {
		t.Fatalf("NO_COLOR=1 left the profile at %v, which still carries colour", w.Profile)
	}
}

// The third probe, beside the two that already exist. It is a package var for
// the same reason they are: a real terminal is not something a test can
// arrange, and the decision hanging on this one is a REFUSAL -- the class that
// has to be exercised.
func TestStderrIsAProbeATestCanReplace(t *testing.T) {
	isolate(t)
	saved := stderrIsTTY
	t.Cleanup(func() { stderrIsTTY = saved })
	stderrIsTTY = func() bool { return true }
	if !stderrIsTTY() {
		t.Fatal("stderrIsTTY is not swappable, so the [A]dd refusal cannot be tested")
	}
}

//go:build unix

package cli

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// The terminal arm of outWidth, on a real one.
//
// Every other test of the fold stubs the seam, which is right -- they are about
// the RENDERING and a stub is how the rendering gets exercised at a width
// nobody has to have. But a stubbed seam leaves the measurement itself
// unexecuted: outWidth could return a constant, or read the size off the wrong
// descriptor, and every one of those tests would still pass. Measured: with the
// terminal arm short-circuited to 0, every other test in this package stays
// green.
//
// A pty master is a real terminal by every question outWidth asks -- tcgetattr
// answers on it and TIOCGWINSZ returns the size set here -- and it needs no
// child process, no window, and no CI capability beyond /dev/ptmx. Where that
// is not there (a container built without it), the test says so and skips
// rather than pretending to have measured something.
func TestOutWidthMeasuresARealTerminal(t *testing.T) {
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pty available on this machine, so the terminal arm is unmeasured here: %v", err)
	}
	t.Cleanup(func() { ptmx.Close() })

	// 137 rather than 80: a round number is what a wrong implementation would
	// most plausibly fall back to.
	const cols = 137
	if err := unix.IoctlSetWinsize(int(ptmx.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: 24}); err != nil {
		t.Skipf("this pty refused a window size, so there is nothing to read back: %v", err)
	}
	if got := outWidth(ptmx); got != cols {
		t.Errorf("outWidth(pty) = %d, want %d", got, cols)
	}
}

package cli

import (
	"io"
	"os"

	"golang.org/x/term"

	"github.com/Kweiza/ccdaddy/internal/daemon"
)

// stdoutIsTTY reports whether this process's stdout is a terminal. It is a
// package var, not a call, because a real terminal is not something a test can
// arrange — and the decisions hanging on it are refusals, which is exactly the
// class that has to be exercised.
//
// `ccdad export --full` refuses when this is true and no --out was given: the
// payload holds live refresh tokens, and a payload printed to a terminal is one
// scrollback buffer, one screen share and one `> backup.json` at the shell's
// umask away from being readable by everyone on the machine.
var stdoutIsTTY = func() bool { return isTTY(os.Stdout) }

// stderrIsTTY reports whether this process's stderr is a terminal.
//
// One decision hangs on it, and it is a refusal. The terminal dashboard's
// add-account key hands the terminal to a child `ccdad add claude`, and
// bubbletea gives that child os.Stderr -- not the program's own output -- as
// its stderr. `ccdad add claude` writes every line of its prose there: the
// login URL, the paste instructions, the re-prompt. Under `ccdad 2>/dev/null`
// the child would sit waiting for a code the user was never shown, behind a
// dashboard that had vanished. The key refuses instead, and names the redirect.
var stderrIsTTY = func() bool { return isTTY(os.Stderr) }

// outWidth is how many display columns w has, and 0 when it has none.
//
// It is measured on the WRITER rather than on os.Stdout, because the writer is
// what the answer is about: `ccdad status > file` and `ccdad status | less`
// both hand this a destination whose width is not the terminal's, and a
// redirect that folded its output to whatever the window happened to be would
// put a line break inside a file whose reader is elsewhere. A *bytes.Buffer --
// what every test renders into -- reports none for the same reason, which is
// what keeps the unfolded rendering the default everywhere it is read as data.
//
// Zero is also what an error reports. There is nothing a user could do with a
// failed ioctl and nothing ccdad would do differently: the line comes out as
// one, exactly as it did before any of this existed.
//
// A package var, not a call, for the reason stdoutIsTTY gives: a real terminal
// is not something a test can arrange, and the rendering that hangs on this is
// the one the item exists about.
//
// GetSize IS the terminal predicate here, and there is deliberately no isTTY
// ahead of it: a descriptor with no window size is not a terminal for the only
// purpose this function has. Measured -- with such a guard in place and then
// removed, every test in this package passes either way, including the one that
// hands this a regular file, because GetSize refuses that file too.
//
// The type assertion stays, and it is NOT the same case. Without it this would
// reach Fd() on a nil *os.File and depend on that returning an invalid
// descriptor rather than panicking. That is os.File's implementation and not
// its contract: no test here can see the difference, and a future standard
// library is under no obligation to keep it.
var outWidth = func(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	cols, _, err := term.GetSize(int(f.Fd()))
	if err != nil {
		return 0
	}
	return cols
}

// isTTY reports whether f is an interactive terminal.
//
// Two decisions hang on this. A non-TTY stdin cannot supply a pasted code, so
// the login waits on the loopback path alone and says so. A non-TTY stdout
// means bare `ccdad` prints usage instead of a dashboard, which is what keeps
// the non-interactive contract stable when the TUI later takes that slot.
func isTTY(f *os.File) bool {
	return f != nil && term.IsTerminal(int(f.Fd()))
}

// consoleVT is the platform half of the VT enable, as a seam. What it does is
// unobservable from a test on any platform — on Windows it changes a console
// this process did not create, everywhere else it does nothing — so the seam is
// what makes the POLICY around it testable: which invocations get the call.
var consoleVT = setConsoleVT

// enableConsoleVT gives this process's stdout ANSI escape processing, unless
// this process is the daemon. args is os.Args[1:].
//
// The VT enable is console only, never in the daemon, and the daemon is reached
// through exactly one argument: Spawn re-execs `ccdad <daemon.RunArg>`. It
// would in fact be a no-op through THAT path — Spawn points the child's stdout
// at os.DevNull, which has no console mode — but that is a fact about the spawn
// path rather than about this one, and a daemon started any other way (a
// supervisor, a developer, a future launcher) would inherit a console whose
// mode is none of its business.
func enableConsoleVT(args []string) {
	if len(args) > 0 && args[0] == daemon.RunArg {
		return
	}
	// The error is discarded because there is nothing a user could do with it
	// and nothing ccdad would do differently: a console that refuses the mode
	// prints escape sequences literally, which is exactly what it did before.
	_ = consoleVT(os.Stdout)
}

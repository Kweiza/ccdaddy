package cli

import (
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

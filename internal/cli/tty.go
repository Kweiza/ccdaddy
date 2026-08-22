package cli

import (
	"os"

	"golang.org/x/term"
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

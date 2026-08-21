package cli

import (
	"os"

	"golang.org/x/term"
)

// isTTY reports whether f is an interactive terminal.
//
// Two decisions hang on this. A non-TTY stdin cannot supply a pasted code, so
// the login waits on the loopback path alone and says so. A non-TTY stdout
// means bare `ccdad` prints usage instead of a dashboard, which is what keeps
// the non-interactive contract stable when the TUI later takes that slot.
func isTTY(f *os.File) bool {
	return f != nil && term.IsTerminal(int(f.Fd()))
}

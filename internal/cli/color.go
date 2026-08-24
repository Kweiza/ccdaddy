package cli

import (
	"io"
	"os"

	"github.com/charmbracelet/colorprofile"
)

// colorWriter is the one place in this binary that decides whether an
// invocation gets colour, and it is a package var so that the decision is
// testable rather than delegated to a library's reading of the environment.
//
// It exists because lipgloss v2's Render() emits truecolor unconditionally.
// The v1 global renderer that stripped colour off a pipe is gone, so a
// rendering handed straight to os.Stdout writes escape bytes into every
// redirected invocation and every CI log. Until this file, this repository
// emitted no escape byte at all.
//
// The profile writer downgrades to whatever the destination can carry, which
// for a bytes.Buffer or a redirected file is nothing. It reads NO_COLOR and
// CLICOLOR_FORCE from the environment it is handed, which is why os.Environ()
// is passed explicitly rather than left to a package-level default: the tests
// set those variables and have to be able to observe the effect.
//
// A live tea.Program does NOT go through here. Bubbletea owns the terminal for
// its own lifetime and takes its profile from the same environment; this is the
// one-shot path and the notices around it.
var colorWriter = func(w io.Writer) io.Writer {
	return colorprofile.NewWriter(w, os.Environ())
}

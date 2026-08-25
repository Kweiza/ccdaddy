package cli

import (
	"io"
	"os"

	"github.com/charmbracelet/colorprofile"
	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/theme"
	"github.com/Kweiza/ccdaddy/internal/tui"
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

// colourlessRoot is the annotation freshRootExec stamps on the root it builds,
// and reading it is how a renderer knows it is drawing into a tool result
// rather than onto a terminal.
//
// An annotation on the ROOT rather than a package var, because the property
// belongs to one command tree and not to the process: a `ccdad mcp` server runs
// a fresh root per tool call inside a process whose own stdout may well be a
// terminal, and a global would make the two invocations fight over one flag.
// Every renderer asks cmd.Root(), which cobra defines for every node.
const colourlessRoot = "ccdad.colourless"

// renderTarget is the writer a human rendering goes to and the palette it is
// painted with, resolved together because they are one decision and separating
// them is how a surface ends up with colour and no writer to strip it.
//
// The colourless branch is STRUCTURAL and looks at neither the writer nor the
// environment, which is the whole point of it. colorprofile gates its NO_COLOR
// early return on the destination being a terminal (its own env.go does the
// isatty test inside that branch), so NO_COLOR does not beat CLICOLOR_FORCE off
// one: measured, NO_COLOR=1 CLICOLOR_FORCE=1 TERM=xterm-256color into a
// bytes.Buffer resolves ANSI256 and writes "\x1b[38;5;173mX\x1b[m". Excluding
// the MCP server by name would therefore have excluded nothing -- an MCP client
// launched from a shell that exports CLICOLOR_FORCE would have got SGR bytes
// inside every tool result, and a JSON tool result is the one destination that
// cannot be asked whether it wanted them. Under the None theme every Style
// carries no Foreground at all, so the branch below emits no escape rather than
// emitting one and stripping it afterwards.
//
// The --json paths do not come through here. They write the document straight
// to cmd.OutOrStdout(), which is what keeps a machine-readable answer out of
// reach of any of this.
func renderTarget(cmd *cobra.Command) (io.Writer, theme.Palette) {
	if cmd.Root().Annotations[colourlessRoot] != "" {
		return cmd.OutOrStdout(), theme.Of(theme.None)
	}
	return colorWriter(cmd.OutOrStdout()), resolvePalette()
}

// resolvePalette is the configured theme, as a package var for the same reason
// colorWriter is one: the decision has to be observable from a test rather than
// delegated to a file on disk and a query to a terminal that no test has.
//
// A bad value in the file is not this function's failure to report. config.Load
// already validates tui.theme against theme.Names() and every caller of a
// renderer has already printed the notice its error carries; a palette that
// refused to resolve would turn a mistyped preference into a command that
// prints nothing.
//
// The file is read per invocation and not cached. These are one-shot commands
// that read the store, the usage cache and the daemon's status file already;
// one more small read is not what makes them slow, and a cache here would be a
// second copy of a value config.Load is the single source of.
var resolvePalette = func() theme.Palette {
	name := theme.Name(config.Defaults().TUITheme)
	if cfg, err := config.Load(); err == nil {
		name = theme.Name(cfg.TUITheme)
	}
	// The guard is a cost bound, not a shortcut: Pick's arguments are evaluated
	// before it is called, so passing darkBackground() unconditionally would
	// pay for the query on every theme including the three that ignore it.
	if name != theme.Auto {
		return theme.Of(name)
	}
	return theme.Pick(theme.Auto, darkBackground())
}

// darkBackground is the one-shot background query, and it is package tui's
// DarkBackground rather than a second query written on this side.
//
// It is a var so this package's tests can replace it, and it POINTS AT the
// other package's seam instead of duplicating it for a reason that outranks
// tidiness. The query puts stdin into raw mode with a deferred restore and
// blocks up to two seconds on a terminal that answers neither OSC 11 nor DA1.
// Two sync.OnceValues in two packages are two caches and two budgets: a process
// that rendered a listing and then a dashboard would pay four seconds, and a
// test in this package that forgot to stub its own private copy would take raw
// mode away from whoever was watching `go test ./internal/cli/` in a terminal.
//
// Every bound the query needs -- terminals on both ends of stdio, at most once
// per process, dark when it did not ask -- is stated where it is implemented,
// in internal/tui. Restating them here would be a second place for them to
// drift apart, and the drift would be invisible until somebody was staring at a
// grey page wondering which half had answered.
var darkBackground = tui.DarkBackground

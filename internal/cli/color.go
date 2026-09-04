package cli

import (
	"io"
	"os"

	"github.com/charmbracelet/colorprofile"
	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/theme"
)

// colorWriter is the one place in this binary that decides whether an
// invocation gets colour, and it is a package var so that the decision is
// testable rather than delegated to a library's reading of the environment.
//
// It exists because lipgloss v2's Render() emits truecolor unconditionally.
// The v1 global renderer that stripped colour off a pipe is gone, so a
// rendering handed straight to os.Stdout writes escape bytes into every
// redirected invocation and every CI log. When this file landed there was not
// one escape byte anywhere in the tree, which is what made the writer cheap to
// get right and impossible to notice; that is no longer the state of anything.
// The four surfaces that call renderTarget below -- list, status, doctor and
// the daemon summary -- and every screen of the dashboard now paint from
// internal/theme, so this writer is the whole reason
// `ccdad status > accounts.txt` is still a file of plain text.
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
// delegated to a file on disk.
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
//
// AUTO IS ANSWERED HERE AND NEVER ASKED, and the number is the whole argument.
// lipgloss's BackgroundColor loops over stdin AND stdout and runs both legs
// even when they are the same file -- there is no in == out guard to fall
// through -- at a two-second timeout each, so ONE call costs FOUR seconds on a
// terminal that answers neither OSC 11 nor DA1. Measured against a silent pty
// with a seeded store, with this function still asking: `list` 4.06 s, `status` 4.08 s, `doctor` 4.10 s,
// `daemon status` 4.07 s, against 0.08 s apiece once `tui.theme = none` takes
// the branch above. Halving it is not available, and caching it is worth
// nothing -- which is the trap this function was in. A sync.OnceValue is once
// per PROCESS, every one of these commands IS its own process, so the cache was
// filled and thrown away inside a single invocation that would otherwise have
// cost a tenth of a second. The rationale that shared it said as much and did
// not notice: it justified one cache over two because "a process that rendered
// a listing and then a dashboard would pay four seconds", which is the price of
// one query.
//
// So the rule the dashboard already lives by is taken here too: a default that
// is DEFINED beats a default that is awaited. The interactive page takes dark
// when a multiplexer eats the reply, the one-shot page takes dark when stdio is
// not a terminal, and a listing takes dark full stop. What that costs is a
// reader on a light terminal who never opens config.toml, and the cost is one
// line -- `theme = "light"` under `[tui]`, once per machine, and every listing
// is right for the life of it. What it buys back is every piped, scripted and
// CI invocation on an emulator that ignores the query: none of them was ever
// going to be shown a colour, and all of them paid four seconds for one.
//
// COLORFGBG would answer this for free on the terminals that export it, and it
// is deliberately not read. It is a new detection mechanism -- its own parsing,
// its own wrong answers on the terminals that set it stale, its own tests --
// and reaching for one to close a latency defect is how a fix turns into a
// feature nobody reviewed.
var resolvePalette = func() theme.Palette {
	name := theme.Name(config.Defaults().TUITheme)
	if cfg, err := config.Load(); err == nil {
		name = theme.Name(cfg.TUITheme)
	}
	if name != theme.Auto {
		return theme.Of(name)
	}
	// theme.Of and not theme.Pick, and the difference is not a matter of taste.
	// Pick's second argument is what a terminal ANSWERED, and no terminal is
	// being asked on this path; handing it a hardcoded true would read to the
	// next person as an answer somebody obtained cheaply, which is exactly the
	// impression that has to be impossible to form here.
	return theme.Of(theme.Dark)
}

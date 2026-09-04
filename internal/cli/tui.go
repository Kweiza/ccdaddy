package cli

import (
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/credhome"
	"github.com/Kweiza/ccdaddy/internal/theme"
	"github.com/Kweiza/ccdaddy/internal/tui"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// runProgram is tui.Run, as a package var beside stdoutIsTTY and consoleVT --
// the same idiom, for the same reason: a real tea.Program is not something a
// test can arrange, and the decision hanging on it (does bare `ccdad` launch
// one) is exactly the class that has to be exercised without actually
// launching one. runTui calls THIS, never tui.Run.
var runProgram = tui.Run

// runTui launches the dashboard after runBare has established that stdin and
// stdout are both terminals.
func runTui(cmd *cobra.Command, _ []string) error {
	// loadSnapshot writes every notice to cmd.ErrOrStderr() as well as into
	// the Snapshot it returns. A full-screen program owns the terminal, so it
	// renders the copy in Snapshot.Notices and silences the side channel.
	cmd.SetErr(io.Discard)
	return runProgram(tuiOptions(cmd))
}

// tuiOptions is everything package tui may not read for itself, built once so
// the two halves above cannot be handed differently-configured worlds.
//
// The executor is freshRootExec, from internal/cli/exec.go, and this file
// defines none of its own: the three refusals that seam exists to hold --
// never omit SetArgs, never omit SetOut/SetErr, never call cli.Execute() in a
// handler -- are stated once there, and a second copy here is exactly the
// drift internal/view.Exec was introduced to prevent.
//
// Load is loadSnapshot, called rather than reimplemented. Task V0 factored
// `status`'s read sequence out for this, and a second read order here would be
// a second chance to derive a number differently -- which json_contract_test's
// walk of the cobra tree would never see, because a renderer is not a command.
//
// The appearance crosses here UNRESOLVED, in three raw answers rather than one
// decision, and that is the sharpest rule this function holds. Two of them are
// the config file's own words carried across untouched, auto included; the
// third is a syscall about this process's attached console, which is the single
// appearance fact package tui cannot obtain for itself. Working out what auto
// means is that package's job, on the path it turns out to be on -- a live
// program learns it from a message and blocks for nothing, a one-shot render
// asks for it synchronously behind its own guard. Deciding it HERE would put
// the synchronous ask on the branch where both terminals are present, which is
// the live-program branch, and leave the redirected branch never asking.
//
// The config is read a second time here, and on purpose. loadSnapshot reads it
// too, but it reads it on every refresh tick from inside the event loop, and
// the appearance is fixed for the process's lifetime: a theme that changed
// under a running Program would repaint half a frame in the old palette with
// the other half already measured against the new one. Reading it once, at the
// moment the two halves are configured, is what makes the answer stable. Its
// notice is dropped because loadSnapshot's own call produces exactly the same
// sentence in the place the page already draws it from -- printing it twice
// would tell a user with an unreadable config file about it twice per tick.
func tuiOptions(cmd *cobra.Command) tui.Options {
	cfg, _ := configOrDefaults()
	return tui.Options{
		Load: func(now time.Time) (view.Snapshot, error) {
			// probeErr is dropped rather than folded into the error. It is the
			// daemon probe's own value, which `status --json` needs and a
			// rendering does not: loadSnapshot has already turned it into the
			// sentence in Snapshot.Notices, which is what the page draws.
			snap, _, err := loadSnapshot(cmd, now, false)
			return snap, err
		},
		Exec: freshRootExec(cmd),
		Now:  timeNow,
		// Out is where the ONE-SHOT render writes, and it is filled for both
		// halves because tui.Run ignores it: a live Program probes the
		// terminal it was given and makes the colour decision for itself,
		// which is why colorWriter's own header says a Program does not go
		// through here.
		Out:       colorWriter(cmd.OutOrStdout()),
		StderrTTY: stderrIsTTY(),
		// The [D] screen's credential-home warning, in the two pieces package
		// tui may not produce for itself: this process's own resolution, which
		// is an environment read, and the comparison, which is a filesystem
		// one. An unresolvable home is left empty, which omits the warning --
		// the honest answer for a caller that cannot answer.
		CredentialHome: credentialHomeOrEmpty(),
		SamePath:       credhome.SamePath,
		// The appearance, as three answers and no decisions. Theme and GlyphSet
		// are what the file said, converted but not resolved -- auto is a
		// legitimate value on both, and the ordinary one. ConsoleUTF8 is the
		// syscall answer, which is the only one of the three package tui could
		// not have obtained for itself.
		//
		// All three are read now, and by both halves of that package. glyphsFor
		// turns GlyphSet and ConsoleUTF8 into the vocabulary the frame, the
		// gauge and the state markers draw from; Theme reaches paletteFor on
		// the one-shot page and again in newApp, where the live program is
		// built. That they are read is what makes the paragraph above a rule
		// with something at stake rather than a description of three unused
		// fields.
		Theme:       configuredTheme(cfg.TUITheme),
		GlyphSet:    cfg.TUIGlyphs,
		ConsoleUTF8: consoleUTF8(),
	}
}

// credentialHomeOrEmpty is ccpath.CredentialHome with its error spent here.
//
// A home that cannot be resolved has nothing to compare, and the [D] screen's
// contract for that is an empty string rather than a sentence: it is one line
// on a screen full of them, and the ways this fails -- no home directory at all
// -- are already reported by every other row in `ccdad doctor` in words that
// say what to do about it.
func credentialHomeOrEmpty() string {
	home, err := ccpath.CredentialHome()
	if err != nil {
		return ""
	}
	return home
}

// configuredTheme is the theme name the config file spells, as a theme.Name.
//
// It converts and it does not resolve. theme.Auto goes in and theme.Auto comes
// out, which is the entire point of the seam this feeds: what auto MEANS is a
// question about the terminal, it has two different answers depending on which
// half of the dashboard is running, and this function is called before either
// half has been chosen.
//
// The one judgement it makes is what to do with a spelling internal/theme does
// not recognise, and the answer is auto -- not the zero Name, and the
// difference between those two is a whole page. The zero Name means "nobody
// said" downstream and paints nothing at all, so a typo that fell through to it
// would answer a mistyped config file with a permanently monochrome dashboard
// that never mentions why, on a machine where every other reading of the file
// is fine. Auto answers it with the default that saying nothing would have got,
// which is what a user who mistyped a value was reaching for. The validator
// refuses the typo where it is written through `ccdad config set`; this arm is
// for a file edited by hand afterwards, and for the day the validator and the
// parser disagree about a spelling -- which is exactly the day this must not
// turn into silence.
func configuredTheme(configured string) theme.Name {
	n, ok := theme.Parse(configured)
	if !ok {
		return theme.Auto
	}
	return n
}

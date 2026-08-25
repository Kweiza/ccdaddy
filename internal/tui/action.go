package tui

import (
	"fmt"
	"strings"

	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/theme"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// The three exit codes this package reads, spelled as literals because they
// belong to package cli's exit contract and package cli imports THIS package
// to register a command — the dependency cannot run the other way. They are
// named here so a reader does not have to recognise a bare 3.
//
// Exit 3 is "the world is already how you asked for it" and is NOT an error.
// Exit 4 is "wanted, but nothing viable to do it to", which is what the
// scoped-session refusal produces. Everything else carries its own number.
const (
	exitOK          = 0
	exitNothingToDo = 3
	exitBlocked     = 4
)

// pickerItem is one row of a picker: the words a user reads, and the argv that
// runs if they press enter on it. The two are declared together so a label can
// never drift away from the command it describes.
type pickerItem struct {
	label string
	argv  []string
}

// picker is the two-keystroke confirm every mutating key goes through. One
// keystroke never moves a credential: the key opens this, and enter chooses,
// and the thing that is about to happen is named in the same words the user
// will read afterwards before it happens.
type picker struct {
	title  string
	items  []pickerItem
	cursor int
	// current is the item already in force, or -1 when none is. It is marked
	// rather than hidden: "you are here" is the fact that makes the rest of
	// the list mean something.
	current int
	// glyphs is this page's vocabulary, carried so the picker's cursor is the
	// same character as the table's. Two lists that a user moves through with
	// the same keys and that point with two different characters is the drift
	// this whole set exists to remove.
	glyphs Glyphs
}

// switchPicker offers every account as a switch target, in the table's order.
//
// The argv carries the account's FULL uuid, never Idx and never a short
// prefix. Idx is a display ordinal that is recompacted when an account is
// removed — the root's own help says scripts must not key on it — and the
// shortest prefix the store resolves is eight characters, so an argv built
// from the number on the screen would either move a credential belonging to
// whoever now occupies that slot or resolve to nothing at all.
//
// A disabled account is still offered. Disabled holds an account out of
// AUTOMATIC rotation and is not a lock: an explicit switch still activates
// one, and hiding it here would make the dashboard disagree with the command.
func switchPicker(rows []view.Row, cursor int, g Glyphs) picker {
	p := picker{title: "Switch to which account?", current: -1, glyphs: g}
	for i, r := range rows {
		if r.Active {
			p.current = i
		}
		p.items = append(p.items, pickerItem{
			// The same form the table draws, for the same reason: this is the
			// name a user reads immediately before pressing the key that
			// moves the credential.
			label: r.ListLabel(),
			argv:  []string{"switch", r.Account.UUID},
		})
	}
	p.cursor = clampCursor(cursor, len(p.items))
	return p
}

// strategyPicker offers the strategies, built from the ranking package's own
// list so a strategy added there cannot be forgotten here.
//
// Hover is not on this key. It is a separate boolean with its own command, and
// folding it in would make one key write two settings. Nor is the engine Mode:
// headroom, recovery and consume-first read like a set of three, but recovery
// is an OUTCOME the ranking reports rather than a setting anyone chooses, and
// offering it would let a user ask for a state instead of a policy.
func strategyPicker(current string, g Glyphs) picker {
	p := picker{title: "Rank accounts by which strategy?", current: -1, glyphs: g}
	for i, name := range strategy.StrategyNames() {
		if name == current {
			p.current = i
		}
		p.items = append(p.items, pickerItem{
			label: name,
			argv:  []string{"config", "set", "strategy", name},
		})
	}
	p.cursor = clampCursor(p.current, len(p.items))
	return p
}

// Move walks the cursor and stops at the ends rather than wrapping.
//
// Wrapping is the friendlier default in a list nobody acts on. This is a list
// where enter moves a credential, and a held-down key that wraps past the
// bottom puts the highlight on an account at the top that the user was not
// looking at when they let go.
func (p picker) Move(delta int) picker {
	p.cursor = clampCursor(p.cursor+delta, len(p.items))
	return p
}

func clampCursor(at, n int) int {
	if at < 0 || n == 0 {
		return 0
	}
	if at > n-1 {
		return n - 1
	}
	return at
}

// Chosen is the argv the cursor is on, or nil when there is nothing to choose.
// An empty store is a real state — a fresh install — and it reaches here as a
// picker with no items rather than as a panic.
func (p picker) Chosen() []string {
	if p.cursor < 0 || p.cursor >= len(p.items) {
		return nil
	}
	return p.items[p.cursor].argv
}

// Body is the picker as a block of text, every line cut to the width it was
// given. Two markers, in two fixed columns so the labels line up: the cursor,
// and the value already in force.
//
// The cursor comes from the glyph set and the in-force mark does not, and the
// asymmetry is the same one the table makes. "Where is the highlight" is this
// screen's own fact and moves with the vocabulary; "which value is already in
// force" is the same claim the table's live marker makes about an account, and
// that one is spelled "*" wherever it appears in this binary.
//
// Each line is styled ON ITS OWN and never as a joined block, for the reason
// the page's chrome is: lipgloss right-pads a multi-line Render out to the
// widest line, which would put trailing space on every short label here and
// leave block's own truncation measuring something that is not text.
//
// The CUT comes afterwards, in block, and it is safe there rather than in spite
// of being there: block's truncate is ansi.Truncate, which measures display
// columns past the escape sequences and carries the opener across the cut. A
// width-blind cut would have to happen first; this one does not, and inverting
// the two would put the padding inside the styled run.
func (p picker) Body(width int, pal theme.Palette) string {
	lines := make([]string, 0, len(p.items)+2)
	lines = append(lines, pal.Style(theme.RoleHeader).Render(p.title))
	for i, it := range p.items {
		cursor, mark := "  ", "  "
		if i == p.cursor {
			cursor = p.glyphs.Cursor + " "
		}
		if i == p.current {
			mark = "* "
		}
		line := cursor + mark + it.label
		// "You are here" is the fact that makes the rest of the list mean
		// something, and it is the one line in this block a reader is comparing
		// everything else against.
		if i == p.current {
			line = pal.Style(theme.RoleAccent).Render(line)
		}
		lines = append(lines, line)
	}
	if len(p.items) == 0 {
		lines = append(lines, pal.Style(theme.RoleMuted).Render("    nothing to choose from"))
	}
	lines = append(lines, pal.Style(theme.RoleMuted).Render("enter  choose    esc  cancel"))
	return block(lines, width)
}

// result is one command's whole outcome: what it wrote, and what it exited
// with. Nothing else is kept, because nothing else is authoritative — the
// command's own bytes are the answer.
type result struct {
	Code           int
	Stdout, Stderr string
	Argv           []string
}

// run executes one ccdad command through the injected executor and keeps
// everything it wrote.
//
// It goes through a FRESH cobra root — which is what the injected function
// does — rather than calling an internal directly, and that is the whole
// design. The scoped-session refusal and the auto-start hook both live in the
// root's persistent pre-run and are keyed on the command path, so a key that
// called the switcher itself would write a session's soon-to-be-deleted
// credentials file and report a switch the live login never saw. It is also
// the only way the prose stays in one place, and the only way exit 3 goes on
// meaning "nothing to do" rather than "failed".
func run(ex view.Exec, argv []string) result {
	code, out, errOut := ex(argv)
	return result{Code: code, Stdout: out, Stderr: errOut, Argv: argv}
}

// Body is the command line that ran, what it exited with, and then everything
// it wrote, verbatim.
//
// It classifies ONLY by exit code and only into the three words the exit
// contract already uses. It never rewrites a message: a switch that changed
// nothing prints the displacement note in the command's own words, and this
// panel is where those words appear unedited. The panel does not know what a
// displacement is, and that is what keeps it from getting one wrong.
//
// The verdict is the only painted line. The command line above it is what the
// user asked for and the output below it is somebody else's bytes -- painting
// arbitrary captured stderr from a vocabulary this package chose would be this
// panel doing exactly the rewriting the paragraph above forbids.
func (r result) Body(width int, pal theme.Palette) string {
	lines := []string{
		"$ ccdad " + strings.Join(r.Argv, " "),
		pal.Style(r.verdictRole()).Render(r.verdict()),
	}
	lines = append(lines, textLines(r.Stdout)...)
	lines = append(lines, textLines(r.Stderr)...)
	return block(lines, width)
}

// verdict is the exit code in the contract's own words.
//
// Exit 3 is deliberately not a failure and is not worded as one: a dashboard
// that reported every non-zero code as a failure would tell a user something
// went wrong when the account they picked was simply the one already live.
// Every code the contract does not name carries its own number rather than a
// word this package invented for it.
func (r result) verdict() string {
	switch r.Code {
	case exitOK:
		return "done"
	case exitNothingToDo:
		return "nothing to do"
	case exitBlocked:
		return "blocked"
	}
	return fmt.Sprintf("exit %d", r.Code)
}

// verdictRole is the colour the word above carries, and it is a SEPARATE
// function from verdict for one reason: exit 3.
//
// Exit 3 is deliberately not a failure, so it takes neither the exhausted role
// nor the active one. A dashboard that painted every non-zero code red would
// tell a user something went wrong when the account they picked was simply the
// one already live, which is the commonest way this panel is opened at all;
// painting it green would claim something happened. Muted says what actually
// happened, which is nothing.
//
// Exit 4 and every code the contract does not name share the exhausted role.
// They are different facts and the WORD keeps them apart -- "blocked" against a
// bare number -- but they are the same answer to "did this work".
func (r result) verdictRole() theme.Role {
	switch r.Code {
	case exitOK:
		return theme.RoleActive
	case exitNothingToDo:
		return theme.RoleMuted
	}
	return theme.RoleExhausted
}

// textLines splits captured output into lines and drops the empty tail a
// trailing newline leaves behind. A command that wrote nothing contributes
// nothing, rather than a blank line that reads as output.
func textLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// block cuts every line to the width it was given and joins them.
//
// A command's stderr is arbitrary length and a bordered box soft-wraps content
// too wide for it rather than truncating, so one long refusal would cost as
// many rows as it needed and push the rest of the panel off the page.
func block(lines []string, width int) string {
	for i := range lines {
		lines[i] = truncate(lines[i], width)
	}
	return strings.Join(lines, "\n")
}

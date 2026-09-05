package tui

import (
	"fmt"
	"strings"

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
	// header marks a line that is DRAWN and never chosen -- a section heading
	// over the rows beneath it.
	//
	// The flag lives on the item rather than the list being nested, and the zero
	// value is what makes that safe: a picker built without ever mentioning
	// headings is a picker of choosable rows, so a list that has no sections is
	// untouched by their existence. A two-level structure would push Body, Move,
	// Chosen and the cursor clamp into two-level indexing and would force every
	// section-less picker to express itself as one anonymous section.
	header bool
	// inForce marks the item already in force. It is marked rather than hidden:
	// "you are here" is the fact that makes the rest of the list mean something.
	//
	// It is per ITEM and not one index on the picker, because there is more than
	// one such fact on a mixed fleet. Claude's live credential and Codex's
	// serving pointer are two different answers about two different accounts,
	// exactly one of each can hold, and a single index could only ever carry one
	// of them.
	inForce bool
}

// picker is the two-keystroke confirm every mutating key goes through. One
// keystroke never moves a credential: the key opens this, and enter chooses,
// and the thing that is about to happen is named in the same words the user
// will read afterwards before it happens.
type picker struct {
	title  string
	items  []pickerItem
	cursor int
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
//
// The list is SECTIONED by provider, under the headings the account table
// already draws, and it is grouped by the same function that table groups with
// so the two orders cannot come apart. Headings appear only where more than one
// section has rows: on a one-provider machine they would label nothing a reader
// has to tell apart, and they cost two of the rows the picker is drawn in.
//
// `at` is the account the page's cursor was standing on, BY UUID and never by
// index. Sectioning reorders, so a display position stops naming the same row --
// the picker would offer one account while the page underneath pointed at
// another. `serving` is the account the codex proxy serves new threads from, and
// it is a second in-force fact rather than a replacement for the first: Claude's
// live credential is still Row.Active, and both are marked.
func switchPicker(rows []view.Row, at, serving string, g Glyphs) picker {
	p := picker{title: "Switch to which account?", glyphs: g}
	secs := view.Sections(rows)
	filled := 0
	for _, s := range secs {
		if len(s.Rows) > 0 {
			filled++
		}
	}
	for _, s := range secs {
		if len(s.Rows) == 0 {
			continue
		}
		if filled > 1 {
			p.items = append(p.items, pickerItem{label: s.Header, header: true})
		}
		for _, line := range s.Rows {
			r := line.Row
			p.items = append(p.items, pickerItem{
				// The same form the table draws, for the same reason: this is the
				// name a user reads immediately before pressing the key that
				// moves the credential.
				label:   r.ListLabel(),
				argv:    []string{"switch", r.Account.UUID},
				inForce: r.Active || (serving != "" && r.Account.UUID == serving),
			})
		}
	}
	p.cursor = p.settle(clampCursor(p.indexOf(at), len(p.items)), 1)
	return p
}

// indexOf is the item carrying one account's uuid, or 0 when no item does.
//
// Zero rather than "not found" because every caller wants a cursor: a page whose
// cursor was on an account that has since gone opens the picker at the top,
// which is where a picker with nothing to restore opens anyway.
func (p picker) indexOf(uuid string) int {
	if uuid == "" {
		return 0
	}
	for i, it := range p.items {
		if len(it.argv) == 2 && it.argv[1] == uuid {
			return i
		}
	}
	return 0
}

// strategyPicker mirrors the four choices of `ccdad strategy`. Recovery is not
// one of them: it is a current engine outcome, not a policy a user selects.
//
// It appends nothing but choosable rows -- every item's header is the zero
// value -- so sections cost this list nothing. The cursor opens on the value
// already in force, tracked in a local rather than read back off the picker,
// because "which item is in force" is now a fact about each item and a list can
// hold more than one of them.
func strategyPicker(current string, g Glyphs) picker {
	p := picker{title: "Choose a switching strategy", glyphs: g}
	at := -1
	for i, name := range []string{"hover", "manual", "headroom", "consume-first"} {
		if name == current {
			at = i
		}
		p.items = append(p.items, pickerItem{
			label:   name,
			argv:    []string{"strategy", name},
			inForce: name == current,
		})
	}
	p.cursor = clampCursor(at, len(p.items))
	return p
}

// Move walks the cursor and stops at the ends rather than wrapping.
//
// Wrapping is the friendlier default in a list nobody acts on. This is a list
// where enter moves a credential, and a held-down key that wraps past the
// bottom puts the highlight on an account at the top that the user was not
// looking at when they let go.
//
// settle is what keeps the cursor off a section heading, and it is applied here
// rather than in Body: a heading the highlight rests on is not a drawing fault
// that redrawing fixes, it is a cursor pointing at nothing, and enter on it
// would be a keystroke that does nothing on the one screen where every keystroke
// is meant to.
func (p picker) Move(delta int) picker {
	p.cursor = p.settle(clampCursor(p.cursor+delta, len(p.items)), delta)
	return p
}

// settle moves an index onto a choosable item, preferring the direction of
// travel and falling back to the other.
//
// The fallback is what makes up from the first account stay on it: there is a
// heading above it and nothing above that, so a walk that only ever continued
// the way it was going would come to rest on the heading. Continuing the
// direction first is what keeps the ordinary case moving -- down off the last
// account of a section lands on the first account of the next, never back on the
// one it just left.
//
// Both walks are bounded by the list length and the input comes back when
// neither finds anything. A picker of nothing but headings cannot be built --
// switchPicker draws a heading only over rows it has -- so that answer is
// unreachable today; an unbounded search for the row it is looking for would
// hang the event loop rather than mis-draw a line, which is the wrong way round
// for a list nobody can leave.
func (p picker) settle(at, dir int) int {
	step := 1
	if dir < 0 {
		step = -1
	}
	for _, d := range [2]int{step, -step} {
		for i, walked := at, 0; walked < len(p.items); i, walked = i+d, walked+1 {
			if i < 0 || i >= len(p.items) {
				break
			}
			if !p.items[i].header {
				return i
			}
		}
	}
	return at
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
//
// A heading is refused on the TYPE and not merely by construction. A heading
// carries no argv, so the nil it already hands back would be the same answer;
// what the guard covers is the item the type can express and the constructor
// does not build — a heading WITH an argv — which would otherwise run a command
// off a line the user cannot put the cursor on.
func (p picker) Chosen() []string {
	if p.cursor < 0 || p.cursor >= len(p.items) {
		return nil
	}
	if p.items[p.cursor].header {
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
		// A heading takes the two marker columns as blanks so its text starts
		// where every label under it does, and it takes the table's heading role
		// so one thing is spelled one way twice on the same screen.
		if it.header {
			lines = append(lines, pal.Style(theme.RoleHeader).Render("    "+it.label))
			continue
		}
		cursor, mark := "  ", "  "
		if i == p.cursor {
			cursor = p.glyphs.Cursor + " "
		}
		if it.inForce {
			mark = "* "
		}
		line := cursor + mark + it.label
		// "You are here" is the fact that makes the rest of the list mean
		// something, and it is the one line in this block a reader is comparing
		// everything else against.
		if it.inForce {
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

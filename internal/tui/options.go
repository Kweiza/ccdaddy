package tui

import (
	"io"
	"time"

	"github.com/Kweiza/ccdaddy/internal/theme"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// Options is everything package cli injects. Nothing in package tui reads the
// environment, the clock or the filesystem except through one of these — which
// is what makes the whole page a pure function of data somebody else read, and
// therefore what makes every visual assertion in this package a string
// comparison rather than a pty.
//
// The fields are declared here in full, once, even though the two-shot render
// below uses only three of them: the event loop that arrives later adds
// METHODS to Model and reads the rest of these, and a field added to a struct
// three files already read is a merge conflict in the one place this package
// cannot afford one.
//
// The appearance trio -- Theme, GlyphSet and ConsoleUTF8 -- is the first group
// here that carries an UNRESOLVED value, and the distinction it draws is worth
// stating plainly, because the paragraph above is easy to over-read. What a
// caller may not do is READ for this package; what this package may not do is
// read for itself. Neither of those forbids this package from DECIDING. "auto"
// is a decision, not a read. The two facts it is decided from -- what the file
// said, and what this process's console is -- are reads, which is why those
// arrive through here and the decision does not.
//
// Resolving the pair in package cli instead would cost two things that no later
// commit could get back. The width-engine fallback -- the arm that answers
// ascii for a terminal whose width tables have been switched to the east-asian
// ambiguous widths -- lives in this package's glyph picker and nowhere else, so
// a caller that collapsed "auto" to a set name first would leave that arm
// unreachable in every shipping binary while its own tests went on passing. And
// the background-colour question is answered on two different paths in here: by
// the event loop for a live program, as a message the terminal sends back in
// its own time, and by the one-shot render for a redirected one, synchronously
// and at a price. A caller that answered it once, before it knows which of the
// two paths it is about to take, necessarily answers it on the wrong one.
type Options struct {
	// Load is one complete read of the documents the dashboard renders. It
	// takes the clock rather than reading it, so a caller can pin time.
	//
	// A failed Load is not fatal and must not empty the table — see
	// Model.AfterLoad, which is the rule that says what happens instead.
	Load func(now time.Time) (view.Snapshot, error)
	// Exec runs one ccdad command through a FRESH cobra root. Every key that
	// changes anything goes through here rather than calling an internal, so
	// the scoped-session refusal, the wording and the exit contract all stay
	// in one authority.
	Exec view.Exec
	// Now is the clock. It is a function value because nothing in this package
	// may call time.Now itself.
	Now func() time.Time
	// Out is where the one-shot render writes.
	Out io.Writer
	// StderrTTY is whether stderr is a terminal, probed by package cli beside
	// the two probes that already exist there. It gates the add key: the login
	// writes every line of its prose to stderr, and bubbletea hands a released
	// child os.Stderr rather than the Program's own output, so under a
	// redirect the prompt would vanish while the dashboard sat blank.
	StderrTTY bool
	// CredentialHome is THIS process's own resolution of the Claude Code
	// credential home, for the [D] screen to compare against the one the
	// daemon published. A daemon started from a shell that resolved a
	// different one manages that directory for the rest of its life and every
	// other file on the machine looks normal; comparing the two is the only
	// way anyone finds out.
	//
	// Empty is a legitimate value -- a caller that could not resolve one --
	// and it omits the warning rather than guessing.
	CredentialHome string
	// SamePath reports whether two spellings name one directory.
	//
	// It is injected rather than called, and that is not ceremony. ccdad
	// manufactures the two spellings of that path itself: daemon.ChildEnv pins
	// an absolute, symlink-resolved one into every daemon it spawns, while a
	// shell's own spelling comes back untouched, so a trailing slash or a
	// symlink is enough to make two names for one directory. Answering it
	// honestly therefore means asking the FILESYSTEM, which is exactly what
	// nothing in this package may do. `doctor` asks the same question and
	// reaches internal/credhome for the answer; package cli hands that same
	// function down here.
	//
	// nil means the caller cannot compare paths, which omits the warning for
	// the same reason an empty CredentialHome does.
	SamePath func(a, b string) bool
	// Theme is which palette the page paints with, AS CONFIGURED. theme.Auto is
	// a legitimate value across this seam, and is in fact the one this field
	// usually carries, since auto is the documented default.
	//
	// Auto is not resolved by the caller because it has two different answers
	// in here rather than one. A live program asks by putting a background
	// request into the batch its event loop already sends and handling the
	// reply as a message, blocking for nothing. A one-shot render asks
	// synchronously and pays the full price: raw mode on stdin, a request
	// written to stdout, and up to FOUR seconds of waiting on a terminal that
	// answers neither that request nor the identity one it falls back to -- the
	// library runs the whole query twice, once per stdio end, two seconds a
	// leg, whether or not the two ends are the same file. Those
	// prices are only payable by whoever knows which path is running, and the
	// caller building this struct does not: it builds one Options and hands it
	// to whichever half the terminals turn out to justify.
	//
	// The ZERO Name is a different value from theme.Auto, and no caller that
	// read a config file produces it. It means nobody said, and nobody-said
	// paints nothing at all -- which is the answer this package's own
	// internally constructed Models need, and the reason this field is a name
	// rather than a Palette: a Palette has no spelling for "unset" that a test
	// can compare, and a name does.
	Theme theme.Name
	// GlyphSet is which vocabulary the frame, the gauge and the state markers
	// draw from, AS CONFIGURED: "unicode", "ascii" or "auto", and auto is the
	// default, so auto is the ordinary case rather than the edge one.
	//
	// Two questions have to be answered to resolve it and only one of them
	// belongs to the caller: whether this process's console can carry the
	// bytes, which is ConsoleUTF8 below, and whether the width tables linked
	// into THIS binary agree with the terminal about how wide those bytes are.
	// The second is a property of the width engine this package draws through,
	// read from an environment variable that engine interprets for itself, and
	// a caller that pre-resolved auto would be deciding with half the evidence
	// -- shipping a Unicode frame whose own measurements are wrong by two
	// columns to precisely the users who configured for it.
	//
	// A spelling the picker does not recognise is treated as auto rather than
	// refused. The config validator rejects it where it is written; a file
	// edited by hand afterwards reaches a running dashboard, and a page that
	// renders is worth more than a page that is right about a typo.
	GlyphSet string
	// ConsoleUTF8 is whether this process's console can carry UTF-8, probed by
	// package cli beside the two terminal probes that already live there.
	//
	// It is the one appearance FACT this package genuinely cannot find out for
	// itself, and the reason is Windows. There the answer is the output code
	// page of the console this PROCESS is attached to -- a property of the
	// process, read through a call that takes no handle at all -- so there is
	// no writer in here to interrogate and no descriptor to hand to anything.
	// On the interactive path there is not even a writer to try, because
	// bubbletea owns the terminal. Everywhere else the answer is yes, and a
	// package that answered "yes, unless Windows" would be making a syscall's
	// decision from a build tag.
	//
	// It reports a capability and never an outcome. A console that answers
	// UTF-8 may still draw boxes -- a legacy console with a raster font does
	// exactly that, whatever its code page says -- which is why an explicit
	// GlyphSet overrules this in both directions, and why nothing downstream
	// may read a true value as a promise that a glyph appeared.
	//
	// false is the safe answer and false is the zero value, so a caller that
	// fills in nothing gets ASCII rather than mojibake. That safety is also why
	// this field cannot be checked for presence: package cli's test asks it
	// twice, once with each answer, because one row alone cannot tell a wired
	// field from an unwired one.
	ConsoleUTF8 bool
}

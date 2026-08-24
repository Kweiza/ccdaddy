package tui

import (
	"io"
	"time"

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
}

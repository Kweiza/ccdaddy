package tui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// addNeedsStderr is why the add key refuses when stderr is not a terminal.
//
// It names the redirect because that is the only thing the user can act on.
// The login writes every line of its prose — the URL, the paste instructions,
// the re-prompt — to stderr, and a released child is handed the process's real
// stderr rather than the dashboard's own output, so under a redirect the
// prompt goes to the sink while the dashboard sits blank waiting for a code
// the user was never shown.
const addNeedsStderr = "add needs a terminal on stderr: the login writes its prompt and its paste " +
	"instructions there, and stderr is redirected. Run ccdad without redirecting stderr, " +
	"or run 'ccdad add claude' or 'ccdad add codex' on its own."

// addFinishedMsg is what the released child's completion carries back to the
// event loop: the error, and nothing else. Everything the login had to say it
// said on the user's terminal while it held it.
type addFinishedMsg struct{ err error }

// addChoices is every provider the add key can open a login for, and the
// command line each one runs.
//
// The label and the argv are declared together for the reason every pickerItem
// is: the words a user reads immediately before releasing their terminal for
// several minutes cannot be allowed to drift away from what actually runs.
//
// The labels are the providers' own names rather than the command lines. A user
// on this screen is answering "which account am I adding", and the answer is
// the provider whether or not the two command lines happen to differ by one
// word.
func addChoices() []pickerItem {
	return []pickerItem{
		{label: "Claude", argv: []string{"add", "claude"}},
		{label: "Codex", argv: []string{"add", "codex"}},
	}
}

// AddArgvs is every command line the add key can run, for the one test that can
// check them and does not live here.
//
// The dashboard builds these argv itself and hands them to a fresh command
// tree, so a spelling that stopped resolving — or that grew children and became
// a group — would only be discovered by a user watching their terminal go
// somewhere unexpected. Package tui cannot check them for itself: package cli
// imports THIS package to register the dashboard, so the command tree is not
// visible from here and the dependency cannot be turned around.
//
// It hands back what addChoices just built, uncopied, and that is safe for one
// reason: addChoices builds the argv fresh on every call, so no two callers
// ever hold the same array and there is nothing here for a caller to corrupt.
// A defensive copy stood here and defended nothing — removing it left the whole
// suite green, because the aliasing it named could not happen. Hoisting the
// choices to a package-level value is what would make these shared, and the
// copy has to come back the day that happens.
func AddArgvs() [][]string {
	choices := addChoices()
	argvs := make([][]string, 0, len(choices))
	for _, it := range choices {
		argvs = append(argvs, it.argv)
	}
	return argvs
}

// addPicker is the provider choice, drawn as a picker and handled as its own
// screen.
//
// NOTHING is marked and the cursor opens on the first choice, every time. The
// mark on the other two pickers answers "which value is already in force", and
// there is no such value for an add: a user is not switching from one provider
// to another, they are adding an account to one of them. Nothing here says so
// in code because nothing has to: addChoices builds items whose inForce is the
// zero value, and that zero value IS the answer. Nor is the last choice
// remembered — the picker is built fresh on each keypress — because a
// remembered position would make the same two keystrokes add a Claude account
// today and a Codex one tomorrow.
func addPicker(g Glyphs) picker {
	return picker{
		title:  "Add an account for which provider?",
		items:  addChoices(),
		glyphs: g,
	}
}

// selfPath is os.Executable behind a name so the failure below can be
// exercised, and because that failure is real rather than theoretical: on
// Linux it resolves through /proc/self/exe, which stops being executable once
// the binary has been replaced under a running process — an upgrade in the
// middle of a session.
var selfPath = os.Executable

// addChild is the login, as a child process.
//
// A child rather than an in-process login, for four reasons, three of them
// measured failures of the alternative rather than arguments.
//
// It reuses every line of `ccdad add claude`'s prose unchanged — the paste
// re-prompt, the quarantine lift, the carriable-keys warning — instead of
// forking a few hundred lines against the rule that keeps prose in one place.
//
// The paste reader in this binary loops on a scanner over os.Stdin in a
// goroutine whose own documentation admits it cannot be stopped. Run in
// process, that goroutine is still parked on the descriptor the program
// re-arms afterwards, and it keeps looping: reproduced over a pty, after one
// successful paste the keys 1, 2 and even q never reached the program — the
// dashboard went permanently deaf and could not be quit. In a child the
// goroutine dies with the process.
//
// The signal arrangement is already written. With a program alive, SIGINT has
// been taken process-wide and is dropped for the whole released span, so an
// in-process login could not be interrupted at all — measured, Ctrl-C did
// literally nothing and the keystrokes were then swallowed by the prompt.
// `ccdad add claude` installs its own trap scoped to the login, so in the child
// that is the whole story: measured, the child exited 130, the parent dropped
// the signal, the program restored, and every subsequent key arrived.
//
// And a panic in the login is a dead child and an exit status, not a dead
// dashboard.
//
// The argv comes from picker.Chosen, whose contract is the argv the cursor is
// on OR nil when there is nothing to choose — the empty store a fresh install
// really has. Today's add picker is never that one: it is built from addChoices
// and its cursor is clamped into range, so the screen above cannot reach here
// with nothing, and the guard below is not a claim that it can. The guard is
// here because this is the last thing before exec and the value it is handed is
// documented to be nil-able. `exec.Command(self)` with nothing after it is the
// dashboard itself, launched into the terminal this dashboard has just let go
// of — a second full-screen program on the same tty, with the first one blocked
// until it exits. One comparison buys that, and it is the only thing standing
// there the day the provider list stops being a constant.
func addChild(argv []string) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, errors.New("no provider was chosen, and ccdad with no arguments is the dashboard itself")
	}
	self, err := selfPath()
	if err != nil {
		return nil, fmt.Errorf("ccdad could not locate its own binary to run the login: %w", err)
	}
	// Stdin, Stdout and Stderr stay nil: the library fills each one ONLY when
	// it is nil, wiring stdin to the program's input (os.Stdin, or the
	// /dev/tty it opened when stdin was redirected), stdout to the program's
	// output, and stderr to os.Stderr. Setting any of them here takes the
	// login off the terminal the user is looking at.
	//
	// SysProcAttr stays nil too. Setting it takes the child out of this
	// process group, and Ctrl-C then never reaches it.
	return exec.Command(self, argv...), nil
}

// addOutcome maps the child's exit onto ccdad's own contract.
//
// It never re-words what the child printed: the child's stderr went straight
// to the user's terminal while it held it, so anything written here would be a
// second, later, worse account of the same event. What this adds is the one
// thing the user cannot see any more, which is how it ended.
//
// 130 is an interrupted login and NOT a failure — it is what the login's own
// scoped trap exits with — so it is worded as the user's own decision.
func addOutcome(err error) string {
	if err == nil {
		return "added"
	}
	// The interface rather than *exec.ExitError itself: it is what an
	// ExitError satisfies, and it is the only thing wanted from one.
	var coded interface{ ExitCode() int }
	if !errors.As(err, &coded) {
		// The child never ran at all — the binary could not be found or could
		// not be executed. That is a different failure from a login that went
		// wrong, and it is the user's whole machine rather than their account.
		return "the login could not be started: " + err.Error()
	}
	switch code := coded.ExitCode(); code {
	case 130:
		return "login canceled"
	case exitBlocked:
		return "blocked; the login printed its own reason on the way past"
	case 2:
		return "usage error"
	default:
		return fmt.Sprintf("the login failed (exit %d)", code)
	}
}

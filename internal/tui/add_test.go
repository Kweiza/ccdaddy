package tui

import (
	"errors"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The library's exec support fills each of the child's three streams ONLY when
// it is nil, wiring stdin to the program's input (os.Stdin, or the /dev/tty it
// opened when stdin was redirected), stdout to the program's output and stderr
// to os.Stderr. Setting any of them here takes the login off the terminal the
// user is looking at.
func TestTheChildLeavesItsThreeStreamsForBubbleteaToFill(t *testing.T) {
	c, err := addChild([]string{"add", "claude"})
	if err != nil {
		t.Skipf("os.Executable() is unavailable here: %v", err)
	}
	if c.Stdin != nil || c.Stdout != nil || c.Stderr != nil {
		t.Fatal("a stream was set, so the library will not wire it to the terminal")
	}
	if c.SysProcAttr != nil {
		t.Fatal("SysProcAttr takes the child out of this process group, and Ctrl-C then never reaches it")
	}
	if !slices.Equal(c.Args[1:], []string{"add", "claude"}) {
		t.Fatalf("the child runs %v, want [<self> add claude]", c.Args)
	}
}

// Every login ships bare, with no --activate. The commands deliberately do not
// switch after adding and the switch key is the next one over, so leaving the
// flag off keeps the two keys meaning what they say.
func TestTheChildCarriesNoFlagsTheUserDidNotAskFor(t *testing.T) {
	for _, argv := range AddArgvs() {
		c, err := addChild(argv)
		if err != nil {
			t.Skipf("os.Executable() is unavailable here: %v", err)
		}
		for _, arg := range c.Args[1:] {
			if strings.HasPrefix(arg, "-") {
				t.Fatalf("the child carries %q, which nobody chose on this key", arg)
			}
		}
	}
}

// The child runs the argv it was handed, word for word: nothing is appended,
// reordered or dropped on the way to the terminal. This says nothing about
// WHICH argv arrives here -- that is the choice's job, pinned by the table
// below and, end to end, by the keypress that releases the terminal.
func TestTheChildRunsTheArgvItWasHandedWordForWord(t *testing.T) {
	for _, argv := range AddArgvs() {
		c, err := addChild(argv)
		if err != nil {
			t.Skipf("os.Executable() is unavailable here: %v", err)
		}
		if !slices.Equal(c.Args[1:], argv) {
			t.Errorf("the child runs %v for the argv %v", c.Args[1:], argv)
		}
	}
}

// The provider a user reads is the provider whose login opens, and the pairing
// is written out HERE, by value, rather than read out of the thing it is meant
// to pin.
//
// Every assertion that takes both the label and the argv from the same row
// holds just as well when the two argvs are transposed -- the label moves with
// the command line and the two go on agreeing with each other about the wrong
// answer. Only a table a reader can see a swap in catches that, and a swap here
// is the whole failure: a user picks Claude, reads "Claude", and their terminal
// is handed to the Codex login for as long as it takes them to notice.
func TestEachProviderLabelNamesItsOwnLogin(t *testing.T) {
	want := map[string][]string{
		"Claude": {"add", "claude"},
		"Codex":  {"add", "codex"},
	}
	choices := addChoices()
	if len(choices) != len(want) {
		t.Fatalf("the add key offers %d providers and this table pins %d", len(choices), len(want))
	}
	for _, it := range choices {
		argv, pinned := want[it.label]
		if !pinned {
			t.Errorf("the add key offers %q, which this table does not pin", it.label)
			continue
		}
		if !slices.Equal(it.argv, argv) {
			t.Errorf("choosing %q runs %v, want %v", it.label, it.argv, argv)
		}
	}
}

// An empty argv is refused rather than executed, and the reason is not
// hygiene. `exec.Command(self)` with nothing after it is the dashboard itself,
// opened inside the terminal this dashboard has just released — a second
// full-screen program on the same tty, with the first one blocked waiting for
// it to exit.
func TestAnEmptyArgvIsRefusedRatherThanReopeningTheDashboard(t *testing.T) {
	for _, argv := range [][]string{nil, {}} {
		c, err := addChild(argv)
		if err == nil {
			t.Fatalf("addChild(%v) produced a child that would re-open the dashboard on the released terminal", argv)
		}
		if c != nil {
			t.Fatalf("addChild(%v) produced both an error and a command", argv)
		}
	}
}

// The exported list and the list the screen draws are one list. Two spellings
// of "which providers can be added" would agree until the day one of them
// grew a provider, and the tripwire in package cli reads the exported one.
func TestTheExportedArgvsAreTheOnesTheScreenOffers(t *testing.T) {
	choices := addChoices()
	argvs := AddArgvs()
	if len(argvs) != len(choices) {
		t.Fatalf("AddArgvs offers %d command lines and the screen draws %d choices", len(argvs), len(choices))
	}
	for i, it := range choices {
		if !slices.Equal(argvs[i], it.argv) {
			t.Errorf("choice %q runs %v and AddArgvs reports %v", it.label, it.argv, argvs[i])
		}
	}
}

// Exit 130 is an interrupted login and not a failure: `ccdad add claude`
// installs its own scoped SIGINT trap for the span of the login and exits 130
// when it fires. Measured over a pty: the child exited 130, the parent dropped
// the signal, the program restored, and every subsequent key arrived.
func TestTheChildsExitCodeIsMappedOntoTheExitContractAndNotGuessedAt(t *testing.T) {
	for _, tc := range []struct {
		code int
		want string
	}{
		{0, "added"},
		{130, "canceled"},
		{4, "blocked"},
		{2, "usage"},
		{9, "exit 9"},
	} {
		got := addOutcome(exitErr(t, tc.code))
		if !strings.Contains(got, tc.want) {
			t.Errorf("addOutcome(exit %d) = %q, want it to say %q", tc.code, got, tc.want)
		}
	}
}

// A canceled login is worded as the user's own decision and never as a
// failure, or a dashboard reports an interruption the user chose as something
// that went wrong.
func TestACanceledLoginIsNotWordedAsAFailure(t *testing.T) {
	got := strings.ToLower(addOutcome(exitErr(t, 130)))
	if strings.Contains(got, "fail") || strings.Contains(got, "error") {
		t.Fatalf("addOutcome(exit 130) = %q, and 130 is the login's own scoped trap firing", got)
	}
}

// A child that never started is a different failure from a login that went
// wrong: it is the machine rather than the account, and the error says which.
func TestAChildThatNeverRanIsDistinguishedFromALoginThatFailed(t *testing.T) {
	got := addOutcome(errors.New("fork/exec: permission denied"))
	if !strings.Contains(got, "could not be started") {
		t.Fatalf("addOutcome of a start failure = %q, which reads as a login that ran", got)
	}
	if !strings.Contains(got, "permission denied") {
		t.Fatalf("addOutcome swallowed what actually went wrong: %q", got)
	}
}

// The message the released child sends back carries the error and nothing
// else. Everything the login had to say it said on the terminal it was holding.
func TestTheFinishedMessageCarriesTheChildsErrorAndNothingElse(t *testing.T) {
	msg := addFinishedMsg{err: exitErr(t, 130)}
	if !strings.Contains(addOutcome(msg.err), "canceled") {
		t.Fatal("the finished message does not carry an error addOutcome can read")
	}
}

// The one gate this key needs. `ccdad add claude` writes every line of its
// prose to stderr and the library gives the child os.Stderr rather than the
// program's own output, so under `ccdad 2>/dev/null` the login would wait for a
// code the user was never shown, behind a dashboard that had vanished.
func TestTheAddKeyRefusesWhenStderrIsRedirectedAndNamesTheRedirect(t *testing.T) {
	if !strings.Contains(addNeedsStderr, "stderr") {
		t.Fatal("the refusal does not name what is redirected, so a user cannot act on it")
	}
}

// os.Executable resolves through /proc/self/exe on Linux, which fails to exec
// after the binary has been replaced under a running process -- an upgrade
// mid-session. That is a real failure a user can act on and it is surfaced
// rather than swallowed.
func TestAnUnresolvableSelfIsReportedRatherThanSwallowed(t *testing.T) {
	restore := selfPath
	t.Cleanup(func() { selfPath = restore })
	selfPath = func() (string, error) {
		return "", errors.New("/proc/self/exe: no such file or directory")
	}
	c, err := addChild([]string{"add", "claude"})
	if err == nil {
		t.Fatal("an unresolvable self produced a child anyway")
	}
	if c != nil {
		t.Fatal("an unresolvable self produced both an error and a command")
	}
	if !strings.Contains(err.Error(), "/proc/self/exe") {
		t.Fatalf("the error lost what actually failed: %v", err)
	}
}

// exitErr runs a child that exits with code and returns what Run said about
// it, so the table above is measured against a REAL *exec.ExitError rather
// than a stand-in. An ExitError's code cannot be set by hand — it is read out
// of an os.ProcessState, which only a finished process produces — so a stub
// would be proving that the stub works.
//
// Code 0 returns a nil error, which is the case the table needs for "added".
func exitErr(t *testing.T, code int) error {
	t.Helper()
	c := exec.Command(os.Args[0], "-test.run=TestTheHelperProcessOnlyExits")
	c.Env = append(os.Environ(), helperExitVar+"="+strconv.Itoa(code))
	return c.Run()
}

const helperExitVar = "CCDAD_TUI_TEST_EXIT"

// TestTheHelperProcessOnlyExits is not a test. It is the child exitErr runs,
// and it does nothing at all unless that variable is set — which is what keeps
// it inert during an ordinary run of this package's suite.
func TestTheHelperProcessOnlyExits(t *testing.T) {
	want := os.Getenv(helperExitVar)
	if want == "" {
		return
	}
	code, err := strconv.Atoi(want)
	if err != nil {
		t.Fatalf("%s=%q is not a number", helperExitVar, want)
	}
	os.Exit(code)
}

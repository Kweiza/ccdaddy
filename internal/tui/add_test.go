package tui

import (
	"errors"
	"os"
	"os/exec"
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
	c, err := addChild()
	if err != nil {
		t.Skipf("os.Executable() is unavailable here: %v", err)
	}
	if c.Stdin != nil || c.Stdout != nil || c.Stderr != nil {
		t.Fatal("a stream was set, so the library will not wire it to the terminal")
	}
	if c.SysProcAttr != nil {
		t.Fatal("SysProcAttr takes the child out of this process group, and Ctrl-C then never reaches it")
	}
	if len(c.Args) != 2 || c.Args[1] != "add" {
		t.Fatalf("the child runs %v, want [<self> add]", c.Args)
	}
}

// The login ships as a bare `ccdad add`, with no --activate. The command
// deliberately does not switch after adding and the switch key is the next one
// over, so leaving the flag off keeps the two keys meaning what they say.
func TestTheChildCarriesNoFlagsTheUserDidNotAskFor(t *testing.T) {
	c, err := addChild()
	if err != nil {
		t.Skipf("os.Executable() is unavailable here: %v", err)
	}
	for _, arg := range c.Args[1:] {
		if strings.HasPrefix(arg, "-") {
			t.Fatalf("the child carries %q, which nobody chose on this key", arg)
		}
	}
}

// Exit 130 is an interrupted login and not a failure: `ccdad add` installs its
// own scoped SIGINT trap for the span of the login and exits 130 when it fires.
// Measured over a pty: the child exited 130, the parent dropped the signal, the
// program restored, and every subsequent key arrived.
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

// The one gate this key needs. `ccdad add` writes every line of its prose to
// stderr and the library gives the child os.Stderr rather than the program's own
// output, so under `ccdad 2>/dev/null` the login would wait for a code the user
// was never shown, behind a dashboard that had vanished.
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
	c, err := addChild()
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

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The one assertion no other host can make: what a Windows child actually
// RECEIVED on its argument vector.
//
// Everything in cmdshim_test.go stubs the launcher, so it measures the
// launchSpec ccdad built — Go's intent. The defect this exists for lives one
// layer down, between makeCmdLine and whoever parses the command line: an
// argument with no space, quote or backslash is emitted RAW, so `fix&whoami`
// reaches cmd.exe as two commands. Only a real CreateProcess tells the two
// apart, and only on this platform.
//
// The interpreter is this test binary re-executed in argvRoleEnv's role rather
// than node, for the reason internal/cclink's fixture `security` gives: a
// runner that happens not to have node would turn a real assertion into a
// skip, and the property under test is about the command line, not about node.
//
// beside picks which of resolvePastShim's two branches a real runner
// exercises, and the choice is load-bearing rather than cosmetic:
//
//   - true puts node.exe next to the shim. That is the branch the shim itself
//     prefers, and the one whose path arrives with a DOUBLED separator, since
//     %dp0% already ends in one and the generator writes another. PATH is
//     closed off so a live lookup cannot quietly stand in for it.
//   - false leaves that absent and resolves through the injected PATH lookup,
//     pointing it at a copy of this binary in a DIFFERENT directory. That also
//     makes the test decisive about the route, which the other branch is not:
//     with no node.exe beside it, the shim's own `IF EXIST` fails and cmd.exe
//     falls back to a bare `node` that either is not installed or is handed a
//     cli.js that does not exist. Either way the launch exits non-zero, so a
//     ccdad that went through cmd.exe cannot pass this test by accident.
func childArgvThroughShim(t *testing.T, beside bool, args ...string) ([]string, string) {
	t.Helper()
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")

	self, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	interpreter := func(dir string) string {
		exe := filepath.Join(dir, "node.exe")
		if err := os.WriteFile(exe, self, 0o700); err != nil {
			t.Fatal(err)
		}
		return exe
	}

	// A real generated shim, with a copy of this binary standing in for node.
	dir := t.TempDir()
	shim := filepath.Join(dir, "claude.cmd")
	if err := os.WriteFile(shim, []byte(readFixture(t, "env-node.cmd")), 0o700); err != nil {
		t.Fatal(err)
	}
	stubLookClaude(t, shim)
	if beside {
		interpreter(dir)
		// If Windows or Go were to reject the doubled separator, a live PATH
		// lookup would find the runner's real node instead and the test would
		// fail somewhere unrecognisable; refusing PATH makes the failure say
		// what it is.
		stubLookProgram(t, "", errors.New("PATH is closed for this test"))
	} else {
		stubFileExists(t, false)
		stubLookProgram(t, interpreter(t.TempDir()), nil)
	}

	// The role, and where it writes what it was given. It goes on the test
	// process because `run` builds the child's environment from this one.
	seen := filepath.Join(t.TempDir(), "argv.json")
	t.Setenv(argvRoleEnv, seen)

	// startChild is NOT stubbed. A real process is the whole point.
	code, _, errOut, top := runRoot(t, append([]string{"run", "1"}, args...)...)
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}

	raw, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("the child never recorded its arguments (%v); stderr: %s", err, errOut)
	}
	var got []string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("the child was given no arguments at all")
	}
	if !strings.HasSuffix(got[0], "cli.js") {
		t.Errorf("argv[0] = %q, want the script the shim runs", got[0])
	}
	return got[1:], errOut
}

// The case the shim resolution was written for: `&` is emitted raw by Go and
// read by cmd.exe as a command separator, so this argument is either delivered
// whole or it is a second command. Going through cmd.exe cannot pass here — it
// is the failure the assertion describes.
func TestWindowsAnAmpersandReachesTheChildUnmangled(t *testing.T) {
	got, _ := childArgvThroughShim(t, true, "-p", "fix&whoami", "--model", "a|b")

	want := []string{"-p", "fix&whoami", "--model", "a|b"}
	if !slices.Equal(got, want) {
		t.Fatalf("the child received %q, want %q — the metacharacters did not survive the launch", got, want)
	}
}

// The case the WIDENING newly covers, and the one a green run of the test
// above says nothing about.
//
// Nothing here contains a character cmd.exe acts on, so until resolution ran
// for every shim these took the other route: Go quoted them, cmd.exe unquoted
// them, `%*` spliced them back and the CRT parsed them again. That round trip
// is approximately correct, which is a different thing from correct.
//
// The last argument ends in a backslash on purpose. It is where Go's escaping
// and a re-reading of the command line disagree most quietly: makeCmdLine
// doubles the trailing backslash so the closing quote is not escaped, and every
// layer that re-reads the line afterwards is another chance for the pair to
// stop being a pair.
//
// Read the assertion honestly: the ARGUMENTS alone cannot tell the two routes
// apart, because `%*` splices the command line verbatim and there is no
// ordinary argument cmd.exe mangles — that is precisely why the widening is a
// correctness argument rather than a bug fix. What makes this test decisive
// about the route is the branch it runs on, described in childArgvThroughShim:
// with no node.exe beside the shim, the cmd.exe route cannot exit zero at all.
func TestWindowsAnOrdinaryArgumentReachesTheChildUnmangled(t *testing.T) {
	got, errOut := childArgvThroughShim(t, false,
		"-p", "summarize this file", "--add-dir", `C:\Users\dev\my repo\`)

	want := []string{"-p", "summarize this file", "--add-dir", `C:\Users\dev\my repo\`}
	if !slices.Equal(got, want) {
		t.Fatalf("the child received %q, want %q — an ordinary argument did not survive the launch", got, want)
	}
	// The rescue note explains why the process tree does not match what was
	// typed. There is nothing to explain here, and this is the launch every
	// npm-installed session now takes.
	if strings.Contains(errOut, "note:") {
		t.Errorf("a note was printed for a launch with nothing to explain:\n%s", errOut)
	}
}

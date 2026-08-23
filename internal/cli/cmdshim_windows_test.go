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
func TestWindowsAnAmpersandReachesTheChildUnmangled(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")

	// A real generated shim, with a copy of this binary standing in for the
	// node.exe npm puts beside it — which is the branch resolvePastShim
	// prefers, and the one with the doubled separator from %dp0% in it.
	dir := t.TempDir()
	shim := filepath.Join(dir, "claude.cmd")
	if err := os.WriteFile(shim, []byte(readFixture(t, "env-node.cmd")), 0o700); err != nil {
		t.Fatal(err)
	}
	self, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node.exe"), self, 0o700); err != nil {
		t.Fatal(err)
	}
	stubLookClaude(t, shim)
	// PATH is closed off deliberately. The branch under test is the one that
	// takes the node.exe sitting beside the shim, and its path arrives with a
	// DOUBLED separator — %dp0% already ends in one and the generator writes
	// another. If Windows or Go were to reject that spelling, a live PATH
	// lookup would quietly find the runner's real node instead and the test
	// would fail somewhere unrecognisable; refusing PATH makes the failure say
	// what it is.
	stubLookProgram(t, "", errors.New("PATH is closed for this test"))

	// The role, and where it writes what it was given. It goes on the test
	// process because `run` builds the child's environment from this one.
	seen := filepath.Join(t.TempDir(), "argv.json")
	t.Setenv(argvRoleEnv, seen)

	// startChild is NOT stubbed. A real process is the whole point.
	code, _, errOut, top := runRoot(t, "run", "1", "-p", "fix&whoami", "--model", "a|b")
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
	want := []string{"-p", "fix&whoami", "--model", "a|b"}
	if !slices.Equal(got[1:], want) {
		t.Fatalf("the child received %q, want %q — the metacharacters did not survive the launch", got[1:], want)
	}
}

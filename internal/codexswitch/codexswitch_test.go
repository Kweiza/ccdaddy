package codexswitch

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// home points CCDAD_HOME at a temp directory. Execute stamps the cooldown
// through strategy.WithState, which resolves the store from that variable, so a
// test without this would write into the developer's real strategy.json.
func home(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "ccdad")
	t.Setenv("CCDAD_HOME", root)
	return root
}

func TestReadServingAnswersNoOnAFreshMachine(t *testing.T) {
	root := home(t)
	if uuid, ok := ReadServing(root); ok {
		t.Fatalf("ReadServing = (%q, true) with no pointer written; want no pointer", uuid)
	}
}

func TestExecuteWritesThePointerAndReadServingReadsItBack(t *testing.T) {
	root := home(t)
	if err := Execute(root, "cx-1"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	uuid, ok := ReadServing(root)
	if !ok || uuid != "cx-1" {
		t.Fatalf("ReadServing = (%q, %v), want (\"cx-1\", true)", uuid, ok)
	}
}

// A successful Execute stamps the cooldown, not just avoiding a stamp on
// failure. Nothing outside this package calls Execute, so without this test
// the RecordCodexSwitch call could be replaced with a no-op and every other
// test here would stay green.
func TestExecuteStampsTheCooldownOnSuccess(t *testing.T) {
	root := home(t)
	if err := Execute(root, "cx-1"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	st, err := strategy.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	at, to := st.CodexLastSwitch()
	if to != "cx-1" {
		t.Fatalf("CodexLastSwitch() to = %q, want %q", to, "cx-1")
	}
	if at.IsZero() {
		t.Fatal("CodexLastSwitch() at is the zero time after a successful Execute, want a real stamp")
	}
}

// The file is read by a proxy handler on every request, so it has to be exactly
// one line with the uuid on it -- and 0600, because it names which account a
// machine is spending.
func TestThePointerIsOneLineAtSixHundred(t *testing.T) {
	root := home(t)
	if err := Execute(root, "cx-1"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	raw, err := os.ReadFile(ServingPath(root))
	if err != nil {
		t.Fatalf("reading the pointer: %v", err)
	}
	if string(raw) != "cx-1\n" {
		t.Fatalf("the pointer holds %q, want %q", string(raw), "cx-1\n")
	}
	if runtime.GOOS == "windows" {
		return // chmod is a no-op there
	}
	info, err := os.Stat(ServingPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("the pointer is mode %v, want 0600", got)
	}
}

// A pointer with whitespace around it, which is what a hand edit leaves.
func TestReadServingTrimsWhatItReads(t *testing.T) {
	root := home(t)
	if err := os.MkdirAll(filepath.Dir(ServingPath(root)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ServingPath(root), []byte("  cx-2\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if uuid, ok := ReadServing(root); !ok || uuid != "cx-2" {
		t.Fatalf("ReadServing = (%q, %v), want (\"cx-2\", true)", uuid, ok)
	}
}

// An empty file is not a pointer at an account named "". Answering ok on one
// would have the proxy look up an account that cannot exist and fall through to
// a branded error instead of to the top-ranked account.
func TestAnEmptyPointerIsNoPointer(t *testing.T) {
	root := home(t)
	if err := os.MkdirAll(filepath.Dir(ServingPath(root)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ServingPath(root), []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if uuid, ok := ReadServing(root); ok {
		t.Fatalf("ReadServing = (%q, true) on an empty file; want no pointer", uuid)
	}
}

// A directory where the pointer file should be -- os.ReadFile refuses it, and
// that refusal must read as "no pointer", the same as any other unreadable
// path.
func TestReadServingAnswersNoOnADirectory(t *testing.T) {
	root := home(t)
	if err := os.MkdirAll(ServingPath(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if uuid, ok := ReadServing(root); ok {
		t.Fatalf("ReadServing = (%q, true) with a directory at the pointer path; want no pointer", uuid)
	}
}

// A strictly zero-byte file, as opposed to one holding only whitespace. The
// two reach the empty check by different paths -- os.ReadFile returning a
// zero-length slice versus TrimSpace collapsing whitespace to "" -- and only
// one of them is exercised by TestAnEmptyPointerIsNoPointer.
func TestReadServingAnswersNoOnAZeroByteFile(t *testing.T) {
	root := home(t)
	if err := os.MkdirAll(filepath.Dir(ServingPath(root)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ServingPath(root), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	if uuid, ok := ReadServing(root); ok {
		t.Fatalf("ReadServing = (%q, true) on a zero-byte file; want no pointer", uuid)
	}
}

// The pointer comes FIRST and the stamp only after it landed. A stamp written
// for a pointer that never got written is a cooldown holding the lane off the
// retry that would have fixed it.
//
// The unwritable directory is codex/ and NOT the store root, and the difference
// is what makes this test assert the ordering rather than the mkdir: the root
// has to stay writable or strategy.json cannot be written either, and then a
// stamp-first implementation would fail for the wrong reason and pass.
func TestAFailedPointerWriteStampsNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod is a no-op on Windows beyond the read-only bit")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := home(t)
	codexDir := filepath.Dir(ServingPath(root))
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(codexDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(codexDir, 0o700) })

	if err := Execute(root, "cx-1"); err == nil {
		t.Fatal("Execute succeeded with an unwritable codex directory; want a failure")
	}
	st, err := strategy.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if at, to := st.CodexLastSwitch(); !at.IsZero() || to != "" {
		t.Fatalf("CodexLastSwitch() = (%v, %q) after a failed pointer write, want the zero pair", at, to)
	}
}

// The opposite split: the pointer write succeeds -- codex/ is writable -- but
// the stamp fails because the STORE ROOT itself is not writable, so
// strategy's lock directory (a sibling of codex/, directly under root) cannot
// be created. Execute must still report an error, but the pointer has
// already moved: ReadServing must show the new account, and the error must
// say so via ErrPointerMovedUnstamped rather than leaving the caller to guess.
func TestAFailedStampLeavesThePointerMovedAndSaysSo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod is a no-op on Windows beyond the read-only bit")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := home(t)
	if err := os.MkdirAll(filepath.Join(root, "codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })

	err := Execute(root, "cx-1")
	if err == nil {
		t.Fatal("Execute succeeded with an unwritable store root; want a failure")
	}
	if !errors.Is(err, ErrPointerMovedUnstamped) {
		t.Fatalf("Execute error = %v, want it to wrap ErrPointerMovedUnstamped", err)
	}
	uuid, ok := ReadServing(root)
	if !ok || uuid != "cx-1" {
		t.Fatalf("ReadServing = (%q, %v) after a stamp-only failure, want (\"cx-1\", true) because the pointer already moved", uuid, ok)
	}
}

func TestClearRemovesThePointer(t *testing.T) {
	root := home(t)
	if err := Execute(root, "cx-1"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if err := Clear(root); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if uuid, ok := ReadServing(root); ok {
		t.Fatalf("ReadServing = (%q, true) after Clear; want no pointer", uuid)
	}
	// Clearing twice is not a failure: `ccdad remove` calls it without first
	// asking whether there was one.
	if err := Clear(root); err != nil {
		t.Fatalf("Clear on a machine with no pointer: %v, want nil", err)
	}
}

// THE IMPORT GATE. This package writes a pointer file and stamps a cooldown,
// and it must be structurally incapable of doing anything to Claude Code's
// login: not through internal/switcher, which installs credentials, not through
// internal/cclink, which writes the credentials file, and not through
// internal/store, which is why Execute takes a uuid string rather than an
// account.
//
// It is a dependency-CLOSURE test rather than a check of this file's own import
// block, because the danger is transitive: a helper added to internal/strategy
// that imported internal/cclink would put the credentials writer one call away
// from here with nothing in this package changing.
func TestTheCodexSwitchCannotReachClaudesSwitcher(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "./").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	forbidden := []string{
		"github.com/Kweiza/ccdaddy/internal/cclink",
		"github.com/Kweiza/ccdaddy/internal/switcher",
		"github.com/Kweiza/ccdaddy/internal/store",
	}
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		for _, bad := range forbidden {
			if strings.TrimSpace(dep) == bad {
				t.Errorf("internal/codexswitch depends on %s; a codex repoint must not be able to "+
					"reach Claude Code's credential path at all", bad)
			}
		}
	}
}

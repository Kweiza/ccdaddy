//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// setConsoleVT's Windows body, against a real console.
//
// Its POLICY — which invocations call it, and that the daemon entrypoint does
// not — is pinned on every platform by root_test.go, which stubs the consoleVT
// seam. The body behind that seam ran on no machine at all until this file:
// GetConsoleMode, the already-set early return and the SetConsoleMode OR were
// compiled and vetted for windows/amd64 and windows/arm64, and executed by
// nothing. A CI job cannot stand in for a console on its own — a job step's
// stdout is a pipe, so GetConsoleMode fails on it and the function returns at
// its first branch, which is the one path that already needed no proof.
//
// So the console is manufactured rather than hoped for. See ensureConsole.

// ensureConsole gives this process a console if it does not already have one.
//
// This is a second copy of internal/daemon's helper of the same name, and the
// duplication is forced rather than chosen: a function defined in one package's
// _test.go cannot be imported by another package, and a shared one would have
// to be a real package in the tree, compiled into every build, existing for two
// test callers. Keep them in step.
//
// AllocConsole's one documented failure is "this process already has a
// console", which is the case that returns above. Anything else is worth being
// loud about rather than skipping past: scripts/ci.sh runs `go test` without
// -v and a passing package's output is discarded, so a skip here would be
// indistinguishable from an assertion that ran.
//
// It is deliberately not undone — FreeConsole would take the console away from
// whatever else in the process is using it — and the standard handles it
// installs are not adopted, since Go captured os.Stdout at init from the handle
// `go test` gave it.
func ensureConsole(t *testing.T) {
	t.Helper()
	if consoleAttached() {
		return
	}
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("AllocConsole")
	if r, _, err := proc.Call(); r == 0 {
		t.Fatalf("AllocConsole: %v — this process has no console and could not be given one, so nothing "+
			"here can execute setConsoleVT's body", err)
	}
	if !consoleAttached() {
		t.Fatal("AllocConsole reported success and CONOUT$ still cannot be opened")
	}
}

// consoleAttached reports whether this process is attached to a console.
// CONOUT$ names the active screen buffer of the CALLING process's console, and
// there is nothing to name when the process has none.
func consoleAttached() bool {
	f, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// openConsole hands back this process's console screen buffer as an *os.File,
// which is the shape setConsoleVT takes, with its mode restored afterwards.
//
// Restoring is for the suite's sake and not the console's: setConsoleVT itself
// deliberately leaves VT processing on, for the reason its own comment gives.
// A test that left the bit as it found it is one that can run in any order and
// twice.
func openConsole(t *testing.T) *os.File {
	t.Helper()
	ensureConsole(t)
	f, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("opening CONOUT$ although this process has a console: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	original := consoleMode(t, f)
	t.Cleanup(func() {
		if err := windows.SetConsoleMode(windows.Handle(f.Fd()), original); err != nil {
			t.Errorf("restoring the console mode to %#x: %v", original, err)
		}
	})
	return f
}

func consoleMode(t *testing.T, f *os.File) uint32 {
	t.Helper()
	var mode uint32
	if err := windows.GetConsoleMode(windows.Handle(f.Fd()), &mode); err != nil {
		t.Fatalf("GetConsoleMode: %v", err)
	}
	return mode
}

func setConsoleMode(t *testing.T, f *os.File, mode uint32) {
	t.Helper()
	if err := windows.SetConsoleMode(windows.Handle(f.Fd()), mode); err != nil {
		t.Fatalf("SetConsoleMode(%#x): %v", mode, err)
	}
}

// The whole point of §10.3: a classic conhost ships with
// ENABLE_VIRTUAL_TERMINAL_PROCESSING off, and until it is on, every escape
// sequence ccdad prints arrives as literal text.
//
// The OR is asserted rather than only the bit, and that is not fussiness.
// SetConsoleMode(h, ENABLE_VIRTUAL_TERMINAL_PROCESSING) — the version without
// the OR, which is the obvious way to write this wrong — clears
// ENABLE_PROCESSED_OUTPUT and ENABLE_WRAP_AT_EOL_OUTPUT with it, so the console
// stops interpreting \n and stops wrapping at the right margin. That failure
// looks like a rendering bug in ccdad rather than like a mode it clobbered.
func TestSetConsoleVTTurnsOnVirtualTerminalProcessingAndKeepsTheRestOfTheMode(t *testing.T) {
	con := openConsole(t)

	before := consoleMode(t, con) &^ windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING
	setConsoleMode(t, con, before)

	if err := setConsoleVT(con); err != nil {
		t.Fatalf("setConsoleVT on a real console: %v", err)
	}

	got := consoleMode(t, con)
	if got&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING == 0 {
		t.Fatalf("console mode is %#x, ENABLE_VIRTUAL_TERMINAL_PROCESSING (%#x) is not set — ccdad's colour "+
			"and cursor sequences arrive as literal text", got, uint32(windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING))
	}
	if want := before | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING; got != want {
		t.Errorf("console mode is %#x, want %#x — setConsoleVT replaced the mode instead of widening it, "+
			"so the console lost %#x", got, want, (before|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)&^got)
	}
}

// Called twice, the second call must leave the mode exactly as the first did.
//
// This is the observable half of the already-set early return, and it is worth
// saying what the unobservable half is: deleting `if mode&...VT != 0 { return
// nil }` would make this pass anyway, because the SetConsoleMode it falls
// through to writes the value the mode already holds. The early return saves a
// syscall; nothing an assertion can reach distinguishes it. What IS worth
// pinning is that a second enable does not disturb a console some other process
// is sharing, which is exactly what this asks.
func TestSetConsoleVTOnAConsoleThatAlreadyHasItChangesNothing(t *testing.T) {
	con := openConsole(t)

	if err := setConsoleVT(con); err != nil {
		t.Fatalf("first setConsoleVT: %v", err)
	}
	after := consoleMode(t, con)

	if err := setConsoleVT(con); err != nil {
		t.Fatalf("second setConsoleVT: %v", err)
	}
	if got := consoleMode(t, con); got != after {
		t.Errorf("a second setConsoleVT changed the console mode from %#x to %#x", after, got)
	}
}

// A handle with no console mode to widen is the answer, not an error: it is a
// pipe, a file, or the null device Spawn hands the daemon. ccdad calls this on
// os.Stdout for every non-daemon invocation, and every one of those redirected
// anywhere would fail at startup if GetConsoleMode's error were returned.
//
// A pipe and a regular file are both asked, because they are the two shapes
// stdout actually takes: `ccdad status | more` and `ccdad status > out.txt`.
func TestSetConsoleVTAcceptsAHandleThatIsNotAConsole(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })

	file, err := os.Create(filepath.Join(t.TempDir(), "redirected"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })

	for _, tc := range []struct {
		name string
		f    *os.File
	}{
		{"a pipe", w},
		{"a regular file", file},
		// Not reachable through enableConsoleVT, which always passes
		// os.Stdout, but the guard is in the function and a nil *os.File
		// answers Fd() with an invalid handle rather than panicking — so
		// without the guard this would reach GetConsoleMode and still return
		// nil. Asserted so that the guard cannot be deleted along with the
		// behaviour it describes.
		{"no file at all", nil},
	} {
		if err := setConsoleVT(tc.f); err != nil {
			t.Errorf("setConsoleVT(%s) = %v, want nil — there is no console mode to widen, which is an "+
				"answer rather than a failure", tc.name, err)
		}
	}
}

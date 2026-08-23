//go:build windows

package daemon

import (
	"os"
	"syscall"
	"testing"
)

// consoleAttached reports whether this process is attached to a console.
//
// CONOUT$ names the active screen buffer of the CALLING process's console, and
// a process created with DETACHED_PROCESS has no console for it to name — so
// the open failing IS the answer, and it needs nothing outside the standard
// library to ask.
//
// This is also the only observation that separates a detached child from one
// that merely had its three standard handles pointed at NUL. Go hands
// CreateProcess an explicit inherited-handle list, so
// TestSpawnDoesNotLeaveTheChildHoldingOurStdout would pass on Windows with the
// creation flags deleted outright — and DETACHED_PROCESS is the flag spawn.go
// calls the load-bearing one.
func consoleAttached() bool {
	f, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// AllocConsole is in neither syscall nor golang.org/x/sys/windows, and a
// LazyDLL in a test file is cheaper than a dependency for one call.
var procAllocConsole = syscall.NewLazyDLL("kernel32.dll").NewProc("AllocConsole")

// ensureConsole gives this process a console if it does not already have one.
//
// A CI job is the only Windows machine this repository can reach, and whether
// its steps run attached to a console is a property of the runner rather than
// of ccdad. Asking for one removes the dependency instead of reporting it: a
// skip would be invisible anyway, since scripts/ci.sh runs `go test` without
// -v and a passing package's output is discarded — so a leg that could not
// assert this would read exactly like a leg that did.
//
// AllocConsole's one documented failure is "this process already has a
// console", which is the case that returns above. A different failure is worth
// being loud about rather than skipping past, because it would mean the
// assumption behind this fixture is wrong.
//
// It is deliberately not undone. FreeConsole would take the console away from
// whatever else in this process is using it, and a console allocated here dies
// with the process regardless. Nor are the standard handles it installs
// adopted: Go captured os.Stdout at init from the handle `go test` gave it, so
// this does not redirect the suite's own output.
func ensureConsole(t *testing.T) {
	t.Helper()
	if consoleAttached() {
		return
	}
	if r, _, err := procAllocConsole.Call(); r == 0 {
		t.Fatalf("AllocConsole: %v — this process has no console and could not be given one, so nothing "+
			"here can observe whether DETACHED_PROCESS severs a child from it", err)
	}
	if !consoleAttached() {
		t.Fatal("AllocConsole reported success and CONOUT$ still cannot be opened")
	}
}

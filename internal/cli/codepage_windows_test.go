//go:build windows

package cli

import (
	"testing"

	"golang.org/x/sys/windows"
)

// consoleUTF8's Windows body, against a real console.
//
// Its POLICY -- which invocations consult it -- is pinned on every platform by
// codepage_test.go, which stubs the seam. The body behind that seam is one
// syscall and one comparison, and it executes on the windows-latest leg alone.
// Two things there are worth proving. First, that 65001 and a non-65001 page
// give DIFFERENT answers: a body that returned a constant would satisfy every
// cross-platform test in the package. Second, that asking the question leaves
// the console exactly as it was, which is the entire refusal this file is
// built around and the one property no amount of reading the source proves as
// well as reading the code page back.
//
// The console is manufactured rather than hoped for, by openConsole in
// tty_windows_test.go: a CI job step's stdout is a pipe, a process holding one
// may have no console at all, and a process with no console fails
// GetConsoleOutputCP -- which is the one path that already needed no proof.
func TestConsoleUTF8ReadsTheOutputCodePageAndLeavesItAlone(t *testing.T) {
	openConsole(t)

	original := outputCP(t)
	// This is the only place in the tree that writes an output code page, and
	// it writes one solely to prove that consoleUTF8 does not. Restoring is
	// therefore not politeness but the same rule the production code follows:
	// a test that left a developer's window on 437 would have done exactly the
	// damage the function refuses to do.
	t.Cleanup(func() {
		if err := windows.SetConsoleOutputCP(original); err != nil {
			t.Errorf("restoring the console output code page to %d: %v", original, err)
		}
	})

	for _, tc := range []struct {
		name string
		cp   uint32
		want bool
	}{
		{"UTF-8", cpUTF8, true},
		// The OEM United States page: what a console nobody has run chcp on
		// comes up as on an en-US install, and the single most likely value to
		// meet in the wild. CP949 is the case the withdrawn write would have
		// trampled, and it is argued in the source rather than asserted here,
		// because a code page the runner image has no NLS file for fails
		// SetConsoleOutputCP and would redden CI over the test's own setup.
		{"CP437", 437, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := windows.SetConsoleOutputCP(tc.cp); err != nil {
				t.Fatalf("SetConsoleOutputCP(%d) on this test's own console: %v", tc.cp, err)
			}
			if got := consoleUTF8(); got != tc.want {
				t.Errorf("consoleUTF8() = %v on code page %d, want %v", got, tc.cp, tc.want)
			}
			if got := outputCP(t); got != tc.cp {
				t.Errorf("the console output code page is %d after consoleUTF8(), want %d: consoleUTF8 "+
					"wrote to a console it shares with every other process attached to it", got, tc.cp)
			}
		})
	}
}

func outputCP(t *testing.T) uint32 {
	t.Helper()
	cp, err := windows.GetConsoleOutputCP()
	if err != nil {
		t.Fatalf("GetConsoleOutputCP although this process has a console: %v", err)
	}
	return cp
}

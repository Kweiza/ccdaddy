//go:build windows

package daemon

import (
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// The half that cannot be written off Windows: neither syscall nor
// x/sys/windows compiles elsewhere, so this is where the literals in spawn.go
// are checked against a definition ccdad does not own.
func TestTheFlagLiteralsMatchTheWindowsDefinitions(t *testing.T) {
	if flagDetachedProcess != windows.DETACHED_PROCESS {
		t.Errorf("flagDetachedProcess = %#x, x/sys/windows says %#x", flagDetachedProcess, windows.DETACHED_PROCESS)
	}
	if flagCreateNewProcessGroup != windows.CREATE_NEW_PROCESS_GROUP {
		t.Errorf("flagCreateNewProcessGroup = %#x, x/sys/windows says %#x", flagCreateNewProcessGroup, windows.CREATE_NEW_PROCESS_GROUP)
	}
	if flagCreateNewProcessGroup != syscall.CREATE_NEW_PROCESS_GROUP {
		t.Errorf("flagCreateNewProcessGroup = %#x, syscall says %#x", flagCreateNewProcessGroup, syscall.CREATE_NEW_PROCESS_GROUP)
	}
}

// This runs only on Windows, so everywhere else it is compiled by
// `GOOS=windows go vet ./...` and no further. It pins the flag set against
// being emptied or widened, which is worth having because the alternative is
// nothing at all — but it asserts a struct, not an operating system. Whether
// DETACHED_PROCESS actually severs the console is what the post-release
// install smoke suite is for.
func TestDetachSetsExactlyTheTwoCreationFlags(t *testing.T) {
	cmd := exec.Command("cmd.exe")
	if err := detach(cmd); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if cmd.SysProcAttr == nil {
		t.Fatal("detach left SysProcAttr nil, so the child would inherit the console")
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Error("HideWindow is false")
	}
	want := uint32(flagDetachedProcess | flagCreateNewProcessGroup)
	if got := cmd.SysProcAttr.CreationFlags; got != want {
		t.Errorf("CreationFlags = %#x, want %#x — DETACHED_PROCESS is what keeps the child alive "+
			"when the console window closes, and the pair is deliberately exactly two", got, want)
	}
}
